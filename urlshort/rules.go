// Copyright (c) 2026 Aaron LI
//
// Mapping rules engine: target canonicalization, regexp matching, key
// templates, and hash-prefix extension.
//
// Rules are global and canonical: one target maps to one key regardless of
// which client asks. Rules are only consulted for new targets; existing links
// keep their keys (section 7.6).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// ErrForbidden marks an authorization failure (namespace violation). It is
// defined here so the rules engine, store generator, and API can share it.
var ErrForbidden = errors.New("forbidden")

// Rule is one compiled mapping rule.
type Rule struct {
	Name string

	re         *regexp.Regexp
	tmpl       *template.Template
	hash       string
	hashMinlen int
}

// Ruleset is the ordered list of rules.
type Ruleset struct {
	rules []*Rule
}

// NewRuleset compiles the configured rules in order.
func NewRuleset(cfgs []RuleConfig, abbrev map[string]string) (*Ruleset, error) {
	rs := &Ruleset{}
	for i := range cfgs {
		cfg := &cfgs[i]
		re, err := regexp.Compile(cfg.Match)
		if err != nil {
			return nil, fmt.Errorf("rule %q: compile match: %w", cfg.Name, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("rule %q: match must not match the empty string",
				cfg.Name)
		}
		tmpl, err := parseKeyTemplate(cfg.Name, cfg.Key, abbrev)
		if err != nil {
			return nil, fmt.Errorf("rule %q: parse key: %w", cfg.Name, err)
		}
		rs.rules = append(rs.rules, &Rule{
			Name:       cfg.Name,
			re:         re,
			tmpl:       tmpl,
			hash:       cfg.Hash,
			hashMinlen: cfg.HashMinlen,
		})
	}
	return rs, nil
}

// Match returns the first rule whose pattern matches the whole target, plus its
// named capture groups.
func (rs *Ruleset) Match(target string) (*Rule, map[string]string, bool) {
	for _, r := range rs.rules {
		m := r.re.FindStringSubmatchIndex(target)
		if m == nil || m[0] != 0 || m[1] != len(target) {
			continue
		}
		vars := make(map[string]string)
		for i, name := range r.re.SubexpNames() {
			if i == 0 || name == "" {
				continue
			}
			if m[2*i] >= 0 {
				vars[name] = target[m[2*i]:m[2*i+1]]
			}
		}
		return r, vars, true
	}
	return nil, nil, false
}

// Render executes the key template with vars.
func (r *Rule) Render(vars map[string]string) (string, error) {
	var b strings.Builder
	if err := r.tmpl.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("rule %q: render key: %w", r.Name, err)
	}
	key := b.String()
	if err := ValidateKey(key); err != nil {
		return "", fmt.Errorf("rule %q: %w", r.Name, err)
	}
	return key, nil
}

// GenerateKey renders a key for the matched target and, for hash rules,
// extends the hash prefix until the key is free. isFree is evaluated inside the
// caller's write transaction so the check and insert are atomic (C9).
func (r *Rule) GenerateKey(vars map[string]string, isFree func(string) (bool, error)) (string, error) {
	if r.hash == "" {
		key, err := r.Render(vars)
		if err != nil {
			return "", err
		}
		free, err := isFree(key)
		if err != nil {
			return "", err
		}
		if !free {
			return "", fmt.Errorf("%w: rule %q: key %q already exists for another target",
				ErrConflict, r.Name, key)
		}
		return key, nil
	}

	hashVal, ok := vars[r.hash]
	if !ok || hashVal == "" {
		return "", fmt.Errorf("rule %q: hash group %q is empty", r.Name, r.hash)
	}
	v := make(map[string]string, len(vars))
	for k, val := range vars {
		v[k] = val
	}
	minlen := r.hashMinlen
	if minlen > len(hashVal) {
		minlen = len(hashVal)
	}
	for l := minlen; l <= len(hashVal); l++ {
		v[r.hash] = hashVal[:l]
		key, err := r.Render(v)
		if err != nil {
			return "", err
		}
		free, err := isFree(key)
		if err != nil {
			return "", err
		}
		if free {
			return key, nil
		}
	}
	return "", fmt.Errorf("%w: rule %q: no unique hash prefix for %q (all lengths <= %d collide)",
		ErrConflict, r.Name, hashVal, len(hashVal))
}

// ValidateKey checks a rule-generated or explicit key against section 7.5.
func ValidateKey(key string) error {
	const maxKeyLen = 256

	if key == "" {
		return errors.New("key is empty")
	}
	if key[0] != '/' {
		return errors.New("key must start with /")
	}
	if len(key) > maxKeyLen {
		return fmt.Errorf("key longer than %d bytes", maxKeyLen)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '/':
		case c == '.':
		case c == '_':
		case c == '~':
		case c == '-':
		default:
			return fmt.Errorf("key contains invalid character %q", c)
		}
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return errors.New("key must not contain a .. segment")
		}
		if len(seg) > 0 && (seg[0] == '.' || seg[0] == '~') {
			return fmt.Errorf("key segment %q must not start with . or ~", seg)
		}
	}
	for _, k := range reservedExactKeys {
		if key == k {
			return fmt.Errorf("key %q is reserved", key)
		}
	}
	return nil
}

// randomID returns n base62 characters from crypto/rand.
func randomID(n int) (string, error) {
	const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	buf := make([]byte, n)
	for i := 0; i < n; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		// Reject the extra values that would bias the modulo 62.
		if b[0] >= 62*4 {
			continue
		}
		buf[i] = base62Alphabet[int(b[0])%62]
		i++
	}
	return string(buf), nil
}

// GenerateRandomKey returns a random key in the given namespace
// (<namespace>~<12 base62 chars>). Random keys intentionally bypass the
// rule-key validation because they start a segment with "~".
func GenerateRandomKey(namespace string, isFree func(string) (bool, error)) (string, error) {
	if !strings.HasSuffix(namespace, "/") {
		return "", fmt.Errorf("namespace %q must end with /", namespace)
	}
	for attempt := 0; attempt < 10; attempt++ {
		id, err := randomID(12)
		if err != nil {
			return "", err
		}
		key := namespace + "~" + id
		free, err := isFree(key)
		if err != nil {
			return "", err
		}
		if free {
			return key, nil
		}
	}
	return "", errors.New("could not generate a unique random key after 10 attempts")
}

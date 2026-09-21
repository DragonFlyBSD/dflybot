// Copyright (c) 2026 Aaron LI
//
// Rules engine tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"errors"
	"strings"
	"testing"
)

func sampleRuleset(t *testing.T) *Ruleset {
	t.Helper()
	cfg := []RuleConfig{
		{
			Name:  "github-pr",
			Match: `^https://github\.com/(?P<org>[^/]+)/(?P<repo>[^/]+)/pull/(?P<num>[0-9]+)$`,
			Key:   "/gh/{{ abbrev .org }}/p/{{ .num }}",
		},
		{
			Name:       "gitweb-commit",
			Match:      `^https://gitweb\.dragonflybsd\.org/(?P<repo>[^/]+?)(?:\.git)?/(?:commit|commitdiff)/(?P<sha>[0-9a-f]{40})$`,
			Key:        "/g/{{ abbrev .repo }}/{{ .sha }}",
			Hash:       "sha",
			HashMinlen: 8,
		},
	}
	rs, err := NewRuleset(cfg, map[string]string{"DragonFlyBSD": "dfbsd", "dragonfly": "d"})
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestRulesetMatch(t *testing.T) {
	rs := sampleRuleset(t)

	target := "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56"
	r, vars, ok := rs.Match(target)
	if !ok || r.Name != "github-pr" {
		t.Fatalf("match failed: %v %v", r, vars)
	}
	key, err := r.GenerateKey(vars, func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if key != "/gh/dfbsd/p/56" {
		t.Fatalf("key = %q, want /gh/dfbsd/p/56", key)
	}

	// Partial match must not count.
	if _, _, ok := rs.Match("https://github.com/a/b/pull/56/extra"); ok {
		t.Error("partial match should fail")
	}
	if _, _, ok := rs.Match("https://example.com/"); ok {
		t.Error("unrelated target should not match")
	}
}

func TestRuleHashExtension(t *testing.T) {
	rs := sampleRuleset(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	target := "https://gitweb.dragonflybsd.org/dragonfly.git/commitdiff/" + sha
	r, vars, ok := rs.Match(target)
	if !ok {
		t.Fatal("no match")
	}

	occupied := map[string]bool{"/g/d/01234567": true}
	key, err := r.GenerateKey(vars, func(k string) (bool, error) { return !occupied[k], nil })
	if err != nil {
		t.Fatal(err)
	}
	if key != "/g/d/012345678" {
		t.Fatalf("key = %q, want /g/d/012345678", key)
	}

	// All lengths collide -> conflict.
	key, err = r.GenerateKey(vars, func(k string) (bool, error) { return false, nil })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got key=%q err=%v", key, err)
	}
}

func TestRuleNonHashCollision(t *testing.T) {
	rs, err := NewRuleset([]RuleConfig{{
		Name:  "fixed",
		Match: `^https://x/(?P<a>[^/]+)$`,
		Key:   "/fixed",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, vars, ok := rs.Match("https://x/abc")
	if !ok {
		t.Fatal("no match")
	}
	if _, err := r.GenerateKey(vars, func(string) (bool, error) { return true, nil }); err != nil {
		t.Fatalf("free key: %v", err)
	}
	if _, err := r.GenerateKey(vars, func(string) (bool, error) { return false, nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestRuleGitwebCommitPaths(t *testing.T) {
	rs := sampleRuleset(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, path := range []string{"commit", "commitdiff"} {
		target := "https://gitweb.dragonflybsd.org/dragonfly.git/" + path + "/" + sha
		r, vars, ok := rs.Match(target)
		if !ok {
			t.Fatalf("%s: no match", path)
		}
		key, err := r.GenerateKey(vars, func(string) (bool, error) { return true, nil })
		if err != nil {
			t.Fatal(err)
		}
		if key != "/g/d/01234567" {
			t.Fatalf("%s: key = %q", path, key)
		}
	}
}

func TestValidateKey(t *testing.T) {
	valid := []string{"/a", "/g/d/728aaaa", "/a-b_c.d", "/gh/dfbsd/p/56"}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("%q: unexpected error %v", k, err)
		}
	}
	invalid := []string{"", "/", "a", "/a b", "/a?b", "/a#b", "/a%b", "/../x", "/a/./b", "/a/~x", "/a/..", "/robots.txt", "/favicon.ico", strings.Repeat("/a", 200)}
	for _, k := range invalid {
		if err := ValidateKey(k); err == nil {
			t.Errorf("%q: expected error", k)
		}
	}
}

func TestGenerateRandomKey(t *testing.T) {
	key, err := GenerateRandomKey("/g/", func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "/g/~") {
		t.Fatalf("random key %q has wrong shape", key)
	}
	if len(key) != len("/g/~")+12 {
		t.Fatalf("random key %q has wrong length", key)
	}
	for _, c := range key {
		if !strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz/~", c) {
			t.Fatalf("random key %q has invalid character %q", key, c)
		}
	}
	// A namespace without a trailing slash is rejected.
	if _, err := GenerateRandomKey("/g", func(string) (bool, error) { return true, nil }); err == nil {
		t.Fatal("expected namespace error")
	}
}

// Copyright (c) 2026 Aaron LI
//
// Configuration loading and validation.
//
// The sample file lives in urlshort.toml. Validation follows the matrix in
// the design document (section 5.1); startup must fail with clear messages
// when any condition is violated.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
)

// ---------------------------------------------------------------------------
// Types

// Config is the full program configuration.
type Config struct {
	LogLevel string `toml:"log_level"`
	DataDir  string `toml:"data_dir"`

	Server        ServerConfig      `toml:"server"`
	ACME          ACMEConfig        `toml:"acme"`
	Access        AccessConfig      `toml:"access"`
	AccessLog     AccessLogConfig   `toml:"access_log"`
	Backup        BackupConfig      `toml:"backup"`
	Abbreviations map[string]string `toml:"abbreviations"`
	Rules         []RuleConfig      `toml:"rules"`
	Clients       []ClientConfig    `toml:"clients"`

	// Derived values (not decoded from TOML).
	publicURL       *url.URL       `toml:"-"`
	trustedPrefixes []netip.Prefix `toml:"-"`
	Warnings        []string       `toml:"-"`
}

// ServerConfig holds the HTTP/TLS listener configuration.
type ServerConfig struct {
	ListenAddresses []string `toml:"listen_addresses"`
	HTTPPort        int      `toml:"http_port"`
	HTTPSPort       int      `toml:"https_port"`
	PublicURL       string   `toml:"public_url"`
	ExtraHosts      []string `toml:"extra_hosts"`
	CertFile        string   `toml:"cert_file"`
	KeyFile         string   `toml:"key_file"`

	ShutdownTimeout int `toml:"shutdown_timeout"`
	ReadTimeout     int `toml:"read_timeout"`
	WriteTimeout    int `toml:"write_timeout"`
	IdleTimeout     int `toml:"idle_timeout"`
	MaxHeaderBytes  int `toml:"max_header_bytes"`

	// HTTP2Enabled enables HTTP/2 over TLS via ALPN.
	HTTP2Enabled bool `toml:"http2_enabled"`

	// TrustedProxies lists the IPs/CIDRs whose X-Forwarded-For and X-Real-IP
	// headers are trusted for client IP extraction. Defaults to localhost.
	TrustedProxies []string `toml:"trusted_proxies"`

	// HSTS configures the Strict-Transport-Security response header. It can
	// only be enabled when https_port > 0.
	HSTS HSTSConfig `toml:"hsts"`
}

// HSTSConfig configures the Strict-Transport-Security header.
type HSTSConfig struct {
	Enabled           bool `toml:"enabled"`
	MaxAge            int  `toml:"max_age"`
	IncludeSubDomains bool `toml:"include_subdomains"`
	Preload           bool `toml:"preload"`
}

// HeaderValue renders the Strict-Transport-Security header value.
func (h HSTSConfig) HeaderValue() string {
	v := fmt.Sprintf("max-age=%d", h.MaxAge)
	if h.IncludeSubDomains {
		v += "; includeSubDomains"
	}
	if h.Preload {
		v += "; preload"
	}
	return v
}

// ACMEConfig holds the automatic certificate management configuration.
type ACMEConfig struct {
	Enabled         bool   `toml:"enabled"`
	DirectoryURL    string `toml:"directory_url"`
	AccountFile     string `toml:"account_file"`
	Email           string `toml:"email"`
	AcceptTOS       bool   `toml:"accept_tos"`
	CacheDir        string `toml:"cache_dir"`
	RenewBeforeDays int    `toml:"renew_before_days"`
	HTTP01Fallback  bool   `toml:"http01_fallback"`
	IssueTimeout    int    `toml:"issue_timeout"`

	// AccountKey is the parsed account key from AccountFile, if any.
	AccountKey crypto.Signer `toml:"-"`
}

// AccessConfig holds the rate limiting configuration.
type AccessConfig struct {
	RedirectRate  float64 `toml:"redirect_rate"`
	RedirectBurst int     `toml:"redirect_burst"`
	APIRate       float64 `toml:"api_rate"`
	APIBurst      int     `toml:"api_burst"`
}

// AccessLogConfig holds the access log configuration.
type AccessLogConfig struct {
	RetentionDays int `toml:"retention_days"`
	FlushInterval int `toml:"flush_interval"`
}

// BackupConfig holds the compacted backup configuration.
type BackupConfig struct {
	Enabled           bool   `toml:"enabled"`
	Dir               string `toml:"dir"`
	HourUTC           int    `toml:"hour_utc"`
	RunOnStart        bool   `toml:"run_on_start"`
	RetentionDays     int    `toml:"retention_days"`
	RetentionCount    int    `toml:"retention_count"`
	CompactTxMaxBytes int64  `toml:"compact_tx_max_bytes"`
}

// RuleConfig is one ordered mapping rule.
type RuleConfig struct {
	Name       string `toml:"name"`
	Match      string `toml:"match"`
	Key        string `toml:"key"`
	Hash       string `toml:"hash"`
	HashMinlen int    `toml:"hash_minlen"`
}

// ClientConfig is one API client.
type ClientConfig struct {
	Enabled    bool     `toml:"enabled"`
	Name       string   `toml:"name"`
	Admin      bool     `toml:"admin"`
	Namespaces []string `toml:"namespaces"`
	Tokens     []string `toml:"tokens"`
}

// ---------------------------------------------------------------------------
// Defaults and loading

const (
	defaultDirectoryURL = "https://acme-v02.api.letsencrypt.org/directory"
	tokenMinLen         = 32
)

// DefaultConfig returns a configuration with the documented defaults. Decoding
// a TOML file on top of this preserves defaults for keys absent from the file.
func DefaultConfig() *Config {
	return &Config{
		LogLevel: "info",
		DataDir:  "./data/",
		Server: ServerConfig{
			ListenAddresses: []string{"0.0.0.0", "::"},
			HTTPPort:        80,
			HTTPSPort:       443,
			ShutdownTimeout: 10,
			ReadTimeout:     30,
			WriteTimeout:    30,
			IdleTimeout:     60,
			MaxHeaderBytes:  8192,
			HTTP2Enabled:    true,
			TrustedProxies:  []string{"127.0.0.0/8", "::1/128"},
			HSTS: HSTSConfig{
				Enabled: false,
				MaxAge:  31536000,
			},
		},
		ACME: ACMEConfig{
			Enabled:         true,
			DirectoryURL:    defaultDirectoryURL,
			RenewBeforeDays: 30,
			IssueTimeout:    60,
		},
		Access: AccessConfig{
			RedirectRate:  20,
			RedirectBurst: 40,
			APIRate:       5,
			APIBurst:      10,
		},
		AccessLog: AccessLogConfig{
			RetentionDays: 30,
			FlushInterval: 5,
		},
		Backup: BackupConfig{
			Enabled:           true,
			HourUTC:           3,
			RunOnStart:        true,
			RetentionDays:     30,
			RetentionCount:    3,
			CompactTxMaxBytes: 1048576,
		},
	}
}

// LoadConfig reads, defaults, and validates a TOML configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := DefaultConfig()
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("config %q: unknown keys: %s", path, strings.Join(keys, ", "))
	}

	if err := cfg.applyDerivedDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyDerivedDefaults fills in path defaults that depend on data_dir.
func (c *Config) applyDerivedDefaults() error {
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}
	if !strings.HasSuffix(c.DataDir, "/") {
		return fmt.Errorf("data_dir %q must end with a slash", c.DataDir)
	}
	if c.ACME.CacheDir == "" {
		c.ACME.CacheDir = filepath.Join(c.DataDir, "acme") + "/"
	}
	if c.Backup.Dir == "" {
		c.Backup.Dir = filepath.Join(c.DataDir, "backup") + "/"
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation

// ConfigError accumulates one or more validation failures.
type ConfigError struct {
	Problems []string
}

func (e *ConfigError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

type validator struct {
	problems []string
	warnings []string
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) warnf(format string, args ...any) {
	v.warnings = append(v.warnings, fmt.Sprintf(format, args...))
}

// Validate checks the whole configuration. It also performs the filesystem
// checks required at startup (data_dir and backup dir creatable/writable).
func (c *Config) Validate() error {
	v := &validator{}

	c.validateLogLevel(v)
	c.validateServer(v)
	c.validateACME(v)
	c.validateAccess(v)
	c.validateAccessLog(v)
	c.validateBackup(v)
	c.validateRules(v)
	c.validateClients(v)

	c.Warnings = v.warnings
	if len(v.problems) > 0 {
		return &ConfigError{Problems: v.problems}
	}
	return nil
}

func (c *Config) validateLogLevel(v *validator) {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		v.addf("log_level %q must be one of debug, info, warn, error", c.LogLevel)
	}
}

func (c *Config) validateServer(v *validator) {
	s := &c.Server
	if s.HTTPPort < 0 || s.HTTPPort > 65535 {
		v.addf("server.http_port %d out of range [0,65535]", s.HTTPPort)
	}
	if s.HTTPSPort < 0 || s.HTTPSPort > 65535 {
		v.addf("server.https_port %d out of range [0,65535]", s.HTTPSPort)
	}
	if s.HTTPPort == 0 && s.HTTPSPort == 0 {
		v.addf("at least one of server.http_port and server.https_port must be > 0")
	}
	if len(s.ListenAddresses) == 0 {
		v.addf("server.listen_addresses must not be empty")
	}
	for _, a := range s.ListenAddresses {
		if strings.TrimSpace(a) == "" {
			v.addf("server.listen_addresses contains an empty address")
		}
	}

	if s.ShutdownTimeout < 0 || s.ReadTimeout < 0 || s.WriteTimeout < 0 || s.IdleTimeout < 0 {
		v.addf("server timeouts must be non-negative")
	}
	if s.HTTPSPort > 0 && (s.ReadTimeout <= 0 || s.WriteTimeout <= 0) {
		v.addf("server.read_timeout and server.write_timeout must be > 0 when https_port > 0")
	}
	if s.MaxHeaderBytes < 4096 {
		v.addf("server.max_header_bytes %d must be >= 4096", s.MaxHeaderBytes)
	}
	c.validateTrustedProxies(v)
	c.validateHSTS(v)

	u, err := url.Parse(s.PublicURL)
	if err != nil {
		v.addf("server.public_url %q does not parse: %v", s.PublicURL, err)
		return
	}
	c.publicURL = u

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		v.addf("server.public_url scheme %q must be http or https", u.Scheme)
	}
	if u.User != nil {
		v.addf("server.public_url must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		v.addf("server.public_url must not contain a query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		v.addf("server.public_url must not contain a path, got %q", u.Path)
	}
	if strings.HasSuffix(strings.TrimSpace(s.PublicURL), "/") {
		v.addf("server.public_url must not end with a slash")
	}
	if u.Hostname() == "" {
		v.addf("server.public_url must contain a host")
	}
	if !isASCII(u.Hostname()) {
		v.addf("server.public_url host %q must be ASCII/punycode", u.Hostname())
	}
	if scheme == "http" {
		if c.ACME.Enabled {
			v.addf("server.public_url uses http but acme.enabled is true; use https or disable ACME")
		}
		if s.HTTPSPort != 0 {
			v.addf("server.public_url uses http but https_port is %d; set https_port = 0", s.HTTPSPort)
		}
	}
	if scheme == "https" && c.ACME.Enabled && s.HTTPSPort != 443 {
		v.addf("acme.enabled requires server.https_port == 443, got %d", s.HTTPSPort)
	}
	if scheme == "https" && s.HTTPSPort == 0 {
		v.addf("server.public_url uses https but server.https_port is 0")
	}
	for _, h := range s.ExtraHosts {
		if h == "" {
			v.addf("server.extra_hosts contains an empty host")
			continue
		}
		if strings.ContainsAny(h, "/:") {
			v.addf("server.extra_hosts entry %q must be a bare hostname", h)
		}
		if !isASCII(h) {
			v.addf("server.extra_hosts entry %q must be ASCII/punycode", h)
		}
	}
}

func (c *Config) validateTrustedProxies(v *validator) {
	prefixes, err := parseTrustedProxies(c.Server.TrustedProxies)
	if err != nil {
		v.addf("server.trusted_proxies: %v", err)
		return
	}
	c.trustedPrefixes = prefixes
	for _, p := range prefixes {
		if p.Bits() == 0 {
			v.warnf("server.trusted_proxies includes %s, which trusts every peer", p)
		}
	}
}

func (c *Config) validateHSTS(v *validator) {
	h := c.Server.HSTS
	if !h.Enabled {
		return
	}
	if c.Server.HTTPSPort == 0 {
		v.addf("server.hsts.enabled requires server.https_port > 0")
	}
	if h.MaxAge <= 0 {
		v.addf("server.hsts.max_age must be > 0 when enabled")
	}
	if h.Preload && !h.IncludeSubDomains {
		v.addf("server.hsts.preload requires include_subdomains = true")
	}
	if h.Preload && h.MaxAge < 31536000 {
		v.addf("server.hsts.preload requires max_age >= 31536000")
	}
}

func (c *Config) validateACME(v *validator) {
	a := &c.ACME
	if a.Enabled {
		if !a.AcceptTOS {
			v.addf("acme.accept_tos must be true when acme.enabled is true")
		}
		if strings.TrimSpace(a.DirectoryURL) == "" {
			v.addf("acme.directory_url must not be empty when acme.enabled is true")
		}
		if c.Server.CertFile != "" || c.Server.KeyFile != "" {
			v.addf("server.cert_file/key_file must be empty when acme.enabled is true")
		}
		if a.HTTP01Fallback && c.Server.HTTPPort != 80 {
			v.addf("acme.http01_fallback requires server.http_port == 80, got %d", c.Server.HTTPPort)
		}
	} else if c.Server.HTTPSPort > 0 {
		if c.Server.CertFile == "" || c.Server.KeyFile == "" {
			v.addf("acme.enabled is false and https_port > 0: server.cert_file and server.key_file are required")
		} else {
			if _, err := os.Stat(c.Server.CertFile); err != nil {
				v.addf("server.cert_file %q not readable: %v", c.Server.CertFile, err)
			}
			if _, err := os.Stat(c.Server.KeyFile); err != nil {
				v.addf("server.key_file %q not readable: %v", c.Server.KeyFile, err)
			}
		}
	}
	if a.RenewBeforeDays <= 0 || a.RenewBeforeDays >= 90 {
		v.addf("acme.renew_before_days %d must be > 0 and < 90", a.RenewBeforeDays)
	}
	if a.IssueTimeout <= 0 {
		v.addf("acme.issue_timeout must be > 0")
	}
	if a.AccountFile != "" {
		signer, err := loadAccountSigner(a.AccountFile)
		if err != nil {
			v.addf("acme.account_file %q: %v", a.AccountFile, err)
		} else {
			a.AccountKey = signer
		}
	}
}

func (c *Config) validateAccess(v *validator) {
	a := &c.Access
	if a.RedirectRate <= 0 {
		v.addf("access.redirect_rate must be > 0")
	}
	if a.RedirectBurst < 0 {
		v.addf("access.redirect_burst must be >= 0")
	}
	if a.APIRate <= 0 {
		v.addf("access.api_rate must be > 0")
	}
	if a.APIBurst < 0 {
		v.addf("access.api_burst must be >= 0")
	}
}

func (c *Config) validateAccessLog(v *validator) {
	if c.AccessLog.RetentionDays < 0 {
		v.addf("access_log.retention_days must be >= 0")
	}
	if c.AccessLog.FlushInterval <= 0 {
		v.addf("access_log.flush_interval must be > 0")
	}
}

func (c *Config) validateBackup(v *validator) {
	b := &c.Backup
	if b.HourUTC < 0 || b.HourUTC > 23 {
		v.addf("backup.hour_utc %d must be in [0,23]", b.HourUTC)
	}
	if b.RetentionDays < 0 {
		v.addf("backup.retention_days must be >= 0")
	}
	if b.RetentionCount < 0 {
		v.addf("backup.retention_count must be >= 0")
	}
	if b.CompactTxMaxBytes <= 0 {
		v.addf("backup.compact_tx_max_bytes must be > 0")
	}
	if b.Dir != "" && !strings.HasSuffix(b.Dir, "/") {
		v.addf("backup.dir %q must end with a slash", b.Dir)
	}
}

func (c *Config) validateRules(v *validator) {
	seen := make(map[string]bool)
	for i := range c.Rules {
		r := &c.Rules[i]
		ctx := fmt.Sprintf("rules[%d]", i)
		if r.Name != "" {
			ctx = fmt.Sprintf("rule %q", r.Name)
		}
		if r.Name == "" {
			v.addf("%s: name must not be empty", ctx)
		} else if seen[r.Name] {
			v.addf("duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true

		if r.Match == "" {
			v.addf("%s: match must not be empty", ctx)
			continue
		}
		re, err := regexp.Compile(r.Match)
		if err != nil {
			v.addf("%s: match does not compile: %v", ctx, err)
			continue
		}
		if re.MatchString("") {
			v.addf("%s: match must not match the empty string", ctx)
		}
		if !strings.HasPrefix(r.Key, "/") {
			v.addf("%s: key must start with /", ctx)
		}
		if _, err := parseKeyTemplate(r.Name, r.Key, c.Abbreviations); err != nil {
			v.addf("%s: key template: %v", ctx, err)
		}
		if r.Hash != "" {
			if !slices.Contains(re.SubexpNames(), r.Hash) {
				v.addf("%s: hash %q does not name a capture group", ctx, r.Hash)
			}
			if r.HashMinlen < 4 {
				v.addf("%s: hash_minlen %d must be >= 4", ctx, r.HashMinlen)
			}
		} else if r.HashMinlen != 0 {
			v.addf("%s: hash_minlen set without hash", ctx)
		}
	}
}

func (c *Config) validateClients(v *validator) {
	type clientTokens struct {
		name   string
		tokens map[string]bool
	}
	var all []clientTokens
	seenNames := make(map[string]bool)

	for i := range c.Clients {
		cl := &c.Clients[i]
		ctx := fmt.Sprintf("clients[%d]", i)
		if cl.Name != "" {
			ctx = fmt.Sprintf("client %q", cl.Name)
		}
		if cl.Name == "" {
			v.addf("%s: name must not be empty", ctx)
		} else if seenNames[cl.Name] {
			v.addf("duplicate client name %q", cl.Name)
		}
		seenNames[cl.Name] = true

		tokens := make(map[string]bool)
		for _, tok := range cl.Tokens {
			if len(tok) < tokenMinLen {
				v.addf("%s: plaintext token shorter than %d characters", ctx, tokenMinLen)
			}
			tokens[tok] = true
		}
		if cl.Enabled && len(tokens) == 0 {
			v.addf("%s: enabled client must have at least one token", ctx)
		}
		if cl.Enabled && !cl.Admin && len(cl.Namespaces) == 0 {
			v.addf("%s: non-admin client must have at least one namespace", ctx)
		}
		for _, ns := range cl.Namespaces {
			if !validNamespace(ns) {
				v.addf("%s: invalid namespace %q", ctx, ns)
			}
		}
		all = append(all, clientTokens{name: cl.Name, tokens: tokens})
	}

	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			for d := range all[i].tokens {
				if all[j].tokens[d] {
					v.addf("token shared by clients %q and %q",
						all[i].name, all[j].name)
				}
			}
		}
	}

	c.warnNamespaceOverlap(v)
}

func (c *Config) warnNamespaceOverlap(v *validator) {
	for i := 0; i < len(c.Clients); i++ {
		for j := i + 1; j < len(c.Clients); j++ {
			for _, a := range c.Clients[i].Namespaces {
				for _, b := range c.Clients[j].Namespaces {
					if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
						v.warnf("namespaces %q (client %q) and %q (client %q) overlap",
							a, c.Clients[i].Name, b, c.Clients[j].Name)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers

// parseTrustedProxies parses a list of IPs and CIDRs into prefixes. A bare
// IP becomes a /32 (IPv4) or /128 (IPv6) prefix.
func parseTrustedProxies(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("invalid entry %q: not an IP or CIDR", item)
		}
		a = a.Unmap()
		bits := 128
		if a.Is4() {
			bits = 32
		}
		out = append(out, netip.PrefixFrom(a, bits).Masked())
	}
	return out, nil
}

// TrustedProxyPrefixes returns the trusted proxy prefixes parsed during
// validation.
func (c *Config) TrustedProxyPrefixes() []netip.Prefix { return c.trustedPrefixes }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

var (
	namespaceRe       = regexp.MustCompile(`^(?:/[A-Za-z0-9._~-]+)+/$`)
	reservedPrefixes  = []string{"/.api/", "/.well-known/"}
	reservedExactKeys = []string{"/robots.txt", "/favicon.ico"}
)

// validNamespace reports whether ns is a well-formed, non-reserved namespace
// prefix such as /g/.
func validNamespace(ns string) bool {
	if !namespaceRe.MatchString(ns) {
		return false
	}
	for _, seg := range strings.Split(strings.Trim(ns, "/"), "/") {
		if seg == "." || seg == ".." {
			return false
		}
		if strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "~") {
			return false
		}
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(ns, p) {
			return false
		}
	}
	for _, k := range reservedExactKeys {
		if ns == k+"/" || ns == k {
			return false
		}
	}
	return true
}

// loadAccountSigner reads a PEM private key and returns it as a crypto.Signer.
// EC and RSA keys are accepted; EC P-256 is preferred by autocert.
func loadAccountSigner(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unsupported private key: %w", err)
	}
	switch k := key.(type) {
	case *ecdsa.PrivateKey, *rsa.PrivateKey:
		return k.(crypto.Signer), nil
	default:
		return nil, fmt.Errorf("unsupported private key type %T", key)
	}
}

// keyTemplateFuncs returns the restricted template functions available to key
// templates. Unknown abbreviation names are lowercased and passed through.
func keyTemplateFuncs(abbrev map[string]string) template.FuncMap {
	lookup := func(s string) string {
		if v, ok := abbrev[s]; ok {
			return v
		}
		return strings.ToLower(s)
	}
	return template.FuncMap{
		"abbrev": lookup,
		"lower":  strings.ToLower,
	}
}

// parseKeyTemplate parses a rule key template with the restricted FuncMap and
// rejects {{template}}/{{block}}/{{define}} constructs.
func parseKeyTemplate(name, text string, abbrev map[string]string) (*template.Template, error) {
	if strings.Contains(text, "{{define") || strings.Contains(text, "{{template") || strings.Contains(text, "{{block") {
		return nil, errors.New("template/define/block actions are not allowed")
	}
	t, err := template.New(name).Funcs(keyTemplateFuncs(abbrev)).Parse(text)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Accessors

// PublicURL returns the parsed public URL.
func (c *Config) PublicURL() *url.URL { return c.publicURL }

// PublicHost returns the hostname (no port) of public_url.
func (c *Config) PublicHost() string {
	if c.publicURL == nil {
		return ""
	}
	return c.publicURL.Hostname()
}

// AllowedHosts returns the lowercase hostnames accepted in the Host header.
func (c *Config) AllowedHosts() []string {
	hosts := []string{}
	if h := c.PublicHost(); h != "" {
		hosts = append(hosts, strings.ToLower(h))
	}
	for _, h := range c.Server.ExtraHosts {
		hosts = append(hosts, strings.ToLower(h))
	}
	return hosts
}

// LogsDir returns the directory holding the daily access log files.
func (c *Config) LogsDir() string {
	return filepath.Join(c.DataDir, "logs") + "/"
}

// EnsureDirs creates the filesystem directories the program needs and verifies
// they are writable. It is called at startup after Validate.
func (c *Config) EnsureDirs() error {
	dirs := []struct {
		name string
		path string
	}{
		{"data_dir", c.DataDir},
		{"access_log dir", c.LogsDir()},
	}
	if c.Backup.Enabled {
		dirs = append(dirs, struct {
			name string
			path string
		}{"backup.dir", c.Backup.Dir})
	}
	if c.ACME.Enabled {
		dirs = append(dirs, struct {
			name string
			path string
		}{"acme.cache_dir", c.ACME.CacheDir})
	}
	for _, d := range dirs {
		if err := ensureDir(d.path); err != nil {
			return fmt.Errorf("%s %q: %w", d.name, d.path, err)
		}
	}
	return nil
}

// ensureDir creates path (mode 0700) if needed and verifies it is writable.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(path, ".writecheck-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if cerr := f.Close(); cerr != nil {
		os.Remove(name)
		return cerr
	}
	return os.Remove(name)
}

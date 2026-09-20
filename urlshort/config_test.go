// Copyright (c) 2026 Aaron LI
//
// Configuration validation tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseConfig() *Config {
	c := DefaultConfig()
	c.Server.PublicURL = "https://example.com"
	c.ACME.AcceptTOS = true
	c.Clients = []ClientConfig{{
		Enabled:    true,
		Name:       "admin",
		Admin:      true,
		Tokens:     []string{strings.Repeat("a", 32)},
		Namespaces: nil,
	}}
	return c
}

func TestValidConfig(t *testing.T) {
	c := baseConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if got := c.PublicHost(); got != "example.com" {
		t.Fatalf("PublicHost = %q, want example.com", got)
	}
}

func TestDerivedDefaults(t *testing.T) {
	c := baseConfig()
	c.DataDir = "/var/lib/urlshort"
	if err := c.applyDerivedDefaults(); err != nil {
		t.Fatalf("applyDerivedDefaults: %v", err)
	}
	if c.ACME.CacheDir != "/var/lib/urlshort/acme" {
		t.Errorf("cache dir = %q", c.ACME.CacheDir)
	}
	if c.Backup.Dir != "/var/lib/urlshort/backup" {
		t.Errorf("backup dir = %q", c.Backup.Dir)
	}
	if c.LogsDir() != "/var/lib/urlshort/logs" {
		t.Errorf("logs dir = %q", c.LogsDir())
	}
}

func TestDerivedDefaultsRejectsDataDir(t *testing.T) {
	c := baseConfig()
	c.DataDir = ""
	if err := c.applyDerivedDefaults(); err == nil {
		t.Errorf("data_dir %q: expected error", c.DataDir)
	}
}

func TestValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"log level", func(c *Config) { c.LogLevel = "trace" }, "log_level"},
		{"ports zero", func(c *Config) { c.Server.HTTPPort = 0; c.Server.HTTPSPort = 0 }, "at least one"},
		{"public url missing", func(c *Config) { c.Server.PublicURL = "" }, "public_url"},
		{"public url scheme", func(c *Config) { c.Server.PublicURL = "ftp://example.com" }, "scheme"},
		{"public url path", func(c *Config) { c.Server.PublicURL = "https://example.com/x" }, "path"},
		{"public url userinfo", func(c *Config) { c.Server.PublicURL = "https://u@example.com" }, "userinfo"},
		{"public url query", func(c *Config) { c.Server.PublicURL = "https://example.com?x=1" }, "query"},
		{"http scheme with acme", func(c *Config) {
			c.Server.PublicURL = "http://example.com"
			c.Server.HTTPSPort = 0
		}, "http"},
		{"https without https port", func(c *Config) {
			c.ACME.Enabled = false
			c.Server.HTTPSPort = 0
		}, "https_port is 0"},
		{"acme https port", func(c *Config) { c.Server.HTTPSPort = 8443 }, "443"},
		{"acme accept tos", func(c *Config) { c.ACME.AcceptTOS = false }, "accept_tos"},
		{"acme with cert", func(c *Config) { c.Server.CertFile = "/x" }, "must be empty"},
		{"http01 fallback port", func(c *Config) {
			c.ACME.HTTP01Fallback = true
			c.Server.HTTPPort = 8080
		}, "http01_fallback"},
		{"renew days", func(c *Config) { c.ACME.RenewBeforeDays = 0 }, "renew_before_days"},
		{"issue timeout", func(c *Config) { c.ACME.IssueTimeout = 0 }, "issue_timeout"},
		{"read timeout", func(c *Config) { c.Server.ReadTimeout = 0 }, "read_timeout"},
		{"max header", func(c *Config) { c.Server.MaxHeaderBytes = 0 }, "max_header_bytes"},
		{"backup hour", func(c *Config) { c.Backup.HourUTC = 24 }, "hour_utc"},
		{"backup retention", func(c *Config) { c.Backup.RetentionDays = -1 }, "retention_days"},
		{"backup tx", func(c *Config) { c.Backup.CompactTxMaxBytes = 0 }, "compact_tx_max_bytes"},
		{"rule duplicate", func(c *Config) {
			c.Rules = []RuleConfig{
				{Name: "r", Match: `^https://a\.example/(?P<x>[^/]+)$`, Key: "/a/{{ .x }}"},
				{Name: "r", Match: `^https://b\.example/(?P<x>[^/]+)$`, Key: "/b/{{ .x }}"},
			}
		}, "duplicate rule"},
		{"rule bad regex", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `(`, Key: "/a/"}}
		}, "does not compile"},
		{"rule empty match", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^(?:x)?$`, Key: "/a/{{\"\"}}"}}
		}, "empty string"},
		{"rule key prefix", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^https://a$`, Key: "a"}}
		}, "must start with /"},
		{"rule key template", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^https://a$`, Key: `/a/{{template "xxx" ...`}}
		}, "template/define/block actions are not allowed"},
		{"rule key block", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^https://a$`, Key: `/a/{{- block "xxx" ...`}}
		}, "template/define/block actions are not allowed"},
		{"rule hash group", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^https://a/(?P<x>x)$`, Key: "/a/{{ .x }}", Hash: "y", HashMinlen: 8}}
		}, "capture group"},
		{"rule hash minlen", func(c *Config) {
			c.Rules = []RuleConfig{{Name: "r", Match: `^https://a/(?P<x>x)$`, Key: "/a/{{ .x }}", Hash: "x", HashMinlen: 2}}
		}, "hash_minlen"},
		{"client enabled no token", func(c *Config) {
			c.Clients = []ClientConfig{{Enabled: true, Name: "c", Admin: true}}
		}, "at least one token"},
		{"client short token", func(c *Config) {
			c.Clients = []ClientConfig{{Enabled: true, Name: "c", Admin: true, Tokens: []string{"short"}}}
		}, "shorter than 32"},
		{"client no namespace", func(c *Config) {
			c.Clients = []ClientConfig{{Enabled: true, Name: "c", Tokens: []string{strings.Repeat("b", 32)}}}
		}, "at least one namespace"},
		{"client bad namespace", func(c *Config) {
			c.Clients = []ClientConfig{{Enabled: true, Name: "c", Namespaces: []string{"g"}, Tokens: []string{strings.Repeat("b", 32)}}}
		}, "invalid namespace"},
		{"client shared token", func(c *Config) {
			tok := strings.Repeat("c", 32)
			c.Clients = []ClientConfig{
				{Enabled: true, Name: "a", Admin: true, Tokens: []string{tok}},
				{Enabled: true, Name: "b", Admin: true, Tokens: []string{tok}},
			}
		}, "shared by clients"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateNamespace(t *testing.T) {
	valid := []string{"/g/", "/gh/", "/a/b/", "/git-1/"}
	invalid := []string{"", "g", "/g", "g/", "/g//", "/../", apiRoot, "/.well-known/", "/~x/", "/a/./"}
	for _, ns := range valid {
		if !validNamespace(ns) {
			t.Errorf("expected %q valid", ns)
		}
	}
	for _, ns := range invalid {
		if validNamespace(ns) {
			t.Errorf("expected %q invalid", ns)
		}
	}
}

func TestLoadConfigUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	content := `
data_dir = "./data/"
bogus_key = 1
[server]
public_url = "https://example.com"
[acme]
accept_tos = true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
}

func TestLoadConfigSampleIsRejected(t *testing.T) {
	// The committed sample has "???" tokens and must fail validation.
	if _, err := LoadConfig("urlshort.toml"); err == nil {
		t.Fatal("expected sample config to fail validation (placeholder tokens)")
	}
}

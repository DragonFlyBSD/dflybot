// Copyright (c) 2026 Aaron LI
//
// HSTS header tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestHSTSHeader(t *testing.T) {
	env := newTestEnvWith(t, func(c *Config) {
		c.Server.HSTS = HSTSConfig{Enabled: true, MaxAge: 31536000, IncludeSubDomains: true}
	})
	rec := env.request(http.MethodGet, "/missing", "", nil, nil)
	got := rec.Header().Get("Strict-Transport-Security")
	if got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("HSTS = %q", got)
	}
}

func TestHSTSDisabled(t *testing.T) {
	env := newTestEnv(t)
	rec := env.request(http.MethodGet, "/missing", "", nil, nil)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS = %q, want empty", got)
	}
}

func TestHSTSHeaderValue(t *testing.T) {
	h := HSTSConfig{MaxAge: 100, IncludeSubDomains: true, Preload: true}
	if got := h.HeaderValue(); got != "max-age=100; includeSubDomains; preload" {
		t.Fatalf("HeaderValue = %q", got)
	}
}

func TestHSTSValidation(t *testing.T) {
	cases := []struct {
		name      string
		httpsPort int
		hsts      HSTSConfig
		want      string
	}{
		{"https required", 0, HSTSConfig{Enabled: true, MaxAge: 31536000}, "https_port > 0"},
		{"positive max_age", 443, HSTSConfig{Enabled: true, MaxAge: 0}, "max_age"},
		{"preload needs subdomains", 443, HSTSConfig{Enabled: true, MaxAge: 31536000, Preload: true}, "include_subdomains"},
		{"preload needs one year", 443, HSTSConfig{Enabled: true, MaxAge: 100, IncludeSubDomains: true, Preload: true}, "31536000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.Server.HTTPSPort = tc.httpsPort
			c.Server.HSTS = tc.hsts
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}

	// A valid preload config passes.
	c := baseConfig()
	c.Server.HSTS = HSTSConfig{Enabled: true, MaxAge: 31536000, IncludeSubDomains: true, Preload: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid HSTS config rejected: %v", err)
	}
}

// Copyright (c) 2026 Aaron LI
//
// Trusted-proxy client IP extraction tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	prefixes, err := parseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "::1", "2001:db8::/32", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128", "2001:db8::/32"}
	if len(prefixes) != len(want) {
		t.Fatalf("got %v, want %v", prefixes, want)
	}
	for i := range want {
		if prefixes[i].String() != want[i] {
			t.Errorf("prefix %d = %q, want %q", i, prefixes[i], want[i])
		}
	}
	if _, err := parseTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("expected error for invalid entry")
	}
}

func TestTrustedProxiesValidation(t *testing.T) {
	c := baseConfig()
	c.Server.TrustedProxies = []string{"bogus"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("expected trusted_proxies error, got %v", err)
	}

	c = baseConfig()
	c.Server.TrustedProxies = []string{"0.0.0.0/0"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 {
		t.Fatal("expected a warning for an overly broad trusted proxy")
	}
}

func TestClientIP(t *testing.T) {
	env := newTestEnv(t)
	s := env.srv
	setTrusted := func(list ...string) {
		p, err := parseTrustedProxies(list)
		if err != nil {
			t.Fatal(err)
		}
		s.trustedProxies = p
	}
	req := func(remote string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	check := func(name string, r *http.Request, want string) {
		t.Helper()
		if got := s.clientIP(r); got.String() != want {
			t.Errorf("%s: clientIP = %q, want %q", name, got, want)
		}
	}

	setTrusted("10.0.0.0/8")
	check("untrusted peer ignores XFF",
		req("203.0.113.7:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}), "203.0.113.7")
	check("rightmost untrusted",
		req("10.0.0.1:1234", map[string]string{"X-Forwarded-For": "198.51.100.9, 10.0.0.2"}), "198.51.100.9")
	check("client-injected leftmost ignored",
		req("10.0.0.1:1234", map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.9, 10.0.0.2"}), "198.51.100.9")
	check("X-Real-IP fallback",
		req("10.0.0.1:1234", map[string]string{"X-Real-IP": "198.51.100.5"}), "198.51.100.5")
	check("all-trusted XFF returns leftmost",
		req("10.0.0.1:1234", map[string]string{"X-Forwarded-For": "10.0.0.9, 10.0.0.8"}), "10.0.0.9")
	check("invalid XFF falls back to peer",
		req("10.0.0.1:1234", map[string]string{"X-Forwarded-For": "garbage"}), "10.0.0.1")

	setTrusted("::1/128")
	check("IPv6 peer",
		req("[::1]:1234", map[string]string{"X-Forwarded-For": "2001:db8::5"}), "2001:db8::5")

	setTrusted("127.0.0.0/8", "::1/128")
	check("IPv4-mapped peer",
		req("[::ffff:127.0.0.1]:1234", map[string]string{"X-Real-IP": "203.0.113.9"}), "203.0.113.9")
}

func TestRateKey(t *testing.T) {
	v4, _ := netip.ParseAddr("192.0.2.5")
	if got := rateKey(v4); got != "192.0.2.5" {
		t.Errorf("IPv4 rateKey = %q", got)
	}
	v6, _ := netip.ParseAddr("2001:db8::1")
	if got := rateKey(v6); got != "2001:db8::/64" {
		t.Errorf("IPv6 rateKey = %q", got)
	}
	if got := rateKey(netip.Addr{}); got != "" {
		t.Errorf("invalid rateKey = %q, want empty", got)
	}
}

func TestAccessLogUsesClientIP(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	req.Host = testHost
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := httptest.NewRecorder()
	env.srv.mainHandler.ServeHTTP(rec, req)
	env.logs.Close()

	files, err := filepath.Glob(filepath.Join(env.cfg.LogsDir(), "access-*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no access log written (err=%v)", err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"remote_ip":"198.51.100.7"`) {
		t.Fatalf("access log does not contain the extracted client IP:\n%s", data)
	}
}

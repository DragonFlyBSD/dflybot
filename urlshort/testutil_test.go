// Copyright (c) 2026 Aaron LI
//
// Shared HTTP test helpers.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const (
	testAdminToken = "admin-token-0123456789abcdef012345"
	testGitToken   = "git-token-0123456789abcdef01234567"
	testHost       = "example.com"
)

type testEnv struct {
	t     *testing.T
	cfg   *Config
	store *BoltStore
	srv   *Server
}

func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWith(t, nil)
}

// newTestEnvWith builds a test environment, applying mutate to the config
// before validation.
func newTestEnvWith(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()

	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Server.PublicURL = "https://example.com"
	cfg.Server.HTTPPort = 80
	cfg.Server.HTTPSPort = 443
	cfg.Server.CertFile = certFile
	cfg.Server.KeyFile = keyFile
	cfg.Server.ExtraHosts = []string{"www.example.com"}
	cfg.ACME.Enabled = false
	cfg.Backup.Enabled = false
	cfg.RateLimit = RateLimitConfig{
		RedirectRate:  10000,
		RedirectBurst: 10000,
		APIRate:       10000,
		APIBurst:      10000,
	}
	cfg.Abbreviations = map[string]string{
		"DragonFlyBSD": "dfbsd",
		"dragonfly":    "d",
	}
	cfg.Rules = []RuleConfig{
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
	cfg.Clients = []ClientConfig{
		{Enabled: true, Name: "admin", Admin: true, Tokens: []string{testAdminToken}},
		{Enabled: true, Name: "git", Namespaces: []string{"/g/"}, Tokens: []string{testGitToken}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.applyDerivedDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	store, err := OpenBoltStore(filepath.Join(dir, "links.db"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := NewStatusState()
	cert := makeTestCert(t, "example.com")
	certs := &recordingCertManager{inner: &manualCertManager{cert: &cert}, status: status, logger: slog.Default()}
	srv, err := NewServer(cfg, store, certs, status, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetMaintenance(NewMaintenance(cfg, store, nil))

	env := &testEnv{t: t, cfg: cfg, store: store, srv: srv}
	t.Cleanup(func() {
		srv.Close()
		store.Close()
	})
	return env
}

func (e *testEnv) request(method, target, token string, body any, handler http.Handler) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = testHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if handler == nil {
		handler = e.srv.mainHandler
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// Copyright (c) 2026 Aaron LI
//
// Tests for the HTTP prober (stub servers; a TLS fixture is generated
// locally).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BurntSushi/toml"
)

func testProber(t *testing.T, url string, codes []int, follow, verify bool) *prober {
	t.Helper()
	web := &ConfigWeb{Name: "w", URL: url, StatusCodes: codes}
	p, err := newProber(web, &ConfigTLS{}, &ConfigTimeouts{}, follow, verify)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeOK(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	res := testProber(t, ts.URL, nil, true, false).Probe()
	if !res.ok || res.status != 200 || res.reason != "" {
		t.Fatalf("result = %+v", res)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("hits = %d", hits)
	}
}

func TestProbeStatusCodes(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204
	}))
	defer ok.Close()
	res := testProber(t, ok.URL, []int{200, 204}, true, false).Probe()
	if !res.ok || res.status != 204 {
		t.Fatalf("204 with codes [200,204]: %+v", res)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	res = testProber(t, bad.URL, nil, true, false).Probe()
	if res.ok || res.status != 500 || !strings.Contains(res.reason, "500") {
		t.Fatalf("500 result = %+v", res)
	}
}

func TestProbeRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusMovedPermanently)
	}))
	defer redir.Close()

	res := testProber(t, redir.URL, nil, true, false).Probe()
	if !res.ok || res.status != 200 {
		t.Fatalf("follow redirect result = %+v", res)
	}
	res = testProber(t, redir.URL, nil, false, false).Probe()
	if res.ok || res.status != http.StatusMovedPermanently {
		t.Fatalf("no-follow result = %+v", res)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	// A port that is not listening: quick refusal on 127.0.0.1.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	ts.Close() // now refused
	res := testProber(t, addr, nil, true, false).Probe()
	if res.ok || res.status != 0 || res.reason == "" {
		t.Fatalf("refused result = %+v", res)
	}
}

func certPEM(t *testing.T, der []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeTLSVerify(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Insecure: succeeds.
	res := testProber(t, ts.URL, nil, true, false).Probe()
	if !res.ok {
		t.Fatalf("insecure TLS result = %+v", res)
	}

	// Verify against the system store: self-signed certificate is unknown.
	res = testProber(t, ts.URL, nil, true, true).Probe()
	if res.ok || res.cert != nil || !strings.Contains(strings.ToLower(res.reason), "certificate") {
		t.Fatalf("verify-vs-system result = %+v", res)
	}

	// Verify with the server's own certificate as the trust store.
	caFile := certPEM(t, ts.Certificate().Raw)
	web := &ConfigWeb{Name: "w", URL: ts.URL}
	p, err := newProber(web, &ConfigTLS{CAFile: caFile}, &ConfigTimeouts{}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	res = p.Probe()
	if !res.ok || res.cert == nil {
		t.Fatalf("verify-with-ca result = %+v", res)
	}
	if res.cert.daysLeft <= 0 {
		t.Errorf("daysLeft = %d, want positive", res.cert.daysLeft)
	}
}

func TestConfigDefaults(t *testing.T) {
	// Absent toggles decode as nil -> defaults apply (true).
	var w ConfigWeb
	snippet := `
name = "a"
url = "https://x/"
interval = 30
`
	if _, err := toml.Decode(snippet, &w); err != nil {
		t.Fatal(err)
	}
	if w.FollowRedirection != nil || w.TLSVerify != nil {
		t.Errorf("absent toggles = %v/%v", w.FollowRedirection, w.TLSVerify)
	}
	// Explicit false decodes as a non-nil pointer.
	w = ConfigWeb{}
	snippet = `
name = "a"
url = "https://x/"
interval = 30
follow_redirection = false
tls_verify = false
`
	if _, err := toml.Decode(snippet, &w); err != nil {
		t.Fatal(err)
	}
	if w.FollowRedirection == nil || *w.FollowRedirection {
		t.Errorf("follow = %v", w.FollowRedirection)
	}
	if w.TLSVerify == nil || *w.TLSVerify {
		t.Errorf("tls_verify = %v", w.TLSVerify)
	}
}

func TestStatusCheck(t *testing.T) {
	if !makeStatusCheck(nil)(200) || makeStatusCheck(nil)(404) {
		t.Error("empty codes should accept any 2xx only")
	}
	check := makeStatusCheck([]int{201})
	if !check(201) || check(200) {
		t.Error("custom codes check failed")
	}
}

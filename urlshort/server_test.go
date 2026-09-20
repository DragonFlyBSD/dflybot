// Copyright (c) 2026 Aaron LI
//
// HTTP server and redirect tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHostValidation(t *testing.T) {
	e := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/g/x", nil)
	req.Host = "evil.example.net"
	rec := httptest.NewRecorder()
	e.srv.mainHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("host mismatch = %d, want 421", rec.Code)
	}

	// extra_hosts is accepted.
	req = httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	req.Host = "www.example.com"
	rec = httptest.NewRecorder()
	e.srv.mainHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("extra host = %d, want 404", rec.Code)
	}
}

func TestRedirect(t *testing.T) {
	e := newTestEnv(t)
	if _, _, err := e.store.Create("https://target.example/page", "", "admin", "/g/t", nil); err != nil {
		t.Fatal(err)
	}

	rec := e.request(http.MethodGet, "/g/t", "", nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("redirect = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://target.example/page" {
		t.Fatalf("location = %q", loc)
	}
	for _, h := range []string{"X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}

	// HEAD has the same status and no body.
	rec = e.request(http.MethodHead, "/g/t", "", nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("head = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rec.Body.String())
	}

	// Unknown key.
	if rec := e.request(http.MethodGet, "/g/missing", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", rec.Code)
	}
	// The incoming query string is ignored for lookup.
	if rec := e.request(http.MethodGet, "/g/t?ignored=1", "", nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("query ignored = %d, want 302", rec.Code)
	}
	// Wrong method.
	rec = e.request(http.MethodPost, "/g/t", "", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post redirect = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestSpecialPaths(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodGet, "/robots.txt", "", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("robots = %d %q", rec.Code, rec.Body.String())
	}
	if rec := e.request(http.MethodGet, "/favicon.ico", "", nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("favicon = %d, want 204", rec.Code)
	}
	if rec := e.request(http.MethodGet, "/.well-known/foo", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("well-known = %d, want 404", rec.Code)
	}
}

func TestAbsoluteFormRejected(t *testing.T) {
	e := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/g/x", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	e.srv.mainHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("absolute-form = %d, want 400", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	e := newTestEnv(t)

	// Redirect rate limit is per source IP.
	e.srv.redirectLimiter = NewRateLimiter(0.000001, 1, 100)
	if rec := e.request(http.MethodGet, "/nope", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("first redirect = %d", rec.Code)
	}
	rec := e.request(http.MethodGet, "/nope", "", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second redirect = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After")
	}

	// API rate limit is per token.
	e.srv.apiLimiter = NewRateLimiter(0.000001, 1, 100)
	if rec := e.request(http.MethodGet, "/.api/v1/whoami", testGitToken, nil, nil); rec.Code != 200 {
		t.Fatalf("first api = %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/.api/v1/whoami", testGitToken, nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second api = %d, want 429", rec.Code)
	}
}

func TestHTTPToHTTPSRedirect(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodGet, "/g/t", "", nil, e.srv.httpHandler)
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("http redirect = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/g/t" {
		t.Fatalf("location = %q", loc)
	}
	// Preserve the validated extra host.
	req := httptest.NewRequest(http.MethodGet, "/g/t", nil)
	req.Host = "www.example.com"
	rec = httptest.NewRecorder()
	e.srv.httpHandler.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "https://www.example.com/g/t" {
		t.Fatalf("extra host location = %q", loc)
	}
}

// TestNewServerAccessLogFailure verifies that a failure while creating the
// access logger returns an error without panicking (the rollback defer must not
// run with a nil receiver).
func TestNewServerAccessLogFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DataDir = blocker // LogsDir() cannot be created

	store, err := OpenBoltStore(filepath.Join(dir, "links.db"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := NewServer(cfg, store, nil, nil, nil); err == nil {
		t.Fatal("expected an error when the access log directory cannot be created")
	}
}

// TestServerCloseIdempotent checks that Close is safe to call repeatedly.
func TestServerCloseIdempotent(t *testing.T) {
	env := newTestEnv(t)
	env.srv.Close()
	env.srv.Close()
}

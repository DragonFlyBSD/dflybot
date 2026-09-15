// Copyright (c) 2026 Aaron LI
//
// REST API tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestAPIHealthAndAuth(t *testing.T) {
	e := newTestEnv(t)

	if rec := e.request(http.MethodGet, "/.api/v1/health", "", nil, nil); rec.Code != 200 {
		t.Fatalf("health status = %d", rec.Code)
	}
	if rec := e.request(http.MethodGet, "/.api/v1/whoami", "", nil, nil); rec.Code != 401 {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := e.request(http.MethodGet, "/.api/v1/whoami", "wrong", nil, nil); rec.Code != 401 {
		t.Fatalf("bad token: status = %d, want 401", rec.Code)
	}
	rec := e.request(http.MethodGet, "/.api/v1/whoami", testGitToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("whoami status = %d", rec.Code)
	}
	var who map[string]any
	decodeBody(t, rec, &who)
	if who["name"] != "git" {
		t.Fatalf("whoami = %v", who)
	}
}

func TestAPICreateRuleAndIdempotent(t *testing.T) {
	e := newTestEnv(t)
	target := "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56"

	rec := e.request(http.MethodPost, "/.api/v1/links", testAdminToken, map[string]string{"target": target}, nil)
	if rec.Code != 201 {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if resp.Key != "/gh/dfbsd/p/56" || !resp.Created {
		t.Fatalf("unexpected create response: %+v", resp)
	}
	if resp.ShortURL != "https://example.com/gh/dfbsd/p/56" {
		t.Fatalf("short_url = %q", resp.ShortURL)
	}

	rec = e.request(http.MethodPost, "/.api/v1/links", testAdminToken, map[string]string{"target": target}, nil)
	if rec.Code != 200 {
		t.Fatalf("idempotent status = %d", rec.Code)
	}
	var resp2 createResponse
	decodeBody(t, rec, &resp2)
	if resp2.Created || resp2.Key != resp.Key {
		t.Fatalf("idempotent response: %+v", resp2)
	}
}

func TestAPINamespaceEnforcement(t *testing.T) {
	e := newTestEnv(t)
	target := "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56"

	// A /g/ client may not create a /gh/ rule key.
	rec := e.request(http.MethodPost, "/.api/v1/links", testGitToken, map[string]string{"target": target}, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Explicit key outside the namespace.
	rec = e.request(http.MethodPost, "/.api/v1/links", testGitToken,
		map[string]string{"target": "https://example.org/x", "key": "/gh/x"}, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAPIGitwebHash(t *testing.T) {
	e := newTestEnv(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	target := "https://gitweb.dragonflybsd.org/dragonfly.git/commitdiff/" + sha
	rec := e.request(http.MethodPost, "/.api/v1/links", testGitToken, map[string]string{"target": target}, nil)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if resp.Key != "/g/d/"+sha[:8] {
		t.Fatalf("key = %q, want the minimum unique prefix", resp.Key)
	}
}

func TestAPIRandomFallback(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, "/.api/v1/links", testGitToken, map[string]string{"target": "https://example.org/unmatched"}, nil)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if !strings.HasPrefix(resp.Key, "/g/~") {
		t.Fatalf("random key = %q", resp.Key)
	}
}

func TestAPIExplicitKeyAndConflict(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, "/.api/v1/links", testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/mine"}, nil)
	if rec.Code != 201 {
		t.Fatalf("create explicit status = %d", rec.Code)
	}
	rec = e.request(http.MethodPost, "/.api/v1/links", testAdminToken,
		map[string]string{"target": "https://example.org/two", "key": "/g/mine"}, nil)
	if rec.Code != 409 {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	rec = e.request(http.MethodPost, "/.api/v1/links", testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/other"}, nil)
	if rec.Code != 409 {
		t.Fatalf("expected 409 for target under a different key, got %d", rec.Code)
	}
}

func TestAPIExistingTargetOutsideNamespace(t *testing.T) {
	e := newTestEnv(t)
	target := "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56"
	// Admin creates the /gh/ link.
	if rec := e.request(http.MethodPost, "/.api/v1/links", testAdminToken, map[string]string{"target": target}, nil); rec.Code != 201 {
		t.Fatalf("admin create = %d", rec.Code)
	}
	// The /g/ client gets the existing, already-published key back (200).
	rec := e.request(http.MethodPost, "/.api/v1/links", testGitToken, map[string]string{"target": target}, nil)
	if rec.Code != 200 {
		t.Fatalf("existing target = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp createResponse
	decodeBody(t, rec, &resp)
	if resp.Created || resp.Key != "/gh/dfbsd/p/56" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestAPIGetUpdateDelete(t *testing.T) {
	e := newTestEnv(t)
	create := e.request(http.MethodPost, "/.api/v1/links", testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/one"}, nil)
	if create.Code != 201 {
		t.Fatalf("create = %d", create.Code)
	}

	rec := e.request(http.MethodGet, "/.api/v1/links?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get = %d", rec.Code)
	}
	var view linkView
	decodeBody(t, rec, &view)
	if view.Target != "https://example.org/one" {
		t.Fatalf("target = %q", view.Target)
	}

	rec = e.request(http.MethodPut, "/.api/v1/links?key=/g/one", testAdminToken,
		map[string]string{"target": "https://example.org/two"}, nil)
	if rec.Code != 200 {
		t.Fatalf("put = %d body=%s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &view)
	if view.Target != "https://example.org/two" {
		t.Fatalf("updated target = %q", view.Target)
	}

	rec = e.request(http.MethodGet, "/.api/v1/links?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get after update = %d", rec.Code)
	}

	rec = e.request(http.MethodDelete, "/.api/v1/links?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/.api/v1/links?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
	rec = e.request(http.MethodDelete, "/.api/v1/links?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
}

func TestAPIList(t *testing.T) {
	e := newTestEnv(t)
	for i, key := range []string{"/g/a", "/g/b", "/g/c"} {
		rec := e.request(http.MethodPost, "/.api/v1/links", testGitToken,
			map[string]string{"target": "https://example.org/" + strings.TrimPrefix(key, "/"), "key": key}, nil)
		if rec.Code != 201 {
			t.Fatalf("create %d = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := e.request(http.MethodGet, "/.api/v1/links?namespace=/g/", testGitToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("list = %d", rec.Code)
	}
	var resp struct {
		Links      []linkView `json:"links"`
		NextCursor string     `json:"next_cursor"`
	}
	decodeBody(t, rec, &resp)
	if len(resp.Links) != 3 || resp.NextCursor != "" {
		t.Fatalf("list = %+v", resp)
	}
	// Namespace outside the caller's namespaces.
	rec = e.request(http.MethodGet, "/.api/v1/links?namespace=/gh/", testGitToken, nil, nil)
	if rec.Code != 403 {
		t.Fatalf("foreign namespace list = %d, want 403", rec.Code)
	}
}

func TestAPIStatus(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request(http.MethodGet, "/.api/v1/status", testGitToken, nil, nil); rec.Code != 403 {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	rec := e.request(http.MethodGet, "/.api/v1/status", testAdminToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{testAdminToken, testGitToken} {
		if strings.Contains(body, secret) {
			t.Fatalf("status leaked a token")
		}
	}

	// The db object matches the documented shape (section 10.4).
	var raw map[string]any
	decodeBody(t, rec, &raw)
	db, ok := raw["db"].(map[string]any)
	if !ok {
		t.Fatalf("status db missing: %v", raw["db"])
	}
	if _, ok := db["file_size_bytes"]; !ok {
		t.Fatalf("db.file_size_bytes missing: %v", db)
	}
	if _, ok := db["tx_stats"].(map[string]any); !ok {
		t.Fatalf("db.tx_stats missing: %v", db)
	}
	if _, ok := raw["maintenance"].(map[string]any); !ok {
		t.Fatalf("status maintenance missing: %v", raw["maintenance"])
	}
}

func TestAPIBodyLimit(t *testing.T) {
	e := newTestEnv(t)
	big := strings.Repeat("a", 70<<10)
	rec := e.request(http.MethodPost, "/.api/v1/links", testAdminToken, map[string]string{"target": big}, nil)
	if rec.Code != 413 {
		t.Fatalf("body limit = %d, want 413", rec.Code)
	}
}

func TestAPIMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request(http.MethodDelete, "/.api/v1/health", "", nil, nil); rec.Code != 405 {
		t.Fatalf("delete health = %d, want 405", rec.Code)
	}
	if rec := e.request(http.MethodGet, "/.api/v1/nope", testAdminToken, nil, nil); rec.Code != 404 {
		t.Fatalf("unknown api = %d, want 404", rec.Code)
	}
}

func TestAPIInvalidJSON(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, "/.api/v1/links", testAdminToken, map[string]string{"bogus": "x"}, nil)
	if rec.Code != 400 {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}
}

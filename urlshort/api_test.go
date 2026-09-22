// Copyright (c) 2026 Aaron LI
//
// REST API tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAPIIndex(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{apiBase, apiBase + "/"} {
		rec := e.request(http.MethodGet, path, "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		var body struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			Endpoints []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"endpoints"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if body.Name != programName || body.Version != version {
			t.Errorf("GET %s: name/version = %q/%q", path, body.Name, body.Version)
		}
		if len(body.Endpoints) != len(apiIndex()) {
			t.Errorf("GET %s: %d endpoints, want %d", path, len(body.Endpoints), len(apiIndex()))
		}
	}
}

// TestAPIIndexCoversRoutes guards against the index drifting from the routes
// actually handled by handleAPI.
func TestAPIIndexCoversRoutes(t *testing.T) {
	e := newTestEnv(t)
	for _, ep := range apiIndex() {
		if !isAPIPath(ep.Path) {
			t.Errorf("%s %s: not an API path", ep.Method, ep.Path)
		}
		rec := e.request(ep.Method, ep.Path, testAdminToken, nil, nil)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d", ep.Method, ep.Path, rec.Code)
		}
	}
}

// TestAPIIPRateLimit checks the per-IP API limiter applied to public and
// unknown endpoints, and its JSON 429 envelope.
func TestAPIIPRateLimit(t *testing.T) {
	e := newTestEnvWith(t, func(c *Config) {
		c.RateLimit.APIIPRate = 0.0001
		c.RateLimit.APIIPBurst = 1
	})
	if rec := e.request(http.MethodGet, apiPathHealth, "", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("first health = %d", rec.Code)
	}
	rec := e.request(http.MethodGet, apiPathHealth, "", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second health = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("429 Content-Type = %q", ct)
	}
}

// TestAPIIPRateLimitPreAuth checks that the IP limiter runs before
// authentication, so failed auth cannot flood the token scan.
func TestAPIIPRateLimitPreAuth(t *testing.T) {
	e := newTestEnvWith(t, func(c *Config) {
		c.RateLimit.APIIPRate = 0.0001
		c.RateLimit.APIIPBurst = 1
	})
	if rec := e.request(http.MethodGet, apiBase+"/nope", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint = %d", rec.Code)
	}
	rec := e.request(http.MethodGet, apiPathWhoami, "bad-token", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("bad-token request = %d, want 429 before auth", rec.Code)
	}
}

// TestAPIAdminSubjectToIPLimit checks that admin endpoints skip the per-token
// limiter but still hit the per-IP limiter.
func TestAPIAdminSubjectToIPLimit(t *testing.T) {
	e := newTestEnvWith(t, func(c *Config) {
		c.RateLimit.APIIPRate = 0.0001
		c.RateLimit.APIIPBurst = 1
		c.RateLimit.APIRate = 10000
		c.RateLimit.APIBurst = 10000
	})
	if rec := e.request(http.MethodGet, apiPathStatus, testAdminToken, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d", rec.Code)
	}
	rec := e.request(http.MethodGet, apiPathStatus, testAdminToken, nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec.Code)
	}
}

func TestAPIHealthAndAuth(t *testing.T) {
	e := newTestEnv(t)

	if rec := e.request(http.MethodGet, apiPathHealth, "", nil, nil); rec.Code != 200 {
		t.Fatalf("health status = %d", rec.Code)
	}
	if rec := e.request(http.MethodGet, apiPathWhoami, "", nil, nil); rec.Code != 401 {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := e.request(http.MethodGet, apiPathWhoami, "wrong", nil, nil); rec.Code != 401 {
		t.Fatalf("bad token: status = %d, want 401", rec.Code)
	}
	rec := e.request(http.MethodGet, apiPathWhoami, testGitToken, nil, nil)
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

	rec := e.request(http.MethodPost, apiPathLinks, testAdminToken, map[string]string{"target": target}, nil)
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

	rec = e.request(http.MethodPost, apiPathLinks, testAdminToken, map[string]string{"target": target}, nil)
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
	rec := e.request(http.MethodPost, apiPathLinks, testGitToken, map[string]string{"target": target}, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Explicit key outside the namespace.
	rec = e.request(http.MethodPost, apiPathLinks, testGitToken,
		map[string]string{"target": "https://example.org/x", "key": "/gh/x"}, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAPIGitwebHash(t *testing.T) {
	e := newTestEnv(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	target := "https://gitweb.dragonflybsd.org/dragonfly.git/commitdiff/" + sha
	rec := e.request(http.MethodPost, apiPathLinks, testGitToken, map[string]string{"target": target}, nil)
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
	rec := e.request(http.MethodPost, apiPathLinks, testGitToken, map[string]string{"target": "https://example.org/unmatched"}, nil)
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
	rec := e.request(http.MethodPost, apiPathLinks, testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/mine"}, nil)
	if rec.Code != 201 {
		t.Fatalf("create explicit status = %d", rec.Code)
	}
	rec = e.request(http.MethodPost, apiPathLinks, testAdminToken,
		map[string]string{"target": "https://example.org/two", "key": "/g/mine"}, nil)
	if rec.Code != 409 {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	rec = e.request(http.MethodPost, apiPathLinks, testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/other"}, nil)
	if rec.Code != 409 {
		t.Fatalf("expected 409 for target under a different key, got %d", rec.Code)
	}
}

func TestAPIExistingTargetOutsideNamespace(t *testing.T) {
	e := newTestEnv(t)
	target := "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56"
	// Admin creates the /gh/ link.
	if rec := e.request(http.MethodPost, apiPathLinks, testAdminToken, map[string]string{"target": target}, nil); rec.Code != 201 {
		t.Fatalf("admin create = %d", rec.Code)
	}
	// The /g/ client gets the existing, already-published key back (200).
	rec := e.request(http.MethodPost, apiPathLinks, testGitToken, map[string]string{"target": target}, nil)
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
	create := e.request(http.MethodPost, apiPathLinks, testAdminToken,
		map[string]string{"target": "https://example.org/one", "key": "/g/one"}, nil)
	if create.Code != 201 {
		t.Fatalf("create = %d", create.Code)
	}

	rec := e.request(http.MethodGet, apiPathLinks+"?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get = %d", rec.Code)
	}
	var view linkView
	decodeBody(t, rec, &view)
	if view.Target != "https://example.org/one" {
		t.Fatalf("target = %q", view.Target)
	}

	rec = e.request(http.MethodPut, apiPathLinks+"?key=/g/one", testAdminToken,
		map[string]string{"target": "https://example.org/two"}, nil)
	if rec.Code != 200 {
		t.Fatalf("put = %d body=%s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &view)
	if view.Target != "https://example.org/two" {
		t.Fatalf("updated target = %q", view.Target)
	}

	rec = e.request(http.MethodGet, apiPathLinks+"?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("get after update = %d", rec.Code)
	}

	rec = e.request(http.MethodDelete, apiPathLinks+"?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = e.request(http.MethodGet, apiPathLinks+"?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
	rec = e.request(http.MethodDelete, apiPathLinks+"?key=/g/one", testAdminToken, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
}

func TestAPIList(t *testing.T) {
	e := newTestEnv(t)
	for i, key := range []string{"/g/a", "/g/b", "/g/c"} {
		rec := e.request(http.MethodPost, apiPathLinks, testGitToken,
			map[string]string{"target": "https://example.org/" + strings.TrimPrefix(key, "/"), "key": key}, nil)
		if rec.Code != 201 {
			t.Fatalf("create %d = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := e.request(http.MethodGet, apiPathLinks+"?namespace=/g/", testGitToken, nil, nil)
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
	rec = e.request(http.MethodGet, apiPathLinks+"?namespace=/gh/", testGitToken, nil, nil)
	if rec.Code != 403 {
		t.Fatalf("foreign namespace list = %d, want 403", rec.Code)
	}
}

func TestAPIStatus(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request(http.MethodGet, apiPathStatus, testGitToken, nil, nil); rec.Code != 403 {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	rec := e.request(http.MethodGet, apiPathStatus, testAdminToken, nil, nil)
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
	rec := e.request(http.MethodPost, apiPathLinks, testAdminToken, map[string]string{"target": big}, nil)
	if rec.Code != 413 {
		t.Fatalf("body limit = %d, want 413", rec.Code)
	}
}

func TestAPIMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.request(http.MethodDelete, apiPathHealth, "", nil, nil); rec.Code != 405 {
		t.Fatalf("delete health = %d, want 405", rec.Code)
	}
	if rec := e.request(http.MethodGet, apiBase+"/nope", testAdminToken, nil, nil); rec.Code != 404 {
		t.Fatalf("unknown api = %d, want 404", rec.Code)
	}
}

func TestAPIInvalidJSON(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, apiPathLinks, testAdminToken, map[string]string{"bogus": "x"}, nil)
	if rec.Code != 400 {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}
}

func TestAPICanonicalizeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"HTTPS://GitHub.COM/a/b", "https://github.com/a/b"},
		{"http://Example.COM:80/a", "http://example.com/a"},
		{"https://example.com:443/a", "https://example.com/a"},
		{"https://example.com/a?b=1&c=2#frag", "https://example.com/a?b=1&c=2#frag"},
		{"https://example.com/A/B", "https://example.com/A/B"},
		{"http://[::1]:80/x", "http://[::1]/x"},
		{"https://example.com:8443/x", "https://example.com:8443/x"},
	}
	for _, tc := range cases {
		got, err := canonicalizeTarget(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",
		"ftp://example.com/x",
		"https://u:p@example.com/x",
		"https:///x",
		"://bad",
	}
	for _, in := range bad {
		if _, err := canonicalizeTarget(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

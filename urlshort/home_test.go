// Copyright (c) 2026 Aaron LI
//
// Home page tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestHomePage(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodGet, "/", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, programName) || !strings.Contains(body, version) {
		t.Fatalf("home page missing name/version:\n%s", body)
	}
	if !strings.Contains(body, apiBase) {
		t.Fatalf("home page missing API link:\n%s", body)
	}
}

func TestHomePageMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, "/", "", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}

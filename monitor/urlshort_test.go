// Copyright (c) 2026 Aaron LI
//
// URL shortener client tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewURLShortener(t *testing.T) {
	if _, err := NewURLShortener(nil); err == nil {
		t.Error("nil config should fail")
	}
	if _, err := NewURLShortener(&ConfigURLShort{API: "not-a-url", Token: "t"}); err == nil {
		t.Error("invalid api URL should fail")
	}
	if _, err := NewURLShortener(&ConfigURLShort{API: "https://example.com", Token: ""}); err == nil {
		t.Error("missing token should fail")
	}
	s, err := NewURLShortener(&ConfigURLShort{API: "https://example.com/.api/v1/", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if s.api != "https://example.com/.api/v1" {
		t.Errorf("api = %q, want trailing slash trimmed", s.api)
	}
}

func TestURLShortenerShorten(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"short_url": "https://s.example/g/d/01234567",
		})
	}))
	defer srv.Close()

	s, err := NewURLShortener(&ConfigURLShort{API: srv.URL + "/.api/v1", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	target := "https://gitweb.dragonflybsd.org/dragonfly.git/commit/0123456789abcdef"
	short, err := s.Shorten(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if short != "https://s.example/g/d/01234567" {
		t.Errorf("short = %q", short)
	}
	if gotPath != "/.api/v1/links" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	var req struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil || req.Target != target {
		t.Errorf("body = %q", gotBody)
	}
}

func TestURLShortenerShortenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "forbidden",
				"message": "key is outside namespace",
			},
		})
	}))
	defer srv.Close()

	s, err := NewURLShortener(&ConfigURLShort{API: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Shorten(context.Background(), "https://example.com/x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error = %v", err)
	}
}

func TestURLShortenerMissingShortURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"key":"/x"}`)
	}))
	defer srv.Close()

	s, err := NewURLShortener(&ConfigURLShort{API: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Shorten(context.Background(), "https://example.com/x"); err == nil {
		t.Fatal("expected error for missing short_url")
	}
}

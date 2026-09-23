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

func TestURLShortenerShortenBatch(t *testing.T) {
	var gotPath, gotAuth string
	var gotItems []shortBatchItem
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req shortBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotItems = req.Items
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"target": req.Items[0].Target, "created": true,
					"link": map[string]string{"short_url": "https://s.example/a"}},
				{"target": req.Items[1].Target,
					"error": map[string]string{"code": "bad_request", "message": "invalid"}},
				{"target": req.Items[2].Target, "created": true,
					"link": map[string]string{"short_url": "https://s.example/c"}},
			},
		})
	}))
	defer srv.Close()

	s, err := NewURLShortener(&ConfigURLShort{API: srv.URL + "/.api/v1", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"https://example.com/1", "https://example.com/2", "https://example.com/3"}
	short, err := s.ShortenBatch(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/.api/v1/links/batch" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(gotItems) != 3 || gotItems[0].Target != targets[0] {
		t.Errorf("items = %+v", gotItems)
	}
	if len(short) != 2 || short[targets[0]] != "https://s.example/a" || short[targets[2]] != "https://s.example/c" {
		t.Errorf("short = %v", short)
	}
	if _, ok := short[targets[1]]; ok {
		t.Errorf("failed target should be absent: %v", short)
	}
}

func TestURLShortenerShortenBatchEmpty(t *testing.T) {
	s, err := NewURLShortener(&ConfigURLShort{API: "https://example.com", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	short, err := s.ShortenBatch(context.Background(), nil)
	if err != nil || len(short) != 0 {
		t.Fatalf("empty batch = %v, %v", short, err)
	}
}

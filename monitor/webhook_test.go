// Copyright (c) 2026 Aaron LI
//
// Tests for the shared monitor package.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookPost(t *testing.T) {
	var got struct {
		body string
		auth string
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		got.auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := NewWebhook(&ConfigWebhook{URL: ts.URL, Method: "POST", Token: "tok",
		From: "mon", Target: "#ch"})
	if err := w.Post(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(got.body), &m); err != nil {
		t.Fatal(err)
	}
	if m["from"] != "mon" || m["target"] != "#ch" || m["text"] != "hello" {
		t.Errorf("body = %v", m)
	}
	if got.auth != "Bearer tok" {
		t.Errorf("auth = %q", got.auth)
	}
}

func TestWebhookPostRetriesThenFails(t *testing.T) {
	var count atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	w := NewWebhook(&ConfigWebhook{URL: ts.URL, Method: "POST",
		From: "mon", Target: "#ch"})
	w.backoff = time.Millisecond
	w.maxAttempts = 3
	if err := w.Post(context.Background(), "x"); err == nil {
		t.Fatal("expected error after retries")
	}
	if count.Load() != 3 {
		t.Errorf("attempts = %d, want 3", count.Load())
	}
}

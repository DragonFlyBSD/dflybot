// Copyright (c) 2026 Aaron LI
//
// Tests for the Redmine project monitor (stub Redmine server).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---- helpers ----

type recordPoster struct {
	mu   sync.Mutex
	msgs []string
}

func (p *recordPoster) GetMaxLength() int { return 400 }
func (p *recordPoster) Post(_ context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, text)
	return nil
}
func (p *recordPoster) messages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.msgs...)
}

// feedStub is a stateful fake of the Redmine activity Atom feed endpoint.
type feedStub struct {
	mu      sync.Mutex
	etag    string
	entries []string
}

func (s *feedStub) set(etag string, entries ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.etag = etag
	s.entries = append([]string(nil), entries...)
}

func (s *feedStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", s.etag)
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		io.WriteString(w, testFeed(s.entries...))
	})
}

// anubisStub toggles between the Anubis challenge page and the feed.
type anubisStub struct {
	mu      sync.Mutex
	blocked bool
	etag    string
	feed    string
}

func (s *anubisStub) setBlocked(blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = blocked
}

func (s *anubisStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.blocked {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<html><head><title>Making sure you're not a bot!</title>`+
				`</head><body>anubis</body></html>`)
			return
		}
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", s.etag)
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		io.WriteString(w, s.feed)
	})
}

func defaultProject() *ConfigProject {
	return &ConfigProject{Name: "dragonfly", Interval: 60}
}

func newTestMonitor(t *testing.T, ts *httptest.Server, cfg *ConfigProject,
	poster *recordPoster) (*ProjectMonitor, string) {
	t.Helper()
	dir := t.TempDir()
	client := newAtomClient()
	cfg.FeedURL = ts.URL + "/projects/dragonfly/activity.atom?key=SECRET"
	m := NewProjectMonitor(cfg, client, poster, dir, nil)
	m.loadState()
	return m, dir
}

// ---- tests ----

func TestMonitorSeedThenAnnounce(t *testing.T) {
	const (
		t0 = "2026-09-10T15:00:00Z"
		t1 = "2026-09-10T16:00:00Z"
		t2 = "2026-09-10T17:00:00Z"
		t3 = "2026-09-10T18:00:00Z"
	)
	stub := &feedStub{}
	stub.set(`"e0"`, testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (New): first issue", t0, "alice", "<p>first body</p>"))
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, dir := newTestMonitor(t, ts, defaultProject(), poster)

	m.poll() // first run: seed silently
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("seed announced: %v", got)
	}

	// New activity (newest first): #1 resolved, #1 commented, #2 created.
	stub.set(`"e1"`,
		testEntry("https://bugs.example.org/issues/1#change-6",
			"DragonFlyBSD - Bug #1 (Resolved): first issue", t3, "carol", "<p>fixed now</p>"),
		testEntry("https://bugs.example.org/issues/1#change-5",
			"DragonFlyBSD - Bug #1: first issue", t2, "bob", "<p>a comment</p>"),
		testEntry("https://bugs.example.org/issues/2",
			"DragonFlyBSD - Bug #2 (New): second issue", t1, "dave", "<p>second body</p>"),
	)
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	for _, want := range []string{
		"issue #2 (Bug: second issue) created by dave: https://bugs.example.org/issues/2",
		"issue #1 (Bug: first issue) commented by bob: a comment",
		"issue #1 (Bug: first issue) resolved by carol: fixed now",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message missing %q:\n%s", want, msgs[0])
		}
	}

	hist, err := os.ReadFile(filepath.Join(dir, "dragonfly.history"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(string(hist)), "\n") + 1; n != 3 {
		t.Errorf("history lines = %d, want 3:\n%s", n, hist)
	}
	for _, want := range []string{
		`"action":"create"`, `"action":"comment"`, `"action":"resolve"`,
		`"url":"https://bugs.example.org/issues/2"`,
	} {
		if !strings.Contains(string(hist), want) {
			t.Errorf("history missing %q:\n%s", want, hist)
		}
	}
}

func TestMonitorActionFilter(t *testing.T) {
	stub := &feedStub{}
	stub.set(`"e0"`, testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (New): first issue", "2026-09-10T15:00:00Z", "alice", "<p>body</p>"))
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	cfg := defaultProject()
	cfg.Actions = []string{"create"} // only creations announced
	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, cfg, poster)
	m.poll() // seed

	stub.set(`"e1"`,
		testEntry("https://bugs.example.org/issues/1#change-5",
			"DragonFlyBSD - Bug #1: first issue", "2026-09-10T17:00:00Z", "bob", "<p>a comment</p>"),
		testEntry("https://bugs.example.org/issues/2",
			"DragonFlyBSD - Bug #2 (New): second issue", "2026-09-10T16:00:00Z", "dave", "<p>body</p>"),
	)
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "created by dave") {
		t.Fatalf("messages = %v", msgs)
	}
	if strings.Contains(msgs[0], "commented") {
		t.Errorf("filtered comment announced: %s", msgs[0])
	}
}

func TestMonitorReopen(t *testing.T) {
	stub := &feedStub{}
	stub.set(`"e0"`, testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (Resolved): first issue", "2026-09-10T15:00:00Z", "alice", ""))
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultProject(), poster)
	m.poll() // seed with a resolved issue

	stub.set(`"e1"`, testEntry("https://bugs.example.org/issues/1#change-5",
		"DragonFlyBSD - Bug #1 (Feedback): first issue", "2026-09-10T16:00:00Z", "bob", "<p>please retest</p>"))
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "reopened by bob: please retest") {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestMonitorAnubisWarn(t *testing.T) {
	stub := &anubisStub{blocked: true, etag: `"v1"`,
		feed: testFeed(testEntry("https://bugs.example.org/issues/1",
			"DragonFlyBSD - Bug #1 (New): first issue", "2026-09-10T15:00:00Z", "alice", "<p>body</p>"))}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultProject(), poster)

	m.poll()
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("warned before the threshold: %v", got)
	}
	m.poll() // third consecutive block: warn
	got := poster.messages()
	if len(got) != 1 || !strings.Contains(got[0], "Anubis") {
		t.Fatalf("messages after threshold = %v", got)
	}
	m.poll() // still blocked: no repeated warning
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("warning repeated: %v", got)
	}

	// A success re-arms the warning for a later block streak.
	stub.setBlocked(false)
	m.poll() // seed
	stub.setBlocked(true)
	m.poll()
	m.poll()
	m.poll()
	if got := poster.messages(); len(got) != 2 {
		t.Fatalf("warning not re-armed: %v", got)
	}
}

func TestMonitorRestartDedupe(t *testing.T) {
	stub := &feedStub{}
	stub.set(`"e0"`, testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (New): first issue", "2026-09-10T15:00:00Z", "alice", "<p>body</p>"))
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	dir := t.TempDir()
	cfg := defaultProject()
	cfg.FeedURL = ts.URL + "/projects/dragonfly/activity.atom?key=SECRET"
	poster := &recordPoster{}
	client := newAtomClient()

	m1 := NewProjectMonitor(cfg, client, poster, dir, nil)
	m1.loadState()
	m1.poll() // seed

	m2 := NewProjectMonitor(cfg, client, poster, dir, nil)
	m2.loadState()
	m2.poll() // 304: nothing re-announced
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("restart re-announced: %v", got)
	}
}

func TestMonitorSameSecondBoundary(t *testing.T) {
	const sameT = "2026-09-10T15:00:00Z"
	stub := &feedStub{}
	stub.set(`"e0"`, testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (New): first issue", sameT, "alice", "<p>body</p>"))
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultProject(), poster)
	m.poll() // seed at sameT

	// Two comments sharing the watermark second arrive together.
	stub.set(`"e1"`,
		testEntry("https://bugs.example.org/issues/1#change-6",
			"DragonFlyBSD - Bug #1: first issue", sameT, "bob", "<p>second</p>"),
		testEntry("https://bugs.example.org/issues/1#change-5",
			"DragonFlyBSD - Bug #1: first issue", sameT, "bob", "<p>first</p>"))
	m.poll()
	if got := poster.messages(); len(got) != 1 ||
		!strings.Contains(got[0], "first") || !strings.Contains(got[0], "second") {
		t.Fatalf("same-second poll = %v", got)
	}

	// Redelivery of the same second's entries must not re-announce.
	stub.set(`"e2"`,
		testEntry("https://bugs.example.org/issues/1#change-6",
			"DragonFlyBSD - Bug #1: first issue", sameT, "bob", "<p>second</p>"),
		testEntry("https://bugs.example.org/issues/1#change-5",
			"DragonFlyBSD - Bug #1: first issue", sameT, "bob", "<p>first</p>"))
	m.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("redelivery re-announced: %v", got)
	}
}

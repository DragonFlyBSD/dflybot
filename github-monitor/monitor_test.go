// Copyright (c) 2026 Aaron LI
//
// Tests for the GitHub repo monitor (stub GitHub server, no real GitHub).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/liweitianux/dflybot/monitor"
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

// openEvent builds an "opened issue" event JSON with the given id.
func openEvent(id string) string {
	return eventJSON("IssuesEvent", id, "opened")
}

// eventsStub is a stateful fake of the repository events endpoint: it
// honours If-None-Match/ETag and serves a fixed newest-first event list.
type eventsStub struct {
	mu   sync.Mutex
	etag string
	list []string // newest first
}

func (s *eventsStub) setEvents(etag string, list []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.etag = etag
	s.list = append([]string(nil), list...)
}

func (s *eventsStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", s.etag)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[")
		for i, e := range s.list {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprint(w, e)
		}
		fmt.Fprint(w, "]")
	})
}

func newTestMonitor(t *testing.T, ts *httptest.Server, cfg *ConfigRepo, poster *recordPoster) (*RepoMonitor, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, cfg.Project), 0o755); err != nil {
		t.Fatal(err)
	}
	client := newGitHubClient("")
	client.baseURL = ts.URL
	m := NewRepoMonitor(cfg, client, poster, dir, nil)
	m.loadState()
	return m, dir
}

func defaultRepo() *ConfigRepo {
	return &ConfigRepo{Project: "o", Repo: "r", Interval: 60}
}

func readState(t *testing.T, dir string) repoState {
	t.Helper()
	var st repoState
	exists, err := monitor.ReadJSON(filepath.Join(dir, "o", "r.state"), &st)
	if err != nil || !exists {
		t.Fatalf("read state: exists=%v err=%v", exists, err)
	}
	return st
}

// ---- tests ----

func TestMonitorSeedThenAnnounce(t *testing.T) {
	stub := &eventsStub{}
	stub.setEvents(`"e1"`, []string{openEvent("3"), openEvent("2"), openEvent("1")})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, dir := newTestMonitor(t, ts, defaultRepo(), poster)

	m.poll() // first run: seed, no announcements
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("seed announced: %v", got)
	}
	if st := readState(t, dir); st.LastEventID != 3 {
		t.Fatalf("watermark = %d, want 3", st.LastEventID)
	}

	// No new events (304): nothing announced.
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("unchanged poll announced: %v", got)
	}

	// New events: announced and recorded in the history.
	stub.setEvents(`"e2"`, []string{openEvent("5"), openEvent("4")})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[o/r] ") ||
		!strings.Contains(msgs[0], "issue #12 (fix foo) opened by aly") {
		t.Fatalf("messages = %v", msgs)
	}
	if st := readState(t, dir); st.LastEventID != 5 {
		t.Errorf("watermark = %d, want 5", st.LastEventID)
	}
	hist, err := os.ReadFile(filepath.Join(dir, "o", "r.history"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(hist)), "\n") + 1; lines != 2 {
		t.Errorf("history lines = %d, want 2", lines)
	}
}

func TestMonitorRestartDedupe(t *testing.T) {
	stub := &eventsStub{}
	stub.setEvents(`"e1"`, []string{openEvent("10"), openEvent("9")})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	cfg := defaultRepo()
	client := newGitHubClient("")
	client.baseURL = ts.URL
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "o"), 0o755); err != nil {
		t.Fatal(err)
	}

	m1 := NewRepoMonitor(cfg, client, poster, dir, nil)
	m1.loadState()
	m1.poll() // seed, watermark = 10

	// Restart: a fresh monitor over the same state dir must not re-announce.
	m2 := NewRepoMonitor(cfg, client, poster, dir, nil)
	m2.loadState()
	m2.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("restart re-announced: %v", got)
	}

	// Only genuinely new events are announced after the restart.
	stub.setEvents(`"e2"`, []string{openEvent("12"), openEvent("11")})
	m2.poll()
	if got := poster.messages(); len(got) != 1 ||
		!strings.Contains(got[0], "issue #12") ||
		!strings.Contains(got[0], "opened by") {
		t.Fatalf("post-restart messages = %v", got)
	}
}

func TestMonitorActionFilter(t *testing.T) {
	stub := &eventsStub{}
	stub.setEvents(`"e1"`, []string{openEvent("20"), openEvent("19")})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	cfg := defaultRepo()
	cfg.IssueActions = []string{"create"} // only "create" announced
	client := newGitHubClient("")
	client.baseURL = ts.URL
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "o"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewRepoMonitor(cfg, client, poster, dir, nil)
	m.loadState()
	m.poll() // seed watermark 20

	// A closed issue and a new opened issue: only the opened one is wanted.
	stub.setEvents(`"e2"`, []string{openEvent("21"), closeEvent("22")})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "issue #") || !strings.Contains(msgs[0], "opened by") {
		t.Fatalf("messages = %v", msgs)
	}
}

func closeEvent(id string) string {
	return strings.Replace(eventJSON("IssuesEvent", id, "closed"), `"state":"open"`, `"state":"closed"`, 1)
}

func TestMonitorBatching(t *testing.T) {
	stub := &eventsStub{}
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	cfg := defaultRepo()
	client := newGitHubClient("")
	client.baseURL = ts.URL
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "o"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewRepoMonitor(cfg, client, poster, dir, nil)
	m.loadState()
	// Seed with one old event, then announce 20 new ones in one poll.
	stub.setEvents(`"e0"`, []string{openEvent("0")})
	m.poll()

	var list []string
	for i := 100; i < 120; i++ {
		list = append(list, openEvent(fmt.Sprintf("%d", i)))
	}
	stub.setEvents(`"e1"`, list)
	m.poll()

	msgs := poster.messages()
	if len(msgs) < 2 {
		t.Fatalf("expected multiple batched messages, got %d", len(msgs))
	}
	for _, msg := range msgs {
		if len(msg) > 400 {
			t.Errorf("message exceeds 400 bytes (%d): %q", len(msg), msg[:50])
		}
		if !strings.HasPrefix(msg, "[o/r] ") {
			t.Errorf("message missing repo prefix: %q", msg[:20])
		}
	}
}

func TestMonitorCommentEvent(t *testing.T) {
	stub := &eventsStub{}
	comment := `{"id":"77","type":"IssueCommentEvent","created_at":"2026-09-06T12:00:00Z",
		"actor":{"login":"zoe"},"payload":{"action":"created","issue":{
		"number":12,"title":"fix foo","html_url":"https://github.com/o/r/issues/12",
		"state":"open","user":{"login":"bob"}},"comment":{"body":"please\n take a look"}}}`
	stub.setEvents(`"e0"`, []string{comment})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultRepo(), poster)
	m.poll() // seed

	stub.setEvents(`"e1"`, []string{
		`{"id":"78","type":"IssueCommentEvent","created_at":"2026-09-06T12:01:00Z",
		"actor":{"login":"zoe"},"payload":{"action":"created","issue":{
		"number":12,"title":"fix foo","html_url":"https://github.com/o/r/issues/12",
		"state":"open","user":{"login":"bob"}},"comment":{"body":"please take a look"}}}`})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "zoe commented on issue #12: please take a look") {
		t.Fatalf("comment messages = %v", msgs)
	}
}

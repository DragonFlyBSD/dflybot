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

// openEventAt builds an "opened issue" event with a given creation time.
func openEventAt(id, at string) string {
	return eventJSONAt("IssuesEvent", id, "opened", at)
}

// commentAt builds an issue-comment event (small web-origin id pool).
func commentAt(id, at string) string {
	return fmt.Sprintf(`{"id":%q,"type":"IssueCommentEvent","created_at":%q,
		"actor":{"login":"zoe"},"payload":{"action":"created","issue":{
		"number":12,"title":"fix foo","html_url":"https://github.com/o/r/issues/12",
		"state":"open","user":{"login":"bob"}},"comment":{"body":"looks good"}}}`, id, at)
}

// deleteAt builds a repository DeleteEvent (large commit-origin id pool).
func deleteAt(id, at string) string {
	return fmt.Sprintf(`{"id":%q,"type":"DeleteEvent","created_at":%q,
		"actor":{"login":"bot"},"payload":{}}`, id, at)
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
	const (
		t0 = "2026-09-06T10:00:00Z"
		t1 = "2026-09-06T10:01:00Z"
	)
	stub := &eventsStub{}
	stub.setEvents(`"e1"`, []string{openEventAt("3", t0), openEventAt("2", t0), openEventAt("1", t0)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, dir := newTestMonitor(t, ts, defaultRepo(), poster)

	m.poll() // first run: seed, no announcements
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("seed announced: %v", got)
	}
	if st := readState(t, dir); st.LastEventAt != t0 {
		t.Fatalf("watermark = %q, want %q", st.LastEventAt, t0)
	}

	// No new events (304): nothing announced.
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("unchanged poll announced: %v", got)
	}

	// New events: announced and recorded in the history.
	stub.setEvents(`"e2"`, []string{openEventAt("5", t1), openEventAt("4", t1)})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[o/r] ") ||
		!strings.Contains(msgs[0], "issue #12 (fix foo) opened by aly") {
		t.Fatalf("messages = %v", msgs)
	}
	if st := readState(t, dir); st.LastEventAt != t1 {
		t.Errorf("watermark = %q, want %q", st.LastEventAt, t1)
	}
	hist, err := os.ReadFile(filepath.Join(dir, "o", "r.history"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(hist)), "\n") + 1; lines != 2 {
		t.Errorf("history lines = %d, want 2", lines)
	}
}

// TestMonitorMixedIDPools is the regression test for GitHub's two event id
// pools: a large commit-origin id (DeleteEvent) must never shadow later
// web-origin events (issue comments) that carry smaller ids.
func TestMonitorMixedIDPools(t *testing.T) {
	const (
		t0 = "2026-09-06T10:00:00Z"
		t1 = "2026-09-06T10:01:00Z"
		t2 = "2026-09-06T10:02:00Z"
	)
	stub := &eventsStub{}
	stub.setEvents(`"e0"`, []string{commentAt("14550000001", t0)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultRepo(), poster)
	m.poll() // seed silently

	// A DeleteEvent with a much larger id arrives together with a comment.
	stub.setEvents(`"e1"`, []string{deleteAt("20309194091", t1), commentAt("14550033122", t1)})
	m.poll()
	if got := poster.messages(); len(got) != 1 ||
		!strings.Contains(got[0], "commented on issue #12") {
		t.Fatalf("after delete+comment: %v", got)
	}

	// Later comments carry smaller ids than the DeleteEvent: they must still
	// be announced (watermark is time-based, not id-based).
	stub.setEvents(`"e2"`, []string{commentAt("14553206432", t2), commentAt("14551077752", t2)})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 2 {
		t.Fatalf("total messages = %d, want 2: %v", len(msgs), msgs)
	}
}

// TestMonitorEmptyRepoSeedsState covers a repo with no events: the state
// file must be (re)written as version 2 even when the events feed is empty,
// and the first real event that later appears must be announced (not
// consumed as a silent seed).
func TestMonitorEmptyRepoSeedsState(t *testing.T) {
	stub := &eventsStub{}
	stub.setEvents(`"e-new"`, nil) // no events
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
	// A stale v1 state file from the previous (id-watermark) format.
	oldState := `{"version":1,"etag":"\"old\"","last_event_id":0,"updated_at":123}`
	if err := os.WriteFile(filepath.Join(dir, "o", "r.state"), []byte(oldState), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewRepoMonitor(cfg, client, poster, dir, nil)
	m.loadState() // rejects the v1 file

	// First poll on the empty repo: seeds and rewrites the state as v2.
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("empty-repo poll announced: %v", got)
	}
	var st repoState
	exists, err := monitor.ReadJSON(filepath.Join(dir, "o", "r.state"), &st)
	if err != nil || !exists {
		t.Fatalf("state read: exists=%v err=%v", exists, err)
	}
	if st.Version != stateVersion {
		t.Fatalf("state version = %d, want %d", st.Version, stateVersion)
	}
	if st.ETag != `"e-new"` || st.LastEventAt != "" {
		t.Errorf("seeded-empty state = %+v", st)
	}

	// No changes: 304, nothing announced.
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("unchanged empty poll announced: %v", got)
	}

	// The first real event appears: it must be announced once.
	stub.setEvents(`"e2"`, []string{openEventAt("500", "2026-09-06T11:00:00Z")})
	m.poll()
	if got := poster.messages(); len(got) != 1 || !strings.Contains(got[0], "opened by") {
		t.Fatalf("first event messages = %v", got)
	}
	// And it must not be re-announced on a repeated poll.
	m.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("first event re-announced: %v", got)
	}
}

// TestMonitorSameSecondBoundary ensures two events sharing the watermark
// second are both announced, and not re-announced when redelivered.
func TestMonitorSameSecondBoundary(t *testing.T) {
	stub := &eventsStub{}
	const sameT = "2026-09-06T10:00:00Z"
	stub.setEvents(`"e0"`, []string{openEventAt("100", sameT)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, ts, defaultRepo(), poster)
	m.poll() // seed at sameT, boundary = {100}

	// A new event in the same second arrives with a changed etag.
	stub.setEvents(`"e1"`, []string{openEventAt("101", sameT), openEventAt("100", sameT)})
	m.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("same-second poll = %v", got)
	}
	// Redelivery of the same second's events must not re-announce.
	stub.setEvents(`"e2"`, []string{openEventAt("101", sameT), openEventAt("100", sameT)})
	m.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("redelivered same-second events re-announced: %v", got)
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

// reviewEvent builds a PullRequestReviewEvent as the Events API delivers it:
// an abbreviated pull_request plus the review with its body.
func reviewEvent(id, action, at, body string) string {
	return fmt.Sprintf(`{"id":%q,"type":"PullRequestReviewEvent","created_at":%q,
		"actor":{"login":"zoe"},"payload":{"action":%q,"review":{"id":9001,
		"body":%q,"state":"commented",
		"html_url":"https://github.com/o/r/pull/55#pullrequestreview-9001"},
		"pull_request":{"number":55,"url":"https://api.github.com/repos/o/r/pulls/55"}}}`,
		id, at, action, body)
}

// TestMonitorReviewCommentEvent covers a submitted PR review: GitHub emits
// a redundant "updated" event for the same review besides the "created"
// one, so the review body must be announced exactly once, like an issue
// comment.
func TestMonitorReviewCommentEvent(t *testing.T) {
	stub := &eventsStub{}
	stub.setEvents(`"e0"`, []string{openEventAt("0", "2026-09-12T05:00:00Z")})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, dir := newTestMonitor(t, ts, defaultRepo(), poster)
	m.poll() // seed

	stub.setEvents(`"e1"`, []string{
		reviewEvent("200", "created", "2026-09-12T05:57:43Z", "A bunch of minor suggestions. Thank you."),
		reviewEvent("199", "updated", "2026-09-12T05:57:41Z", "A bunch of minor suggestions. Thank you."),
	})
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0],
		"zoe commented on PR #55: A bunch of minor suggestions. Thank you.") {
		t.Fatalf("review messages = %v", msgs)
	}
	// A redelivery must not re-announce the review.
	m.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("review re-announced: %v", got)
	}
	hist, err := os.ReadFile(filepath.Join(dir, "o", "r.history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"kind":"PR","action":"comment"`,
		`"url":"https://github.com/o/r/pull/55#pullrequestreview-9001"`,
	} {
		if !strings.Contains(string(hist), want) {
			t.Errorf("history missing %q:\n%s", want, hist)
		}
	}
}

// eventsAndRefStub serves the repository events list as well as the full
// pull request referenced by the events' payload URL, mirroring the real
// Events API that abbreviates pull_request payloads.
type eventsAndRefStub struct {
	mu     sync.Mutex
	etag   string
	events []string // newest first; may embed "$BASE$"
	fullPR string   // full PR JSON for GET /pulls/1668
	refGot int      // number of full PR fetches
}

func (s *eventsAndRefStub) setEvents(etag string, list []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.etag = etag
	s.events = append([]string(nil), list...)
}

func (s *eventsAndRefStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			if r.Header.Get("If-None-Match") == s.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", s.etag)
			fmt.Fprint(w, "[")
			for i, e := range s.events {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprint(w, strings.ReplaceAll(e, "$BASE$", base))
			}
			fmt.Fprint(w, "]")
		case strings.HasSuffix(r.URL.Path, "/pulls/1668"):
			s.refGot++
			fmt.Fprint(w, strings.ReplaceAll(s.fullPR, "$BASE$", base))
		default:
			http.NotFound(w, r)
		}
	})
}

// TestMonitorPRDetailsAndMerge is the regression test for the two data
// problems of the Events API: pull_request payloads are abbreviated to
// url/id/number/head/base (so title and html_url must be fetched), and a
// merged PR arrives as its own event with action "merged" (which must be
// announced).  It also checks that creation announcements carry the URL
// and that the history records title and url.
func TestMonitorPRDetailsAndMerge(t *testing.T) {
	abbrevPR := `{"url":"$BASE$/repos/o/r/pulls/1668","id":4479651821,"number":1668,
		"head":{"ref":"agentic/x","sha":"111","repo":{"id":1}},
		"base":{"ref":"master","sha":"222","repo":{"id":1}}}`
	prEvent := func(id, action, at string) string {
		return fmt.Sprintf(`{"id":%q,"type":"PullRequestEvent","created_at":%q,
			"actor":{"login":"dragonflybot"},"payload":{"action":%q,"pull_request":%s}}`,
			id, at, action, abbrevPR)
	}
	fullPR := `{"url":"$BASE$/repos/o/r/pulls/1668","id":4479651821,"number":1668,
		"state":"closed","title":"devel/libcxx22: fix build failure on DragonFly",
		"html_url":"https://github.com/o/r/pull/1668","user":{"login":"dragonflybot"},
		"merged":true,"merged_at":"2026-09-08T23:59:54Z"}`

	stub := &eventsAndRefStub{fullPR: fullPR}
	stub.setEvents(`"e0"`, []string{openEventAt("0", "2026-09-08T23:00:00Z")})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, dir := newTestMonitor(t, ts, defaultRepo(), poster)
	m.poll() // seed silently

	stub.setEvents(`"e1"`, []string{
		prEvent("14672592472", "merged", "2026-09-08T23:59:54Z"), // newest first
		prEvent("14672578728", "opened", "2026-09-08T23:59:31Z"),
	})
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	for _, want := range []string{
		"PR #1668 (devel/libcxx22: fix build failure on DragonFly) opened by dragonflybot:" +
			" https://github.com/o/r/pull/1668",
		"PR #1668 (devel/libcxx22: fix build failure on DragonFly) merged by dragonflybot",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message missing %q:\n%s", want, msgs[0])
		}
	}
	if stub.refGot != 2 {
		t.Errorf("full PR fetches = %d, want 2", stub.refGot)
	}
	// The saved history carries title and url for both events.
	hist, err := os.ReadFile(filepath.Join(dir, "o", "r.history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"kind":"PR","action":"create"`, `"action":"merge"`,
		`"title":"devel/libcxx22: fix build failure on DragonFly"`,
		`"url":"https://github.com/o/r/pull/1668"`,
	} {
		if !strings.Contains(string(hist), want) {
			t.Errorf("history missing %q:\n%s", want, hist)
		}
	}
}

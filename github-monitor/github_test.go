// Copyright (c) 2026 Aaron LI
//
// Tests for the GitHub events client (stub server, no real GitHub).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func eventJSON(typ, id string, action string) string {
	return eventJSONAt(typ, id, action, "2026-09-06T12:00:00Z")
}

func eventJSONAt(typ, id, action, created string) string {
	// Minimal event JSON for the various types; ids exercised as strings.
	issue := `{"number":12,"title":"fix foo","html_url":"https://github.com/o/r/issues/12",
		"state":"open","user":{"login":"bob"}}`
	pr := `{"number":200,"title":"big change","html_url":"https://github.com/o/r/pull/200",
		"user":{"login":"carol"}}`
	var payload string
	switch typ {
	case "IssuesEvent":
		payload = `"action":"` + action + `","issue":` + issue
	case "PullRequestEvent":
		payload = `"action":"` + action + `","pull_request":` + pr
	case "IssueCommentEvent":
		payload = `"action":"created","issue":` + issue + `,"comment":{"body":"looks good"}`
	}
	if payload == "" {
		return ""
	}
	return `{"id":"` + id + `","type":"` + typ + `","created_at":"` + created + `",
		"actor":{"login":"aly"},"payload":{` + payload + `}}`
}

func decodeEvent(t *testing.T, raw string) *ghEvent {
	t.Helper()
	var e ghEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("decode event %q: %v", raw, err)
	}
	return &e
}

func TestEventIDTolerance(t *testing.T) {
	// id as a JSON string.
	e := decodeEvent(t, eventJSON("IssuesEvent", "9001", "opened"))
	if int64(e.ID) != 9001 {
		t.Errorf("id = %d", e.ID)
	}
	// id as a JSON number.
	var e2 ghEvent
	if err := json.Unmarshal([]byte(strings.Replace(
		eventJSON("IssuesEvent", "9002", "opened"), `"9002"`, "9002", 1)), &e2); err != nil {
		t.Fatal(err)
	}
	if int64(e2.ID) != 9002 {
		t.Errorf("numeric id = %d", e2.ID)
	}
}

func TestClassify(t *testing.T) {
	mon := NewRepoMonitor(&ConfigRepo{Project: "o", Repo: "r"}, nil, nil, t.TempDir(), nil)
	tests := []struct {
		raw        string
		wantAction string
		wantOK     bool
	}{
		{eventJSON("IssuesEvent", "1", "opened"), "create", true},
		{eventJSON("IssuesEvent", "2", "closed"), "close", true},
		{eventJSON("IssuesEvent", "3", "reopened"), "reopen", true},
		{eventJSON("IssuesEvent", "4", "labeled"), "", false}, // unsupported
		{eventJSON("PullRequestEvent", "5", "opened"), "create", true},
		{eventJSON("PullRequestEvent", "30", "merged"), "merge", true},
		{eventJSON("PullRequestEvent", "6", "synchronize"), "update", true},
		{eventJSON("PullRequestEvent", "7", "edited"), "update", true},
		{eventJSON("PullRequestEvent", "8", "closed"), "close", true},
		{eventJSON("PullRequestEvent", "9", "ready_for_review"), "", false},
		{eventJSON("IssueCommentEvent", "10", "created"), "comment", true},
		{`{"type":"PushEvent","id":"11","payload":{}}`, "", false},
	}
	for _, tt := range tests {
		a, ok := mon.classify(decodeEvent(t, tt.raw))
		if ok != tt.wantOK {
			t.Errorf("%s: ok=%v, want %v", tt.raw[:30], ok, tt.wantOK)
			continue
		}
		if ok && a.action != tt.wantAction {
			t.Errorf("%s: action=%s, want %s", tt.raw[:30], a.action, tt.wantAction)
		}
	}
}

func TestActionWanted(t *testing.T) {
	tests := []struct {
		kind, action string
		issues       []string
		pulls        []string
		want         bool
	}{
		{activityIssue, "create", []string{"create", "comment", "close"}, []string{}, true},
		{activityIssue, "close", []string{"create", "comment", "close"}, []string{}, true},
		{activityIssue, "reopen", []string{"create", "comment", "close"}, []string{}, false},
		{activityIssue, "reopen", nil, nil, true}, // empty means all
		{activityPR, "merge", nil, []string{"create", "close"}, false},
		{activityPR, "update", nil, nil, true},
		{activityPR, "merge", nil, nil, true},
	}
	for _, tt := range tests {
		if got := actionWanted(tt.kind, tt.action, tt.issues, tt.pulls); got != tt.want {
			t.Errorf("actionWanted(%s,%s,%v,%v) = %v, want %v",
				tt.kind, tt.action, tt.issues, tt.pulls, got, tt.want)
		}
	}
}

func TestGetRef(t *testing.T) {
	pr := `{"url":"https://api.github.com/repos/o/r/pulls/9","id":1,"number":9,
		"state":"open","title":"fix foo","html_url":"https://github.com/o/r/pull/9",
		"user":{"login":"bob"}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(pr))
	}))
	defer ts.Close()

	c := newGitHubClient("tok")
	ref, err := c.getRef(ts.URL + "/pulls/9")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Number != 9 || ref.Title != "fix foo" ||
		ref.HtmlUrl != "https://github.com/o/r/pull/9" {
		t.Errorf("ref = %+v", ref)
	}
}

func TestGetRefError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	c := newGitHubClient("")
	if _, err := c.getRef(ts.URL + "/pulls/404"); err == nil {
		t.Error("expected an error for a 404 reply")
	}
}

func TestFetchEventsAndETag(t *testing.T) {
	page := "[" + eventJSON("IssuesEvent", "100", "opened") + "]"
	var ifNone string
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		ifNone = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"abc"`)
		if ifNone == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(page))
	}))
	defer ts.Close()

	c := newGitHubClient("tok")
	c.baseURL = ts.URL
	events, _, modified, err := c.fetchEvents("o", "r", "", time.Time{})
	if err != nil || !modified || len(events) != 1 || int64(events[0].ID) != 100 {
		t.Fatalf("first fetch = %d events, modified=%v, err=%v", len(events), modified, err)
	}
	// Second fetch with the etag: 304, nothing new.
	events2, etag2, modified2, err := c.fetchEvents("o", "r", `"abc"`, time.Time{})
	if err != nil || modified2 || len(events2) != 0 || etag2 == "" {
		t.Fatalf("304 fetch = %v events, modified=%v, etag=%q, err=%v", len(events2), modified2, etag2, err)
	}
	if calls != 2 {
		t.Errorf("calls = %d", calls)
	}
}

func TestFetchEventsPaging(t *testing.T) {
	p1 := "[" + eventJSON("IssuesEvent", "201", "opened") + "]"
	p2 := "[" + eventJSON("IssuesEvent", "200", "closed") + "]"
	page2 := false
	var nextURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "page=2") {
			page2 = true
			w.Write([]byte(p2))
			return
		}
		w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
		w.Write([]byte(p1))
	}))
	defer ts.Close()
	nextURL = ts.URL + "/x?per_page=100&page=2"

	c := newGitHubClient("")
	c.baseURL = ts.URL
	events, _, _, err := c.fetchEvents("o", "r", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (page2 fetched=%v)", len(events), page2)
	}
}

func TestFetchEventsStopsAtWatermark(t *testing.T) {
	// Page 1 has a link to page 2, but its only event is already older than
	// the watermark: page 2 must not be fetched.
	p1 := "[" + eventJSONAt("IssuesEvent", "300", "opened", "2026-09-06T10:00:00Z") + "]"
	page2 := false
	var nextURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "page=2") {
			page2 = true
			w.Write([]byte("[]"))
			return
		}
		w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
		w.Write([]byte(p1))
	}))
	defer ts.Close()
	nextURL = ts.URL + "/x?per_page=100&page=2"

	c := newGitHubClient("")
	c.baseURL = ts.URL
	since := time.Date(2026, 9, 6, 10, 0, 1, 0, time.UTC) // later than the event
	events, _, _, err := c.fetchEvents("o", "r", "", since)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", len(events), err)
	}
	if page2 {
		t.Error("should not fetch page 2 past the watermark")
	}
}

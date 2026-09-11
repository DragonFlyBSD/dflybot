// Copyright (c) 2026 Aaron LI
//
// Tests for the Redmine Atom feed client (stub server, no real Redmine).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// xmlEscape escapes s for use as XML text/attribute content.
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		panic(err)
	}
	return b.String()
}

// testEntry builds one Atom entry with an alternate link matching its id.
func testEntry(id, title, updated, author, content string) string {
	return fmt.Sprintf(
		`<entry><title>%s</title><link rel="alternate" href="%s"/><id>%s</id>`+
			`<updated>%s</updated><author><name>%s</name></author>`+
			`<content type="html">%s</content></entry>`,
		xmlEscape(title), xmlEscape(id), xmlEscape(id),
		updated, xmlEscape(author), xmlEscape(content))
}

// testFeed wraps entries into a minimal Atom document.
func testFeed(entries ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<feed xmlns="http://www.w3.org/2005/Atom">` +
		strings.Join(entries, "") + `</feed>`
}

func TestParseTitle(t *testing.T) {
	tests := []struct {
		in   string
		want issueTitle
		ok   bool
	}{
		{"DragonFlyBSD - Bug #3414 (Resolved): /rescue/awk is broken",
			issueTitle{"Bug", 3414, "Resolved", "/rescue/awk is broken"}, true},
		{"DragonFlyBSD - Bug #3392: dm_target_crypt causes random but serious hangs",
			issueTitle{"Bug", 3392, "", "dm_target_crypt causes random but serious hangs"}, true},
		{"DragonFlyBSD - Submit #3411 (New): update tzdata/zoneinfo",
			issueTitle{"Submit", 3411, "New", "update tzdata/zoneinfo"}, true},
		{"garbage without an issue", issueTitle{}, false},
		{"", issueTitle{}, false},
	}
	for _, tt := range tests {
		got, ok := parseTitle(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseTitle(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestEntryClassification(t *testing.T) {
	creation := &atomEntry{ID: "https://bugs.example.org/issues/3420",
		Links: []atomLink{{Rel: "alternate", Href: "https://bugs.example.org/issues/3420"}}}
	journal := &atomEntry{ID: "https://bugs.example.org/issues/3414#change-14693",
		Links: []atomLink{{Rel: "alternate", Href: "https://bugs.example.org/issues/3414#change-14693"}}}

	if !creation.isCreation() || creation.journalID() != "" {
		t.Errorf("creation misclassified: creation=%v journal=%q", creation.isCreation(), creation.journalID())
	}
	if journal.isCreation() || journal.journalID() != "change-14693" {
		t.Errorf("journal misclassified: creation=%v journal=%q", journal.isCreation(), journal.journalID())
	}
	if got := journal.issueURL(); got != "https://bugs.example.org/issues/3414" {
		t.Errorf("journal issueURL = %q", got)
	}
}

func TestHTMLText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"<p>Hello <b>world</b></p>", "Hello world"},
		{"<p>a &amp; b</p>", "a & b"},
		{"<p>line one<br/>line two</p>", "line one line two"},
	}
	for _, tt := range tests {
		if got := htmlText(tt.in, 120); got != tt.want {
			t.Errorf("htmlText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedact(t *testing.T) {
	in := `Get "https://bugs.example.org/activity.atom?key=SECRET123&x=1": dial tcp`
	got := redact(in)
	if strings.Contains(got, "SECRET123") || !strings.Contains(got, "key=REDACTED") {
		t.Errorf("redact(%q) = %q", in, got)
	}
}

func TestFetchAndETag(t *testing.T) {
	feed := testFeed(testEntry("https://bugs.example.org/issues/1",
		"DragonFlyBSD - Bug #1 (New): hello", "2026-09-10T15:00:00Z", "alice", "<p>body</p>"))
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		io.WriteString(w, feed)
	}))
	defer ts.Close()

	c := newAtomClient()
	entries, etag, modified, err := c.fetch(ts.URL+"/activity.atom?key=SECRET", "")
	if err != nil || !modified || len(entries) != 1 || etag != `"v1"` {
		t.Fatalf("first fetch: entries=%d etag=%q modified=%v err=%v", len(entries), etag, modified, err)
	}
	if entries[0].Author.Name != "alice" || entries[0].time().IsZero() {
		t.Errorf("entry not decoded: %+v", entries[0])
	}

	entries2, _, modified2, err := c.fetch(ts.URL+"/activity.atom?key=SECRET", etag)
	if err != nil || modified2 || len(entries2) != 0 {
		t.Fatalf("second fetch: entries=%d modified=%v err=%v", len(entries2), modified2, err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestFetchAnubis(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head>`+
			`<title>Making sure you&#39;re not a bot!</title></head>`+
			`<body>anubis challenge</body></html>`)
	}))
	defer ts.Close()

	_, _, _, err := newAtomClient().fetch(ts.URL, "")
	if !errors.Is(err, errAnubis) {
		t.Fatalf("err = %v, want errAnubis", err)
	}
}

func TestFetchUnexpectedContentType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><body>maintenance</body></html>`)
	}))
	defer ts.Close()

	_, _, _, err := newAtomClient().fetch(ts.URL, "")
	if err == nil || errors.Is(err, errAnubis) {
		t.Fatalf("err = %v, want a non-Anubis error", err)
	}
}

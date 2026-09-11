// Copyright (c) 2026 Aaron LI
//
// Redmine Atom feed client: fetch a project activity feed with ETag
// support, parse its entries, and render the HTML note/description as
// plain text for announcements.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

const (
	// userAgent is deliberately not browser-like: the site is fronted by
	// the Anubis anti-bot proxy, which challenges browser user agents and
	// answers with an HTML page instead of the feed.
	userAgent = "dflybot-redmine-monitor"
	// feedLimit mirrors Redmine's Setting.feeds_limit (default 15): the
	// activity feed returns at most this many entries.
	feedLimit = 15
)

// errAnubis marks a reply intercepted by Anubis: HTTP 200 with an HTML
// challenge page instead of the Atom feed.
var errAnubis = errors.New("blocked by Anubis")

// atomFeed is the subset of the Atom document used by the monitor.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Author  atomAuthor  `xml:"author"`
	Content atomContent `xml:"content"`
	Links   []atomLink  `xml:"link"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

func (e *atomEntry) time() time.Time {
	t, err := time.Parse(time.RFC3339, e.Updated)
	if err != nil {
		return time.Time{}
	}
	return t
}

// journalID returns the "change-<n>" journal id of a journal entry, or ""
// for a creation entry.
func (e *atomEntry) journalID() string {
	if i := strings.Index(e.ID, "#change-"); i >= 0 {
		return e.ID[i+1:]
	}
	return ""
}

// isCreation reports whether the entry is an issue-creation activity; a
// journal entry carries a "#change-<journalID>" fragment in its id.
func (e *atomEntry) isCreation() bool {
	return strings.Contains(e.ID, "/issues/") && e.journalID() == ""
}

// issueURL returns the issue's HTML URL (without a journal fragment).
func (e *atomEntry) issueURL() string {
	u := ""
	for _, l := range e.Links {
		if l.Rel == "alternate" {
			u = l.Href
			break
		}
	}
	if u == "" {
		u = e.ID
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	return u
}

// issueTitle is the parsed "<project> - <tracker> #<number> [(<status>)]:
// <subject>" activity title.
type issueTitle struct {
	Tracker string
	Number  int64
	Status  string
	Subject string
}

var titleRe = regexp.MustCompile(`^(.*?) #(\d+)(?: \(([^)]*)\))?: (.*)$`)

func parseTitle(s string) (issueTitle, bool) {
	m := titleRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return issueTitle{}, false
	}
	tracker := strings.TrimSpace(m[1])
	if i := strings.LastIndex(tracker, " - "); i >= 0 {
		tracker = strings.TrimSpace(tracker[i+3:])
	}
	number, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return issueTitle{}, false
	}
	return issueTitle{
		Tracker: tracker,
		Number:  number,
		Status:  strings.TrimSpace(m[3]),
		Subject: strings.TrimSpace(m[4]),
	}, true
}

// atomClient fetches and parses Redmine Atom feeds.
type atomClient struct {
	client *http.Client
}

func newAtomClient() *atomClient {
	return &atomClient{client: &http.Client{Timeout: 30 * time.Second}}
}

// fetch retrieves the feed.  It returns the entries, the new ETag, and
// whether the feed changed (false on 304 Not Modified).  errAnubis is
// returned when Anubis answers with its HTML challenge instead of the feed.
func (c *atomClient) fetch(feedURL, etag string) ([]atomEntry, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/atom+xml, application/xml;q=0.9")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, etag, false, nil
	case http.StatusOK:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, "", false, fmt.Errorf("http status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Anubis replies with HTTP 200 but an HTML challenge page; a content
	// type check is the only reliable way to tell it apart from the feed.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "xml") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if isAnubis(body) {
			return nil, "", false, errAnubis
		}
		return nil, "", false, fmt.Errorf("unexpected content type %q", ct)
	}

	var feed atomFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, "", false, fmt.Errorf("decode feed: %w", err)
	}
	return feed.Entries, resp.Header.Get("ETag"), true, nil
}

// isAnubis reports whether body looks like the Anubis challenge page.
func isAnubis(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "anubis") || strings.Contains(s, "not a bot")
}

// htmlText renders the HTML note/description of an entry as one line of
// plain text, shortened to at most max runes.
func htmlText(s string, max int) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	doc, err := xhtml.Parse(strings.NewReader(s))
	if err != nil {
		return snippet(html.UnescapeString(stripTags(s)), max)
	}
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	return snippet(b.String(), max)
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

func stripTags(s string) string {
	return tagRe.ReplaceAllString(s, " ")
}

var keyRe = regexp.MustCompile(`(?i)([?&]key=)[^&\s"']*`)

// redact removes the Redmine API key from a URL or error message before it
// is logged.
func redact(s string) string {
	return keyRe.ReplaceAllString(s, "${1}REDACTED")
}

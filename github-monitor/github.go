// Copyright (c) 2026 Aaron LI
//
// GitHub REST API client (read-only) for the repository Events API.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	githubAPI = "https://api.github.com"
	userAgent = "dflybot-github-monitor"
	pageSize  = 100 // events per page
	maxPages  = 10  // page cap per poll
)

// eventID is a tolerant GitHub event id, which may be serialized as a JSON
// number or as a string.
type eventID int64

func (e *eventID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*e = eventID(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("bad event id %s: %w", b, err)
	}
	*e = eventID(n)
	return nil
}

// ghEvent is the interesting subset of a GitHub repository event.
type ghEvent struct {
	ID        eventID   `json:"id"`
	Type      string    `json:"type"`
	CreatedAt string    `json:"created_at"`
	Actor     ghActor   `json:"actor"`
	Payload   ghPayload `json:"payload"`
}

type ghActor struct {
	Login string `json:"login"`
}

type ghPayload struct {
	Action  string     `json:"action"`
	Issue   *ghRef     `json:"issue"`
	PR      *ghRef     `json:"pull_request"`
	Comment *ghComment `json:"comment"`
}

// ghRef is a referenced issue or pull request.
type ghRef struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HtmlUrl string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Merged      bool            `json:"merged"`
	MergedAt    string          `json:"merged_at"`
	PullRequest json.RawMessage `json:"pull_request"` // non-null when a PR
}

// IsPR reports whether the referenced item is a pull request (an issue can
// be a pull request in the issues/comments payloads).
func (r *ghRef) IsPR() bool {
	return len(r.PullRequest) > 0 && string(r.PullRequest) != "null"
}

// IsMerged reports whether a closed pull request was merged.
func (r *ghRef) IsMerged() bool {
	return r.Merged || r.MergedAt != ""
}

type ghComment struct {
	Body string `json:"body"`
}

type githubClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newGitHubClient(token string) *githubClient {
	return &githubClient{
		baseURL: githubAPI,
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// fetchEvents polls the repository events newer than the sinceID watermark.
// It returns the events (newest first), the new ETag, and whether the list
// changed (false on a 304 Not Modified reply).  The watermark is used both
// to stop paging early and by the caller to deduplicate.
func (c *githubClient) fetchEvents(project, repo, etag string, sinceID eventID) ([]ghEvent, string, bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/events?per_page=%d", c.baseURL, project, repo, pageSize)
	var events []ghEvent
	newEtag := ""
	for page := 1; page <= maxPages; page++ {
		reqEtag := ""
		if page == 1 {
			reqEtag = etag // only the first page is ETag-conditional
		}
		got, next, changed, err := c.getEventsPage(url, reqEtag)
		if err != nil {
			return nil, "", false, err
		}
		if page == 1 {
			if !changed {
				return nil, etag, false, nil // 304 Not Modified
			}
			newEtag = got.etag
		}
		events = append(events, got.events...)
		// Stop paging once an event not newer than the watermark is reached
		// (pages are newest first, so older pages cannot help).
		if next == "" {
			break
		}
		if n := len(got.events); n > 0 && got.events[n-1].ID <= sinceID {
			break
		}
		url = next
	}
	return events, newEtag, true, nil
}

type eventsPage struct {
	events []ghEvent
	etag   string
}

func (c *githubClient) getEventsPage(url, etag string) (eventsPage, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return eventsPage{}, "", false, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return eventsPage{}, "", false, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return eventsPage{}, "", false, nil
	case resp.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err = fmt.Errorf("github: http status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
		return eventsPage{}, "", false, err
	}

	var events []ghEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return eventsPage{}, "", false, fmt.Errorf("github: decode events: %w", err)
	}
	return eventsPage{events: events, etag: resp.Header.Get("ETag")},
		linkNext(resp.Header.Get("Link")), true, nil
}

// linkNext extracts the URL of the "next" page from a Link header.
func linkNext(link string) string {
	for _, part := range strings.Split(link, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 || !strings.Contains(seg[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(seg[0]), "<>")
	}
	return ""
}

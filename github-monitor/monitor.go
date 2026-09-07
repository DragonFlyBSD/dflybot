// Copyright (c) 2026 Aaron LI
//
// GitHub repo monitor: poll the events of one repo and announce the
// configured issue/PR actions, batched per poll, via the webhook.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liweitianux/dflybot/monitor"
)

const (
	stateVersion = 1
	titleMax     = 100 // runes
	commentMax   = 120 // runes

	activityIssue = "issue"
	activityPR    = "PR"
)

// Supported actions, per kind.
var (
	defaultIssueActions = []string{"create", "comment", "close", "reopen"}
	defaultPRActions    = []string{"create", "comment", "update", "close", "merge", "reopen"}
)

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// actionWanted reports whether the action should be announced for the
// given kind, given the per-repo config lists (empty means all).
func actionWanted(kind, action string, issueActions, prActions []string) bool {
	var list, all []string
	switch kind {
	case activityIssue:
		list, all = issueActions, defaultIssueActions
	case activityPR:
		list, all = prActions, defaultPRActions
	default:
		return false
	}
	if len(list) == 0 {
		return contains(all, action)
	}
	return contains(list, action)
}

// activity is one announced event.
type activity struct {
	kind    string // "issue" | "PR"
	action  string // create|comment|close|merge|update|reopen
	number  int64
	actor   string
	title   string // issue/PR title, or comment snippet for comments
	url     string
	eventID int64
}

// line renders the announcement text (without the repo prefix).
func (a *activity) line() string {
	verb := map[string]string{
		"create": "opened",
		"close":  "closed",
		"reopen": "reopened",
		"merge":  "merged",
		"update": "updated",
	}[a.action]
	if verb == "" {
		verb = a.action
	}

	if a.action == "comment" {
		return fmt.Sprintf("%s commented on %s #%d: %s", a.actor, a.kind, a.number, a.title)
	}
	return fmt.Sprintf("%s #%d (%s) %s by %s", a.kind, a.number, a.title, verb, a.actor)
}

// ghHistoryLine is one line of the .history JSONL file.
type ghHistoryLine struct {
	Timestamp time.Time `json:"ts"`
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action"`
	Number    int64     `json:"number"`
	Actor     string    `json:"actor"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
}

func (a *activity) history() ghHistoryLine {
	return ghHistoryLine{Kind: a.kind, Action: a.action, Number: a.number,
		Actor: a.actor, Title: a.title, URL: a.url}
}

// repoState is the on-disk state of one repo.
type repoState struct {
	Version     int    `json:"version"`
	ETag        string `json:"etag,omitempty"`
	LastEventID int64  `json:"last_event_id"`
	UpdatedAt   int64  `json:"updated_at"`
}

type RepoMonitor struct {
	cfg    *ConfigRepo
	github *githubClient
	poster monitor.Poster
	logger *slog.Logger

	statePath   string
	historyPath string
	history     *monitor.History

	state repoState
}

func NewRepoMonitor(cfg *ConfigRepo, gh *githubClient, poster monitor.Poster,
	dataDir string, base *slog.Logger) *RepoMonitor {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("project", cfg.Project), slog.String("repo", cfg.Repo))
	return &RepoMonitor{
		cfg:         cfg,
		github:      gh,
		poster:      poster,
		logger:      logger,
		statePath:   filepath.Join(dataDir, cfg.Project, cfg.Repo+".state"),
		historyPath: filepath.Join(dataDir, cfg.Project, cfg.Repo+".history"),
		history:     monitor.NewHistory(filepath.Join(dataDir, cfg.Project, cfg.Repo+".history")),
		state:       repoState{Version: stateVersion},
	}
}

func (m *RepoMonitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		if err := m.history.Close(); err != nil {
			m.logger.Warn("history close failure", "error", err)
		}
		m.saveState()
		wg.Done()
	}()

	dir := filepath.Dir(m.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logger.Error("repo dir creation failed", "dir", dir, "error", err)
		return
	}

	m.loadState()
	m.logger.Info("github repo monitor started",
		"interval", m.cfg.Interval, "last_event_id", m.state.LastEventID)

	monitor.Loop(ctx, time.Duration(m.cfg.Interval)*time.Second, m.poll)
	m.logger.Debug("repo monitor exiting")
}

// poll checks the repo events once and announces the new ones.
func (m *RepoMonitor) poll() {
	now := time.Now()
	events, etag, modified, err := m.github.fetchEvents(
		m.cfg.Project, m.cfg.Repo, m.state.ETag, eventID(m.state.LastEventID))
	if err != nil {
		m.logger.Warn("events fetch failed", "error", err)
		return // retry next poll; keep the previous state
	}
	m.logger.Debug("events fetched", "count", len(events), "etag", etag, "modified", modified)
	if !modified {
		return // nothing changed (304); state stays as-is
	}

	// First run: seed the watermark silently (no announcements).  The etag
	// (always present on a successful reply) tells the first run apart from
	// a restarted monitor whose watermark happens to still be 0.
	if m.state.LastEventID == 0 && m.state.ETag == "" {
		m.state.ETag = etag
		m.state.UpdatedAt = now.Unix()
		for _, e := range events {
			if int64(e.ID) > m.state.LastEventID {
				m.state.LastEventID = int64(e.ID)
			}
		}
		m.saveState()
		m.logger.Info("seeded watermark", "last_event_id", m.state.LastEventID)
		return
	}

	// Collect the new interesting events in chronological order.
	var acts []activity
	for i := len(events) - 1; i >= 0; i-- {
		e := &events[i]
		if int64(e.ID) <= m.state.LastEventID {
			continue
		}
		if a, ok := m.classify(e); ok {
			acts = append(acts, *a)
		}
	}
	// Advance the watermark past everything the API returned.
	m.state.LastEventID = int64(events[0].ID)
	m.state.ETag = etag
	m.state.UpdatedAt = now.Unix()

	m.logger.Debug("activities classified", "count", len(acts))
	if len(acts) > 0 {
		m.announce(acts)
		for i := range acts {
			h := acts[i].history()
			h.Timestamp = now.UTC()
			h.ID = acts[i].eventID
			if err := m.history.Append(h); err != nil {
				m.logger.Error("history append failure", "error", err)
			}
		}
	}
	if err := m.history.Flush(); err != nil {
		m.logger.Error("history flush failure", "path", m.historyPath, "error", err)
	}
	m.saveState()
}

// classify maps an event to an activity, honoring the configured actions.
func (m *RepoMonitor) classify(e *ghEvent) (*activity, bool) {
	var a *activity
	switch e.Type {
	case "IssuesEvent":
		ref := e.Payload.Issue
		if ref == nil {
			return nil, false
		}
		action := map[string]string{
			"opened":   "create",
			"closed":   "close",
			"reopened": "reopen",
		}[e.Payload.Action]
		if action == "" {
			return nil, false
		}
		a = &activity{
			kind:    activityIssue,
			action:  action,
			number:  ref.Number,
			actor:   e.Actor.Login,
			title:   snippet(ref.Title, titleMax),
			url:     ref.HtmlUrl,
			eventID: int64(e.ID),
		}
	case "PullRequestEvent":
		ref := e.Payload.PR
		if ref == nil {
			return nil, false
		}
		var action string
		switch e.Payload.Action {
		case "opened":
			action = "create"
		case "closed":
			if ref.IsMerged() {
				action = "merge"
			} else {
				action = "close"
			}
		case "reopened":
			action = "reopen"
		case "synchronize", "edited":
			action = "update"
		}
		if action == "" {
			return nil, false
		}
		a = &activity{
			kind:    activityPR,
			action:  action,
			number:  ref.Number,
			actor:   e.Actor.Login,
			title:   snippet(ref.Title, titleMax),
			url:     ref.HtmlUrl,
			eventID: int64(e.ID),
		}
	case "IssueCommentEvent":
		ref := e.Payload.Issue
		if ref == nil || e.Payload.Comment == nil {
			return nil, false
		}
		kind := activityIssue
		if ref.IsPR() {
			kind = activityPR
		}
		a = &activity{
			kind:    kind,
			action:  "comment",
			number:  ref.Number,
			actor:   e.Actor.Login,
			title:   snippet(e.Payload.Comment.Body, commentMax),
			url:     ref.HtmlUrl,
			eventID: int64(e.ID),
		}
	default:
		return nil, false
	}
	if !actionWanted(a.kind, a.action, m.cfg.IssueActions, m.cfg.PRActions) {
		return nil, false
	}
	return a, true
}

// announce posts the activities batched by repo, split at the poster's
// length limit.
func (m *RepoMonitor) announce(acts []activity) {
	lines := make([]string, len(acts))
	for i, a := range acts {
		lines[i] = a.line()
	}
	prompt := "[" + m.cfg.Project + "/" + m.cfg.Repo + "] "
	maxLen := m.poster.GetMaxLength() - len(prompt)
	const sep = " || "

	texts := make([]string, 0, len(acts))
	curLen := 0
	flush := func() {
		if len(texts) > 0 {
			m.post(prompt + strings.Join(texts, sep))
			texts = texts[:0]
			curLen = 0
		}
	}
	for _, line := range lines {
		sepLen := 0
		if len(texts) > 0 {
			sepLen = len(sep)
		}
		if curLen+sepLen+len(line) <= maxLen {
			texts = append(texts, line)
			curLen += sepLen + len(line)
			continue
		}
		flush()
		texts = append(texts, line)
		curLen = len(line)
	}
	flush()
}

func (m *RepoMonitor) post(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.poster.Post(ctx, text); err != nil {
		m.logger.Error("announce failed", "error", err)
	}
}

// Persistence.

func (m *RepoMonitor) loadState() {
	var st repoState
	exists, err := monitor.ReadJSON(m.statePath, &st)
	if err != nil {
		m.logger.Error("state file read failure", "path", m.statePath, "error", err)
		return
	}
	if !exists {
		m.logger.Debug("state file not exist", "path", m.statePath)
		return
	}
	if st.Version != stateVersion {
		m.logger.Warn("state file version unsupported; starting fresh",
			"version", st.Version, "path", m.statePath)
		return
	}
	m.state = st
	m.logger.Debug("state loaded", "path", m.statePath)
}

func (m *RepoMonitor) saveState() {
	m.state.UpdatedAt = time.Now().Unix()
	if err := monitor.SaveJSON(m.statePath, &m.state); err != nil {
		m.logger.Error("state file save failure", "path", m.statePath, "error", err)
		return
	}
	m.logger.Debug("state saved", "path", m.statePath)
}

// snippet is the shortened, cleaned text used for announcements.
func snippet(s string, max int) string {
	// Replace newlines/tabs with spaces and collapses whitespace runs.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ")

	// Shorten to at most max runes.
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	for len(s) > 0 && utf8.RuneCountInString(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s + "…"
}

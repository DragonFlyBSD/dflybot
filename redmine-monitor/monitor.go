// Copyright (c) 2026 Aaron LI
//
// Redmine monitor: poll the activity Atom feed of one project, announce the
// configured issue actions (create/comment/close/resolve/reopen/update),
// and persist the per-project state and history.
//
// Announcement rules:
//   - Issue creation is announced with the issue URL.
//   - A journal that changes the status is announced according to the new
//     status (resolve/close); a transition from a closed status to an open
//     one is announced as a reopen.  The journal note, when present, is
//     appended.
//   - A journal with only a note is announced as a comment.
//   - Other journals (assignee, category, description, ...) are announced as
//     updates, with the note when present.
//   - The first run seeds the watermark and the per-issue status silently
//     (based on the activity feed); nothing is announced for the backlog.
//
// The activity feed returns at most Redmine's feeds_limit entries (15 by
// default).  A full page whose oldest entry is still newer than the
// watermark means activities were missed; this is logged as a warning but
// cannot be recovered from the feed (its page parameter is ignored).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liweitianux/dflybot/monitor"
)

const (
	stateVersion = 1
	// anubisWarnAfter is the number of consecutive Anubis blocks after
	// which a single warning is announced for the block streak.
	anubisWarnAfter = 3

	subjectMax = 100 // runes
	noteMax    = 120 // runes
)

// supportedActions are the announcement actions selectable per project.
var supportedActions = []string{"create", "comment", "close", "resolve", "reopen", "update"}

// closedStatuses are the Redmine statuses from which a transition to an open
// status counts as a reopen.
var closedStatuses = map[string]bool{"Closed": true, "Resolved": true, "Rejected": true}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// actionWanted reports whether action should be announced given the
// per-project filter (empty means all supported actions).
func actionWanted(action string, list []string) bool {
	if len(list) == 0 {
		return contains(supportedActions, action)
	}
	return contains(list, action)
}

// activity is one announced activity.
type activity struct {
	action  string
	number  int64
	actor   string
	tracker string
	subject string
	note    string
	status  string
	url     string
	entryID string
	at      time.Time
}

// line renders the announcement text (without the project prefix).
func (a *activity) line() string {
	verb := map[string]string{
		"create":  "created",
		"comment": "commented",
		"close":   "closed",
		"resolve": "resolved",
		"reopen":  "reopened",
		"update":  "updated",
	}[a.action]

	head := fmt.Sprintf("issue #%d", a.number)
	detail := a.subject
	if a.tracker != "" {
		detail = a.tracker + ": " + a.subject
	}
	if detail != "" {
		head += " (" + detail + ")"
	}
	head += " " + verb + " by " + a.actor
	if a.note != "" {
		head += ": " + a.note
	}
	if a.action == "create" && a.url != "" {
		head += ": " + a.url
	}
	return head
}

func (a *activity) history() historyLine {
	return historyLine{
		ID:      a.entryID,
		Action:  a.action,
		Number:  a.number,
		Actor:   a.actor,
		Tracker: a.tracker,
		Subject: a.subject,
		Note:    a.note,
		Status:  a.status,
		URL:     a.url,
	}
}

// historyLine is one line of the .history JSONL file.
type historyLine struct {
	Timestamp time.Time `json:"ts"`
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Number    int64     `json:"number"`
	Actor     string    `json:"actor"`
	Tracker   string    `json:"tracker,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Note      string    `json:"note,omitempty"`
	Status    string    `json:"status,omitempty"`
	URL       string    `json:"url,omitempty"`
}

// issueState is the persisted last-known state of one issue.
type issueState struct {
	Tracker string `json:"tracker,omitempty"`
	Subject string `json:"subject,omitempty"`
	Status  string `json:"status,omitempty"`
}

// projState is the on-disk state of one project.
type projState struct {
	Version int    `json:"version"`
	ETag    string `json:"etag,omitempty"`
	// LastAt is the time (RFC3339 UTC) of the newest processed activity;
	// LastIDs are the entry ids processed at exactly that time (activities
	// have second granularity and may share a second).
	LastAt  string                 `json:"last_at,omitempty"`
	LastIDs []string               `json:"last_ids,omitempty"`
	Issues  map[string]*issueState `json:"issues,omitempty"`
	// AnubisStreak counts consecutive Anubis blocks; AnubisWarned records
	// that the warning was already announced for the current streak.
	AnubisStreak int   `json:"anubis_streak,omitempty"`
	AnubisWarned bool  `json:"anubis_warned,omitempty"`
	UpdatedAt    int64 `json:"updated_at"`
}

func (st *projState) lastAt() time.Time {
	t, err := time.Parse(time.RFC3339, st.LastAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// seenAtBoundary reports whether the entry id was already processed at the
// watermark's exact second.
func (st *projState) seenAtBoundary(id string) bool {
	for _, v := range st.LastIDs {
		if v == id {
			return true
		}
	}
	return false
}

// setLast records the new watermark and the entry ids processed at exactly
// that time.
func (st *projState) setLast(at time.Time, ids []string) {
	st.LastAt = at.UTC().Format(time.RFC3339)
	st.LastIDs = ids
}

type ProjectMonitor struct {
	cfg    *ConfigProject
	client *atomClient
	poster monitor.Poster
	logger *slog.Logger

	statePath   string
	historyPath string
	history     *monitor.History

	state projState
}

func NewProjectMonitor(cfg *ConfigProject, client *atomClient, poster monitor.Poster,
	dataDir string, base *slog.Logger) *ProjectMonitor {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("project", cfg.Name))
	return &ProjectMonitor{
		cfg:         cfg,
		client:      client,
		poster:      poster,
		logger:      logger,
		statePath:   filepath.Join(dataDir, cfg.Name+".state"),
		historyPath: filepath.Join(dataDir, cfg.Name+".history"),
		history:     monitor.NewHistory(filepath.Join(dataDir, cfg.Name+".history")),
		state: projState{
			Version: stateVersion,
			Issues:  make(map[string]*issueState),
		},
	}
}

func (m *ProjectMonitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		if err := m.history.Close(); err != nil {
			m.logger.Warn("history close failure", "error", err)
		}
		m.saveState()
		wg.Done()
	}()

	dir := filepath.Dir(m.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logger.Error("project dir creation failed", "dir", dir, "error", err)
		return
	}

	m.loadState()
	m.logger.Info("redmine project monitor started",
		"interval", m.cfg.Interval, "last_at", m.state.LastAt)

	monitor.Loop(ctx, time.Duration(m.cfg.Interval)*time.Second, m.poll)
	m.logger.Debug("project monitor exiting")
}

// poll fetches the activity feed once and announces the new activities.
func (m *ProjectMonitor) poll() {
	now := time.Now()
	entries, etag, modified, err := m.client.fetch(m.cfg.FeedURL, m.state.ETag)
	if err != nil {
		if errors.Is(err, errAnubis) {
			m.handleAnubis(now)
			return
		}
		m.logger.Warn("feed fetch failed", "error", err)
		return
	}

	// Any successful fetch ends an Anubis block streak.
	m.state.AnubisStreak = 0
	m.state.AnubisWarned = false
	if !modified {
		m.saveState()
		return
	}

	// First run: seed the watermark and the per-issue status silently.
	if m.state.LastAt == "" && m.state.ETag == "" {
		m.seed(entries, etag, now)
		return
	}

	cur := m.state.lastAt()
	m.warnIfOverflow(entries, cur)

	// Collect the genuinely new activities, oldest first, so that the
	// announcements are chronological.
	var accepted []atomEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		t := e.time()
		if t.IsZero() {
			m.logger.Warn("entry without a valid timestamp", "id", e.ID)
			continue
		}
		if t.After(cur) || (t.Equal(cur) && !m.state.seenAtBoundary(e.ID)) {
			accepted = append(accepted, e)
		}
	}

	// Classify the accepted activities; the per-issue state is updated for
	// every entry even when the action is filtered out.
	var acts []activity
	for i := range accepted {
		a, ok := m.classify(&accepted[i])
		if !ok {
			continue
		}
		m.remember(issueTitle{
			Tracker: a.tracker,
			Number:  a.number,
			Status:  a.status,
			Subject: a.subject,
		})
		if actionWanted(a.action, m.cfg.Actions) {
			acts = append(acts, *a)
		}
	}

	// Advance the watermark to the newest accepted activity and record the
	// ids at exactly that time.
	if len(accepted) > 0 {
		maxT := accepted[0].time()
		for i := range accepted[1:] {
			if t := accepted[i+1].time(); t.After(maxT) {
				maxT = t
			}
		}
		var boundary []string
		if maxT.Equal(cur) {
			boundary = append(boundary, m.state.LastIDs...)
		}
		for i := range accepted {
			if accepted[i].time().Equal(maxT) {
				boundary = append(boundary, accepted[i].ID)
			}
		}
		m.state.setLast(maxT, boundary)
	}
	m.state.ETag = etag
	m.state.UpdatedAt = now.Unix()

	m.logger.Debug("activities classified", "accepted", len(accepted), "announced", len(acts))
	if len(acts) > 0 {
		m.announce(acts)
		for i := range acts {
			h := acts[i].history()
			h.Timestamp = acts[i].at.UTC()
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

// seed records the current feed as the silent baseline of the first run.
func (m *ProjectMonitor) seed(entries []atomEntry, etag string, now time.Time) {
	if len(entries) == 0 {
		m.state.ETag = etag
		m.state.UpdatedAt = now.Unix()
		m.saveState()
		m.logger.Debug("seeded empty feed")
		return
	}

	var maxT time.Time
	for _, e := range entries {
		if it, ok := parseTitle(e.Title); ok {
			m.remember(it)
		}
		if t := e.time(); t.After(maxT) {
			maxT = t
		}
	}
	var ids []string
	for _, e := range entries {
		if e.time().Equal(maxT) {
			ids = append(ids, e.ID)
		}
	}
	m.state.setLast(maxT, ids)
	m.state.ETag = etag
	m.state.UpdatedAt = now.Unix()
	m.saveState()
	m.logger.Info("seeded watermark", "last_at", m.state.LastAt, "issues", len(m.state.Issues))
}

// warnIfOverflow logs when a full feed page does not reach back to the
// watermark, which means activities were dropped before they could be seen.
func (m *ProjectMonitor) warnIfOverflow(entries []atomEntry, cur time.Time) {
	if len(entries) < feedLimit || cur.IsZero() {
		return
	}
	oldest := entries[len(entries)-1].time()
	if oldest.After(cur) {
		m.logger.Warn("activity feed page full and older than watermark; activities may have been missed",
			"watermark", m.state.LastAt,
			"oldest_returned", oldest.UTC().Format(time.RFC3339))
	}
}

// classify maps an activity entry to an announcement, deriving the action
// from the entry id (creation vs journal), the status in the title, and the
// note.
func (m *ProjectMonitor) classify(e *atomEntry) (*activity, bool) {
	if !strings.Contains(e.ID, "/issues/") {
		return nil, false
	}
	it, ok := parseTitle(e.Title)
	if !ok {
		m.logger.Warn("unparsable activity title", "title", e.Title, "id", e.ID)
		return nil, false
	}

	prev := m.state.Issues[strconv.FormatInt(it.Number, 10)]
	note := e.Content.bodyText()

	action := ""
	status := it.Status
	switch {
	case e.isCreation():
		action = "create"
		// The subject is the headline; the description is not announced.
		note = ""
		if status == "" {
			status = "New"
		}
	case status != "":
		switch {
		case status == "Resolved":
			action = "resolve"
		case status == "Closed" || status == "Rejected":
			action = "close"
		case prev != nil && closedStatuses[prev.Status]:
			action = "reopen"
		default:
			action = "update"
		}
	case note != "":
		action = "comment"
	default:
		action = "update"
	}

	return &activity{
		action:  action,
		number:  it.Number,
		actor:   strings.TrimSpace(e.Author.Name),
		tracker: it.Tracker,
		subject: snippet(it.Subject, subjectMax),
		note:    snippet(note, noteMax),
		status:  status,
		url:     e.issueURL(),
		entryID: e.ID,
		at:      e.time(),
	}, true
}

// rememberRaw stores the last-known tracker/subject/status of an issue.  An
// empty status (e.g. a comment) leaves the previous status untouched.
func (m *ProjectMonitor) remember(it issueTitle) {
	key := strconv.FormatInt(it.Number, 10)
	st := m.state.Issues[key]
	if st == nil {
		st = &issueState{}
		m.state.Issues[key] = st
	}
	if it.Tracker != "" {
		st.Tracker = it.Tracker
	}
	if it.Subject != "" {
		st.Subject = it.Subject
	}
	if it.Status != "" {
		st.Status = it.Status
	}
}

// announce posts the activities batched by project, split at the poster's
// length limit.
func (m *ProjectMonitor) announce(acts []activity) {
	lines := make([]string, len(acts))
	for i := range acts {
		lines[i] = acts[i].line()
	}
	prompt := "[" + m.cfg.Name + "] "
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

func (m *ProjectMonitor) post(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.poster.Post(ctx, text); err != nil {
		m.logger.Error("announce failed", "error", err)
	}
}

// handleAnubis records a block and announces a single warning once the
// block streak reaches anubisWarnAfter.
func (m *ProjectMonitor) handleAnubis(now time.Time) {
	m.state.AnubisStreak++
	m.logger.Warn("blocked by Anubis", "streak", m.state.AnubisStreak)
	if m.state.AnubisStreak >= anubisWarnAfter && !m.state.AnubisWarned {
		m.state.AnubisWarned = true
		m.post(fmt.Sprintf("[%s] I'm blocked by Anubis (%d consecutive polls). Help!",
			m.cfg.Name, m.state.AnubisStreak))
	}
	m.state.UpdatedAt = now.Unix()
	m.saveState()
}

// Persistence.

func (m *ProjectMonitor) loadState() {
	var st projState
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
	if st.Issues == nil {
		st.Issues = make(map[string]*issueState)
	}
	m.state = st
	m.logger.Debug("state loaded", "path", m.statePath, "issues", len(st.Issues))
}

func (m *ProjectMonitor) saveState() {
	m.state.UpdatedAt = time.Now().Unix()
	if err := monitor.SaveJSON(m.statePath, &m.state); err != nil {
		m.logger.Error("state file save failure", "path", m.statePath, "error", err)
		return
	}
	m.logger.Debug("state saved", "path", m.statePath)
}

// snippet is the shortened, cleaned text used for announcements.
func snippet(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ")

	if utf8.RuneCountInString(s) <= max {
		return s
	}
	for len(s) > 0 && utf8.RuneCountInString(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s + "…"
}

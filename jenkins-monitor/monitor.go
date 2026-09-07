// Copyright (c) 2026 Aaron LI
//
// Jenkins monitor: poll the configured jobs and nodes, announce
// failures/recoveries via the webhook, and persist the state.
//
// Announcement rules:
//   - Each newly completed failed build (FAILURE or UNSTABLE) is announced,
//     so a job that keeps failing produces one message per failed build.
//   - A SUCCESS after failed build(s) is announced as a recovery (with the
//     count of consecutive failures).
//   - SUCCESS after SUCCESS is silent; stable healthy jobs are never
//     announced.
//   - ABORTED/NOT_BUILT builds are ignored and keep the last state.
//   - On startup only the *current* failure of each job is announced
//     (with the consecutive-failure count); builds that completed while the
//     monitor was down are not each announced.  A failure already announced
//     before (recorded in the state file) is not re-announced when no new
//     build happened since.
//   - Nodes (computers) are announced only on offline<->online transitions.
//
// The monitor runs its poll loop in a single goroutine; no locking needed.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/liweitianux/dflybot/monitor"
)

// Health states of a job.
const (
	stateOK      = "ok"
	stateFailed  = "failed"
	stateUnknown = "unknown"
)

const (
	stateVersion = 1
	// maxNewBuilds caps how many new completed builds are processed in one
	// poll; a larger gap is collapsed onto the newest build only.
	maxNewBuilds = 20
)

// jobState is the persisted per-job tracking state.
type jobState struct {
	State       string `json:"state"`       // ok|failed|unknown
	LastResult  string `json:"last_result"` // result of the latest processed build
	LastBuild   int64  `json:"last_build"`  // latest processed completed build number
	Announced   int64  `json:"announced"`   // last announced failed build number
	Consecutive int    `json:"consecutive"` // consecutive failed builds
	LastURL     string `json:"last_url,omitempty"`
	LastSeen    int64  `json:"last_seen"` // unix seconds of the last poll
}

func (st *jobState) toHistory(name string) *historyLine {
	return &historyLine{
		Type:   "job",
		Name:   name,
		State:  st.State,
		Result: st.LastResult,
		Build:  st.LastBuild,
		URL:    st.LastURL,
	}
}

// nodeState is the persisted per-node tracking state.
type nodeState struct {
	Offline bool   `json:"offline"`
	Reason  string `json:"reason"`
	Seen    int64  `json:"seen"`
}

func (st *nodeState) toHistory(name string) *historyLine {
	return &historyLine{
		Type:    "node",
		Name:    name,
		Offline: &st.Offline,
		Reason:  st.Reason,
	}
}

// monitorState is the on-disk JSON structure of the whole monitor state.
type monitorState struct {
	Version   int                   `json:"version"`
	UpdatedAt int64                 `json:"updated_at"`
	Jobs      map[string]*jobState  `json:"jobs"`
	Nodes     map[string]*nodeState `json:"nodes"`
}

// historyLine is one JSONL line of the .history file.
type historyLine struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"` // "job" or "node"
	Name      string    `json:"name"`
	State     string    `json:"state,omitempty"`
	Result    string    `json:"result,omitempty"`
	Build     int64     `json:"build,omitempty"`
	URL       string    `json:"url,omitempty"`
	Offline   *bool     `json:"offline,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

type Monitor struct {
	cfg     *ConfigJenkins
	jenkins *jenkinsClient
	poster  monitor.Poster

	statePath   string
	historyPath string

	logger *slog.Logger

	state monitorState
	// firstPoll marks the first successful poll after (re)start, during
	// which only the current failures are announced (see rules above).
	firstPoll bool
	// history buffers the history lines of one poll round and flushes them
	// with a single file append at the end of the poll (see monitor pkg).
	history *monitor.History
}

func NewMonitor(
	cfg *ConfigJenkins,
	jenkins *jenkinsClient,
	poster monitor.Poster,
	statePath string,
	historyPath string,
	base *slog.Logger,
) *Monitor {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("jenkins", cfg.Name))
	return &Monitor{
		cfg:         cfg,
		jenkins:     jenkins,
		poster:      poster,
		statePath:   statePath,
		historyPath: historyPath,
		history:     monitor.NewHistory(historyPath),
		logger:      logger,
		state: monitorState{
			Version: stateVersion,
			Jobs:    make(map[string]*jobState),
			Nodes:   make(map[string]*nodeState),
		},
	}
}

func (m *Monitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		if err := m.history.Close(); err != nil {
			m.logger.Warn("history close failure", "error", err)
		}
		m.saveState()
		wg.Done()
	}()

	m.loadState()
	m.firstPoll = true
	m.logger.Info("jenkins monitor started",
		"url", m.cfg.URL, "jobs", len(m.cfg.Jobs),
		"interval", m.cfg.Interval)

	monitor.Loop(ctx, time.Duration(m.cfg.Interval)*time.Second, m.poll)
	m.logger.Debug("monitor exiting")
}

// poll checks all jobs and nodes once and announces any state changes.
func (m *Monitor) poll() {
	// Poll errors are logged and skipped so that a Jenkins outage never
	// causes bogus "recovery" announcements.  State is only saved (and the
	// first-poll handling left behind) after a fully successful poll.
	ok := true
	for _, name := range m.cfg.Jobs {
		if err := m.pollJob(name); err != nil {
			m.logger.Warn("job poll failed", "job", name, "error", err)
			ok = false
		}
	}
	if err := m.pollNodes(); err != nil {
		m.logger.Warn("node poll failed", "error", err)
		ok = false
	}
	// Flush the poll's history lines even when parts failed, so that the
	// lines of the successfully polled jobs are not lost.
	if err := m.history.Flush(); err != nil {
		m.logger.Error("history flush failure", "path", m.historyPath, "error", err)
	}
	if !ok {
		return
	}

	m.firstPoll = false
	m.state.UpdatedAt = time.Now().Unix()
	m.saveState()
}

// pollJob announces any new failed build(s) of one job.
func (m *Monitor) pollJob(name string) error {
	now := time.Now()
	cur, err := m.jenkins.lastCompleted(name)
	if err != nil {
		return err
	}
	if cur == nil {
		return nil // never built; stays unknown
	}

	st := m.state.Jobs[name]
	if st == nil {
		st = &jobState{State: stateUnknown}
		m.state.Jobs[name] = st
	}
	st.LastSeen = now.Unix()

	if st.LastBuild == cur.Number {
		// No new build; nothing to announce.
		m.logHistory(st.toHistory(name))
		return nil
	}

	// Startup (re)handling: only announce the current failure, and skip it
	// if the same failed build was already announced in a previous run.
	if m.firstPoll || st.LastBuild == 0 {
		m.collapseCurrent(name, st, cur)
		m.logHistory(st.toHistory(name))
		return nil
	}

	if cur.Number-st.LastBuild > maxNewBuilds {
		// A large gap (e.g. after a downtime): collapse onto the newest
		// build instead of announcing every missed build.
		m.logger.Warn("large build gap, collapsing onto latest",
			"job", name, "gap", cur.Number-st.LastBuild)
		m.collapseCurrent(name, st, cur)
		m.logHistory(st.toHistory(name))
		return nil
	}

	// Process each newly completed build in order.
	for n := st.LastBuild + 1; n <= cur.Number; n++ {
		b, err := m.jenkins.build(name, n)
		if err != nil {
			if errors.Is(err, errNotFound) {
				continue // pruned/never existed; keep going
			}
			return err // transient error; retry next poll
		}
		m.processBuild(name, st, b)
	}
	st.LastBuild = cur.Number
	m.logHistory(st.toHistory(name))
	return nil
}

// collapseCurrent applies only the newest build (startup or big gap):
// announce a failure once with an estimated consecutive count, otherwise
// just adopt the state.  Missed intermediate builds are not announced.
func (m *Monitor) collapseCurrent(job string, st *jobState, cur *jenkinsBuild) {
	switch cur.Result {
	case "FAILURE", "UNSTABLE":
		st.Consecutive++
		st.State = stateFailed
		st.Announced = cur.Number
		m.announce(m.failText(job, cur, st.Consecutive))
		m.logger.Info("failure announced", "build", cur.Number, "consecutive", st.Consecutive)
	case "SUCCESS":
		st.State = stateOK
		st.Consecutive = 0
	default:
		// ABORTED / NOT_BUILT / "" (running): keep the last state.
		m.logger.Debug("build ignored", "build", cur.Number, "result", cur.Result)
	}
	// The newest build is always adopted as the processed baseline, even
	// when its result was ignored above.
	st.LastResult = cur.Result
	st.LastBuild = cur.Number
	st.LastURL = cur.URL
}

// processBuild announces one completed build and updates the state.
func (m *Monitor) processBuild(job string, st *jobState, b *jenkinsBuild) {
	switch b.Result {
	case "SUCCESS":
		if st.State == stateFailed {
			m.announce(m.recoverText(job, b, st.Consecutive))
			m.logger.Info("recovery announced", "build", b.Number, "after", st.Consecutive)
		}
		st.State = stateOK
		st.Consecutive = 0
	case "FAILURE", "UNSTABLE":
		st.Consecutive++
		st.State = stateFailed
		st.Announced = b.Number
		m.announce(m.failText(job, b, st.Consecutive))
		m.logger.Info("failure announced", "build", b.Number, "consecutive", st.Consecutive)
	default:
		// ABORTED / NOT_BUILT / "" (running): keep the last state.
		m.logger.Debug("build ignored", "build", b.Number, "result", b.Result)
		return
	}
	st.LastResult = b.Result
	st.LastURL = b.URL
}

// pollNodes announces node offline/online transitions.
func (m *Monitor) pollNodes() error {
	now := time.Now()
	computers, err := m.jenkins.computers()
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(computers))
	for _, n := range computers {
		if !m.cfg.nodeWanted(n.DisplayName) {
			continue
		}
		seen[n.DisplayName] = true
		st := m.state.Nodes[n.DisplayName]
		switch {
		case st == nil:
			// First observation: baseline; announce it if offline
			// (e.g., on startup), and log the state.
			st = &nodeState{Offline: n.Offline, Reason: n.OfflineCauseReason, Seen: now.Unix()}
			m.state.Nodes[n.DisplayName] = st
			if n.Offline {
				m.announce(m.nodeText(n.DisplayName, true, n.OfflineCauseReason))
				m.logger.Info("node offline announced", "node", n.DisplayName,
					"reason", n.OfflineCauseReason)
				m.logHistory(st.toHistory(n.DisplayName))
			}
		case st.Offline && !n.Offline:
			st.Offline = false
			st.Reason = ""
			st.Seen = now.Unix()
			m.announce(m.nodeText(n.DisplayName, false, ""))
			m.logger.Info("node online announced", "node", n.DisplayName)
			m.logHistory(st.toHistory(n.DisplayName))
		case !st.Offline && n.Offline:
			st.Offline = true
			st.Reason = n.OfflineCauseReason
			st.Seen = now.Unix()
			m.announce(m.nodeText(n.DisplayName, true, n.OfflineCauseReason))
			m.logger.Info("node offline announced", "node", n.DisplayName,
				"reason", n.OfflineCauseReason)
			m.logHistory(st.toHistory(n.DisplayName))
		default:
			st.Seen = now.Unix() // unchanged; no announcement
		}
	}
	// Computers that disappeared: ephemeral agents are removed silently;
	// whitelisted permanent nodes keep their last known state.
	for name := range m.state.Nodes {
		if !seen[name] && !m.cfg.nodeWanted(name) {
			delete(m.state.Nodes, name)
			m.logger.Debug("node disappeared", "node", name)
		}
	}
	return nil
}

func (m *Monitor) announce(text string) {
	msg := fmt.Sprintf("[%s] %s", m.cfg.Name, text)
	m.logger.Debug("announce message", "msg", msg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.poster.Post(ctx, msg); err != nil {
		m.logger.Error("announce failed", "msg", msg, "error", err)
	}
}

func (m *Monitor) failText(job string, b *jenkinsBuild, consecutive int) string {
	text := fmt.Sprintf("%s FAILED: build #%d", job, b.Number)
	if b.Result == "UNSTABLE" {
		text += " (UNSTABLE)"
	}
	if consecutive > 1 {
		text += fmt.Sprintf(" (%d consecutive failures)", consecutive)
	}
	text += " " + b.URL
	return strings.TrimSpace(text)
}

func (m *Monitor) recoverText(job string, b *jenkinsBuild, after int) string {
	return fmt.Sprintf("%s RECOVERED: build #%d SUCCESS after %d failed builds %s",
		job, b.Number, after, b.URL)
}

func (m *Monitor) nodeText(name string, offline bool, reason string) string {
	text := fmt.Sprintf("executor `%s` ", name)
	if offline {
		text += "OFFLINE"
		if reason != "" {
			text += ": " + reason
		}
	} else {
		text += "back ONLINE"
	}
	return text
}

// Persistence: state (JSON, atomic) and history (JSONL).

func (m *Monitor) loadState() {
	var st monitorState
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
	if st.Jobs == nil {
		st.Jobs = make(map[string]*jobState)
	}
	if st.Nodes == nil {
		st.Nodes = make(map[string]*nodeState)
	}
	m.state = st
	m.logger.Info("state loaded", "path", m.statePath,
		"jobs", len(st.Jobs), "nodes", len(st.Nodes))
}

func (m *Monitor) saveState() {
	m.state.UpdatedAt = time.Now().Unix()
	if err := monitor.SaveJSON(m.statePath, &m.state); err != nil {
		m.logger.Error("state file save failure", "path", m.statePath, "error", err)
		return
	}
	m.logger.Debug("state saved", "path", m.statePath)
}

// logHistory buffers one history line of the current poll round.
func (m *Monitor) logHistory(h *historyLine) {
	h.Timestamp = time.Now().UTC()
	if err := m.history.Append(h); err != nil {
		m.logger.Error("history append failure", "error", err)
	}
}

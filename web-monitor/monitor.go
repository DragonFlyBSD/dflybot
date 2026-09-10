// Copyright (c) 2026 Aaron LI
//
// WebMonitor: per-web probe loop with down/up hysteresis, certificate
// expiry warnings, announcements, state and history persistence.
//

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/liweitianux/dflybot/monitor"
)

const stateVersion = 1

// Web health states.
const (
	stateUnknown = ""
	stateUp      = "up"
	stateDown    = "down"
)

// Default certificate expiry warning thresholds (days), in decreasing
// order.
var defaultExpiringDays = []int{15, 7, 3, 2, 1}

// webState is the on-disk state of one web.
type webState struct {
	Version     int    `json:"version"`
	State       string `json:"state"` // up|down|""(unknown)
	PendingUp   int    `json:"pending_up,omitempty"`
	PendingDown int    `json:"pending_down,omitempty"`
	// DownSince is the unix time when the current down period started
	// (only meaningful while State == "down"); used to report the total
	// downtime in the recovery announcement.
	DownSince int64 `json:"down_since,omitempty"`
	// Certificate tracking (verified HTTPS only).
	CertNotAfterUnix int64 `json:"cert_not_after_unix,omitempty"`
	CertWarnedDays   int   `json:"cert_warned_days,omitempty"`
	// CertWarnedDate is the UTC date (YYYY-MM-DD) of the last "expired"
	// warning; an expired certificate is re-announced at most once per day.
	CertWarnedDate string `json:"cert_warned_date,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

// webHistory is one line of the per-web .history JSONL file.
type webHistory struct {
	Timestamp time.Time `json:"ts"`
	URL       string    `json:"url"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status,omitempty"`
	Duration  int64     `json:"duration_ms"`
	Reason    string    `json:"reason,omitempty"`
	DaysLeft  int       `json:"days_left,omitempty"`
}

type WebMonitor struct {
	cfg          *ConfigWeb
	prober       *prober
	alert        *ConfigAlert
	poster       monitor.Poster
	expiringDays []int  // in ascending order (e.g. [1 2 3 7 15]).
	host         string // host of the probed URL, for the certificate messages
	logger       *slog.Logger

	statePath   string
	historyPath string
	history     *monitor.History

	state webState
}

func newWebMonitor(
	web *ConfigWeb,
	prober *prober,
	poster monitor.Poster,
	alert *ConfigAlert,
	tlsCfg *ConfigTLS,
	dataDir string,
	base *slog.Logger,
) *WebMonitor {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("web", web.Name))
	days := append([]int(nil), tlsCfg.ExpiringDays...) // make a copy
	if len(days) == 0 {
		days = defaultExpiringDays
	}
	sort.Ints(days)
	host := web.Name
	if u, err := url.Parse(web.URL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return &WebMonitor{
		cfg:          web,
		prober:       prober,
		alert:        alert,
		poster:       poster,
		expiringDays: days,
		host:         host,
		logger:       logger,
		statePath:    dataDir + "/" + web.Name + ".state",
		historyPath:  dataDir + "/" + web.Name + ".history",
		history:      monitor.NewHistory(dataDir + "/" + web.Name + ".history"),
		state:        webState{Version: stateVersion},
	}
}

func (m *WebMonitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		if err := m.history.Close(); err != nil {
			m.logger.Warn("history close failure", "error", err)
		}
		m.saveState()
		wg.Done()
	}()

	m.loadState()
	m.logger.Info("web monitor started", "url", m.cfg.URL, "interval", m.cfg.Interval)

	interval := time.Duration(m.cfg.Interval) * time.Second
	monitor.Loop(ctx, interval, m.poll)
	m.logger.Debug("web monitor exiting")
}

// poll probes the web once, updates the state (hysteresis) and announces
// any state changes or certificate warnings.
func (m *WebMonitor) poll() {
	now := time.Now()
	res := m.prober.Probe()
	m.logger.Debug("probed", "url", m.cfg.URL, "result", res)

	hist := webHistory{
		Timestamp: now.UTC(),
		URL:       m.cfg.URL,
		OK:        res.ok,
		Status:    res.status,
		Duration:  res.ms,
		Reason:    res.reason,
	}
	if res.cert != nil {
		hist.DaysLeft = res.cert.daysLeft
	}

	var msgs []string
	if msg := m.updateState(res, now); msg != "" {
		msgs = append(msgs, msg)
	}
	if res.cert != nil {
		if msg := m.updateCert(res.cert, now); msg != "" {
			msgs = append(msgs, msg)
		}
	}
	for _, msg := range msgs {
		m.post(msg)
	}

	if err := m.history.Append(hist); err != nil {
		m.logger.Error("history append failure", "error", err)
	}
	if err := m.history.Flush(); err != nil {
		m.logger.Error("history flush failure", "path", m.historyPath, "error", err)
	}
	m.state.UpdatedAt = now.Unix()
	m.saveState()
}

// updateState applies the probe result to the hysteresis state machine and
// returns any announcement messages.
func (m *WebMonitor) updateState(res *probeResult, now time.Time) string {
	var msg string
	st := &m.state
	switch st.State {
	case stateUnknown, stateUp:
		if res.ok {
			if st.State == stateUnknown {
				st.State = stateUp
			} else {
				st.PendingDown = 0
			}
		} else {
			st.PendingDown++
			if st.PendingDown >= m.alert.DownRepeats {
				msg = m.downMsg(res)
				st.State = stateDown
				st.PendingDown = 0
				st.DownSince = now.Unix()
			}
		}
	case stateDown:
		if res.ok {
			st.PendingUp++
			if st.PendingUp >= m.alert.UpRepeats {
				msg = m.upMsg(res, now.Sub(time.Unix(st.DownSince, 0)))
				st.State = stateUp
				st.PendingUp = 0
				st.DownSince = 0
			}
		} else {
			st.PendingUp = 0
		}
	}
	return msg
}

// updateCert checks the certificate expiry thresholds and returns a warning
// message when a new threshold is crossed (once per threshold per cert).
// An expired certificate is announced at most once per UTC day.
func (m *WebMonitor) updateCert(cert *certInfo, now time.Time) string {
	if m.state.CertNotAfterUnix != cert.notAfterUnix {
		// New certificate: start over.
		m.state.CertNotAfterUnix = cert.notAfterUnix
		m.state.CertWarnedDays = m.expiringDays[len(m.expiringDays)-1] + 1
		m.state.CertWarnedDate = ""
	}
	warned := m.state.CertWarnedDays
	notAfter := time.Unix(cert.notAfterUnix, 0)

	if cert.daysLeft < 0 {
		// Expired: announce at most once per UTC day (roughly at midnight).
		today := now.UTC().Format("2006-01-02")
		if m.state.CertWarnedDate != today {
			m.state.CertWarnedDate = today
			return m.prefix(fmt.Sprintf("certificate for %s expired %s",
				m.host, notAfter.Format("2006-01-02")))
		}
		return ""
	}

	// Find the highest (least urgent) yet-unwarned threshold that the
	// certificate is now at or below.
	for i := len(m.expiringDays) - 1; i >= 0; i-- {
		t := m.expiringDays[i]
		if cert.daysLeft <= t && t < warned {
			m.state.CertWarnedDays = t
			return m.prefix(fmt.Sprintf("certificate for %s expires in %d day(s) (%s)",
				m.host, cert.daysLeft, notAfter.Format("2006-01-02")))
		}
	}
	return ""
}

func (m *WebMonitor) prefix(msg string) string {
	return "[web:" + m.cfg.Name + "] " + msg
}

func (m *WebMonitor) downMsg(res *probeResult) string {
	reason := res.reason
	if reason == "" {
		reason = "unreachable"
	}
	return m.prefix(fmt.Sprintf("DOWN: %s (%s) — %d consecutive failures",
		m.cfg.URL, reason, m.alert.DownRepeats))
}

func (m *WebMonitor) upMsg(res *probeResult, down time.Duration) string {
	text := fmt.Sprintf("UP: %s recovered", m.cfg.URL)
	if down > 0 {
		text += fmt.Sprintf(" (down %s)", fmtDowntime(down))
	}
	return m.prefix(text)
}

// fmtDowntime renders a duration compactly, e.g. "45s", "3m20s", "1h5m".
func fmtDowntime(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	m := secs / 60
	if m < 60 {
		s := secs % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := m / 60
	m = m % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func (m *WebMonitor) post(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.poster.Post(ctx, text); err != nil {
		m.logger.Error("announce failed", "error", err)
	}
}

// Persistence.

func (m *WebMonitor) loadState() {
	var st webState
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

func (m *WebMonitor) saveState() {
	m.state.UpdatedAt = time.Now().Unix()
	if err := monitor.SaveJSON(m.statePath, &m.state); err != nil {
		m.logger.Error("state file save failure", "path", m.statePath, "error", err)
	}
}

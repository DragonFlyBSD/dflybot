// Copyright (c) 2026 Aaron LI
//
// Tests for the web monitor state machine (hysteresis, certificate
// thresholds) and the end-to-end poll loop against a stub HTTP server.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liweitianux/dflybot/monitor"
)

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

// newStateMon builds a WebMonitor whose state machine can be driven
// directly without any network probing.
func newStateMon(down, up int) *WebMonitor {
	return &WebMonitor{
		cfg:          &ConfigWeb{Name: "www", URL: "https://example.com/"},
		poster:       &recordPoster{},
		alert:        &ConfigAlert{DownRepeats: down, UpRepeats: up},
		expiringDays: []int{1, 2, 3, 7, 15},
	}
}

func TestHysteresis(t *testing.T) {
	mon := newStateMon(2, 2)
	poster := mon.poster.(*recordPoster)

	if msg := mon.updateState(&probeResult{ok: true}); msg != "" {
		t.Fatalf("startup ok announced: %s", msg)
	}
	// One failure is not enough.
	if msg := mon.updateState(&probeResult{ok: false, reason: "timeout"}); msg != "" {
		t.Fatalf("first failure announced: %s", msg)
	}
	if mon.state.State != stateUp {
		t.Fatalf("state = %q, want up", mon.state.State)
	}
	// Second consecutive failure declares down.
	if msg := mon.updateState(&probeResult{ok: false, reason: "timeout"}); msg == "" ||
		!strings.Contains(msg, "DOWN: https://example.com/") {
		t.Fatalf("down messages = %s", msg)
	}
	// Still failing: no re-announcement.
	if msg := mon.updateState(&probeResult{ok: false, reason: "timeout"}); msg != "" {
		t.Fatalf("continued failure announced: %s", msg)
	}
	// One success is not enough to recover.
	if msg := mon.updateState(&probeResult{ok: true, status: 200}); msg != "" {
		t.Fatalf("first success announced: %s", msg)
	}
	// Second consecutive success recovers.
	if msg := mon.updateState(&probeResult{ok: true, status: 200}); msg == "" ||
		!strings.Contains(msg, "UP:") {
		t.Fatalf("up messages = %s", msg)
	}
	if mon.state.State != stateUp {
		t.Fatalf("state = %q, want up", mon.state.State)
	}
	_ = poster
}

func TestStartupDown(t *testing.T) {
	mon := newStateMon(2, 2)
	// A site that is down from the start is announced after the window.
	mon.updateState(&probeResult{ok: false, reason: "refused"})
	if msg := mon.updateState(&probeResult{ok: false, reason: "refused"}); msg == "" {
		t.Fatalf("no startup down messages")
	}
	if mon.state.State != stateDown {
		t.Fatalf("state = %q", mon.state.State)
	}
}

func certInfoAt(notAfter time.Time, days int) *certInfo {
	return &certInfo{notAfterUnix: notAfter.Unix(), daysLeft: days}
}

func TestCertThresholds(t *testing.T) {
	mon := newStateMon(2, 2)
	now := time.Now()

	// New certificate, plenty of days left: no warning, but initialized.
	na := now.Add(30 * 24 * time.Hour)
	if msg := mon.updateCert(certInfoAt(na, 30)); msg != "" {
		t.Fatalf("unexpected warning: %q", msg)
	}
	// Crosses 15: warn once.
	na = now.Add(12 * 24 * time.Hour)
	if msg := mon.updateCert(certInfoAt(na, 12)); !strings.Contains(msg, "expires in 12 day(s)") {
		t.Fatalf("warn at 12 days: %q", msg)
	}
	// Same days again: no repeat.
	if msg := mon.updateCert(certInfoAt(na, 12)); msg != "" {
		t.Fatalf("repeat warning: %q", msg)
	}
	// Crosses 7 and then 1.
	if msg := mon.updateCert(certInfoAt(na.Add(-6*24*time.Hour), 6)); !strings.Contains(msg, "expires in 6 day(s)") {
		t.Fatalf("warn at 6 days: %q", msg)
	}
	if msg := mon.updateCert(certInfoAt(na.Add(-11*24*time.Hour), 1)); !strings.Contains(msg, "expires in 1 day(s)") {
		t.Fatalf("warn at 1 day: %q", msg)
	}
	// Expired: warn once.
	na = now.Add(-24 * time.Hour)
	if msg := mon.updateCert(certInfoAt(na, -1)); !strings.Contains(msg, "certificate expired") {
		t.Fatalf("expired warning: %q", msg)
	}
	if msg := mon.updateCert(certInfoAt(na, -2)); msg != "" {
		t.Fatalf("expired repeat warning: %q", msg)
	}
	// A new certificate resets the warnings.
	na = now.Add(20 * 24 * time.Hour)
	if msg := mon.updateCert(certInfoAt(na, 20)); msg != "" {
		t.Fatalf("new cert unexpected warning: %q", msg)
	}
}

func TestEndToEndPoll(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	poster := &recordPoster{}
	web := &ConfigWeb{Name: "w", URL: ts.URL, Interval: 30}
	p, err := newProber(web, &ConfigTLS{}, &ConfigTimeouts{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mon := newWebMonitor(web, p, poster, &ConfigAlert{DownRepeats: 2, UpRepeats: 1},
		&ConfigTLS{}, dir, nil)
	mon.loadState()

	// Up at start: silent seed.
	mon.poll()
	mon.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("healthy polls announced: %v", got)
	}

	// Two failures -> down.
	healthy.Store(false)
	mon.poll()
	mon.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "DOWN:") {
		t.Fatalf("down messages = %v", msgs)
	}

	// One success -> recovery (up_repeats = 1).
	healthy.Store(true)
	mon.poll()
	msgs = poster.messages()
	if len(msgs) != 2 || !strings.Contains(msgs[1], "UP:") {
		t.Fatalf("up messages = %v", msgs)
	}

	// History and state files exist with the expected lines.
	b, err := os.ReadFile(filepath.Join(dir, "w.history"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; got != 5 {
		t.Errorf("history lines = %d, want 5", got)
	}
	var st webState
	exists, err := monitor.ReadJSON(filepath.Join(dir, "w.state"), &st)
	if err != nil || !exists {
		t.Fatalf("state read: %v %v", exists, err)
	}
	if st.State != stateUp {
		t.Errorf("state = %q", st.State)
	}
}

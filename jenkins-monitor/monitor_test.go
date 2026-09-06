// Copyright (c) 2026 Aaron LI
//
// Unit tests for the Jenkins REST client and the monitor, using a stub
// Jenkins HTTP server (no real Jenkins involved).
//
// Co-authored-by: Deepseek-v4-flash (wit Pi Coding Agent)
//

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

type stubBuild struct {
	Number int64  `json:"number"`
	Result string `json:"result"`
	URL    string `json:"url"`
}

// jenkinsStub is a configurable fake Jenkins server.
type jenkinsStub struct {
	mu        sync.Mutex
	last      map[string]stubBuild        // job name -> last completed build
	builds    map[string]map[int64]string // job name -> build number -> result
	computers []stubComputer
}

type stubComputer struct {
	DisplayName        string `json:"displayName"`
	Offline            bool   `json:"offline"`
	OfflineCauseReason string `json:"offlineCauseReason"`
}

func (s *jenkinsStub) setLast(name string, b stubBuild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]stubBuild{}
	}
	s.last[name] = b
}

func (s *jenkinsStub) setBuild(name string, number int64, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.builds == nil {
		s.builds = map[string]map[int64]string{}
	}
	if s.builds[name] == nil {
		s.builds[name] = map[int64]string{}
	}
	s.builds[name][number] = result
}

func (s *jenkinsStub) setComputers(cs []stubComputer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.computers = append([]stubComputer(nil), cs...)
}

// handler serves the fake Jenkins REST API.
func (s *jenkinsStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case parts[0] == "computer": // computer/api/json
			if len(parts) == 3 && parts[1] == "api" && parts[2] == "json" {
				json.NewEncoder(w).Encode(struct {
					Computer []stubComputer `json:"computer"`
				}{s.computers})
				return
			}
		case parts[0] == "job" && len(parts) == 5 && parts[3] == "api" && parts[4] == "json":
			// job/<name>/<number>/api/json
			var number int64
			if _, err := fmt.Sscanf(parts[2], "%d", &number); err != nil {
				http.NotFound(w, r)
				return
			}
			result, ok := s.builds[parts[1]][number]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(stubBuild{Number: number, Result: result})
			return
		case parts[0] == "job" && len(parts) == 4 && parts[2] == "api" && parts[3] == "json":
			// job/<name>/api/json
			b, ok := s.last[parts[1]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(struct {
				LastCompletedBuild *stubBuild `json:"lastCompletedBuild"`
			}{&b})
			return
		}
		http.NotFound(w, r)
	})
}

// fakeCfg builds a ConfigJenkins pointing at the stub server.
func fakeCfg(ts *httptest.Server) *ConfigJenkins {
	return &ConfigJenkins{
		Name:     "dragonfly",
		URL:      ts.URL + "/",
		Interval: 60,
		Jobs:     []string{"DragonFlyBSD"},
	}
}

// newTestMonitor creates a monitor in a temp dir with the stub server and a
// recording poster, mirroring the Start() prelude (state load, first poll).
func newTestMonitor(t *testing.T, cfg *ConfigJenkins, poster *recordPoster) (*Monitor, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "dragonfly.state")
	histPath := filepath.Join(dir, "dragonfly.history")
	m := NewMonitor(cfg, newJenkinsClient(cfg), poster, statePath, histPath, nil)
	m.loadState()
	m.firstPoll = true
	return m, statePath
}

func buildURL(jobURL string, n int64) string {
	return fmt.Sprintf("%s/%d/", strings.TrimRight(jobURL, "/"), n)
}

// ---- tests ----

func TestMonitorStartupFailureAndRestartDedup(t *testing.T) {
	stub := &jenkinsStub{}
	stub.setLast("DragonFlyBSD", stubBuild{Number: 7, Result: "FAILURE",
		URL: buildURL("https://ci/job/DragonFlyBSD", 7)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	cfg := fakeCfg(ts)
	m, statePath := newTestMonitor(t, cfg, poster)
	m.poll() // startup: current failure announced once
	if got := poster.messages(); len(got) != 1 || !strings.Contains(got[0], "DragonFlyBSD FAILED: build #7") {
		t.Fatalf("startup messages = %v", got)
	}
	m.poll() // no new build; nothing
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("second poll messages = %v", got)
	}

	// Restart (same state file, same build): the failure was already
	// announced and must not be re-announced (same poster would see it).
	m2 := NewMonitor(cfg, newJenkinsClient(cfg), poster, statePath,
		filepath.Join(filepath.Dir(statePath), "dragonfly.history"), nil)
	m2.loadState()
	m2.firstPoll = true
	m2.poll()
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("restart re-announced the failure: %v", got)
	}
}

func TestMonitorFailureRecoveryAndContinuous(t *testing.T) {
	stub := &jenkinsStub{}
	jobURL := "https://ci/job/DragonFlyBSD"
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "FAILURE", URL: buildURL(jobURL, 1)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	cfg := fakeCfg(ts)
	m, _ := newTestMonitor(t, cfg, poster)

	m.poll() // startup failure #1
	// New failed build #2 while running.
	stub.setLast("DragonFlyBSD", stubBuild{Number: 2, Result: "FAILURE", URL: buildURL(jobURL, 2)})
	stub.setBuild("DragonFlyBSD", 2, "FAILURE")
	m.poll()
	// New successful build #3: recovery.
	stub.setLast("DragonFlyBSD", stubBuild{Number: 3, Result: "SUCCESS", URL: buildURL(jobURL, 3)})
	stub.setBuild("DragonFlyBSD", 3, "SUCCESS")
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	if !strings.Contains(msgs[0], "FAILED: build #1") || !strings.Contains(msgs[1], "FAILED: build #2") {
		t.Errorf("failure msgs: %v", msgs)
	}
	if !strings.Contains(msgs[2], "RECOVERED: build #3 SUCCESS after 2 failed builds") {
		t.Errorf("recovery msg: %v", msgs[2])
	}
	m.poll() // stable healthy; silent
	if got := poster.messages(); len(got) != 3 {
		t.Errorf("healthy poll announced: %v", got)
	}
}

func TestMonitorIgnoreAborted(t *testing.T) {
	stub := &jenkinsStub{}
	jobURL := "https://ci/job/DragonFlyBSD"
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "FAILURE", URL: buildURL(jobURL, 1)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, fakeCfg(ts), poster)
	m.poll() // failure #1 announced

	// Builds #2 (aborted) and #3 (success) complete before the next poll.
	stub.setBuild("DragonFlyBSD", 2, "ABORTED")
	stub.setBuild("DragonFlyBSD", 3, "SUCCESS")
	stub.setLast("DragonFlyBSD", stubBuild{Number: 3, Result: "SUCCESS", URL: buildURL(jobURL, 3)})
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if !strings.Contains(msgs[1], "RECOVERED: build #3 SUCCESS after 1 failed builds") {
		t.Errorf("recovery after aborted build: %v", msgs[1])
	}
}

func TestMonitorNodeTransitions(t *testing.T) {
	stub := &jenkinsStub{}
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "SUCCESS", URL: "https://ci/x/1/"})
	stub.setComputers([]stubComputer{{DisplayName: "Built-In Node", Offline: false}})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, fakeCfg(ts), poster)
	m.poll() // baseline; nothing announced (online)
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("baseline announced: %v", got)
	}

	stub.setComputers([]stubComputer{{DisplayName: "Built-In Node", Offline: true,
		OfflineCauseReason: "crashed"}})
	m.poll()
	if got := poster.messages(); len(got) != 1 || !strings.Contains(got[0], "Built-In Node OFFLINE: crashed") {
		t.Fatalf("offline messages = %v", got)
	}
	m.poll() // still offline; silent
	if got := poster.messages(); len(got) != 1 {
		t.Fatalf("still-offline poll announced: %v", got)
	}
	stub.setComputers([]stubComputer{{DisplayName: "Built-In Node", Offline: false}})
	m.poll()
	if got := poster.messages(); len(got) != 2 || !strings.Contains(got[1], "back ONLINE") {
		t.Fatalf("online messages = %v", got)
	}
}

func TestMonitorNodeStartupOffline(t *testing.T) {
	stub := &jenkinsStub{}
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "SUCCESS", URL: "https://ci/x/1/"})
	stub.setComputers([]stubComputer{{DisplayName: "Build1", Offline: true,
		OfflineCauseReason: "down"}})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, fakeCfg(ts), poster)
	m.poll()
	msgs := poster.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Build1 OFFLINE: down") {
		t.Fatalf("startup offline messages = %v", msgs)
	}
}

func TestMonitorNodeWhitelist(t *testing.T) {
	stub := &jenkinsStub{}
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "SUCCESS", URL: "https://ci/x/1/"})
	// A transient ephemeral agent appears offline; it must be ignored.
	stub.setComputers([]stubComputer{
		{DisplayName: "Keep", Offline: false},
		{DisplayName: "ephemeral-abc", Offline: true, OfflineCauseReason: "dying"},
	})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	cfg := fakeCfg(ts)
	cfg.Nodes = []string{"Keep"}
	poster := &recordPoster{}
	m, _ := newTestMonitor(t, cfg, poster)
	m.poll()
	if got := poster.messages(); len(got) != 0 {
		t.Fatalf("whitelist ignored ephemeral: %v", got)
	}
}

func TestMonitorLargeGapCollapses(t *testing.T) {
	stub := &jenkinsStub{}
	jobURL := "https://ci/job/DragonFlyBSD"
	stub.setLast("DragonFlyBSD", stubBuild{Number: 1, Result: "FAILURE", URL: buildURL(jobURL, 1)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, fakeCfg(ts), poster)
	m.poll() // failure #1 (1 msg)

	// 30 more builds completed while we were away: only the current
	// failure is announced (not each of the 30).
	stub.setLast("DragonFlyBSD", stubBuild{Number: 31, Result: "FAILURE", URL: buildURL(jobURL, 31)})
	m.poll()

	msgs := poster.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if !strings.Contains(msgs[1], "build #31") {
		t.Errorf("collapse msg: %v", msgs[1])
	}
}

func TestMonitorStateAndHistoryFiles(t *testing.T) {
	stub := &jenkinsStub{}
	jobURL := "https://ci/job/DragonFlyBSD"
	stub.setLast("DragonFlyBSD", stubBuild{Number: 5, Result: "SUCCESS", URL: buildURL(jobURL, 5)})
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	poster := &recordPoster{}
	m, _ := newTestMonitor(t, fakeCfg(ts), poster)
	m.poll()
	m.saveState()

	if _, err := os.Stat(m.statePath); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	hist, err := os.ReadFile(m.historyPath)
	if err != nil {
		t.Fatalf("history file missing: %v", err)
	}
	if !strings.Contains(string(hist), `"type":"job"`) {
		t.Errorf("history lines: %s", hist)
	}
}

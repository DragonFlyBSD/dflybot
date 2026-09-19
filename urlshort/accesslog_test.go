// Copyright (c) 2026 Aaron LI
//
// Access log tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct {
	ns atomic.Int64
}

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.set(t)
	return c
}

func (c *testClock) now() time.Time  { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) set(t time.Time) { c.ns.Store(t.UnixNano()) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAccessLogRotation(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 11, 5, 0, 0, 0, time.UTC))
	l, err := NewAccessLogger(dir, 30, time.Hour, clock.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Log(AccessEntry{Type: AccessTypeRedirect, Path: "/a", Status: 302})
	waitFor(t, "day 1 file", func() bool {
		_, err := os.Stat(filepath.Join(dir, "access-2026-09-11.jsonl"))
		return err == nil
	})

	clock.set(time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC))
	l.Log(AccessEntry{Type: AccessTypeRedirect, Path: "/b", Status: 404})
	waitFor(t, "day 2 file", func() bool {
		_, err := os.Stat(filepath.Join(dir, "access-2026-09-12.jsonl"))
		return err == nil
	})
	l.Close()

	for _, day := range []string{"2026-09-11", "2026-09-12"} {
		f, err := os.Open(filepath.Join(dir, "access-"+day+".jsonl"))
		if err != nil {
			t.Fatalf("open %s: %v", day, err)
		}
		n := 0
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if len(sc.Bytes()) > 0 {
				n++
			}
		}
		f.Close()
		if n != 1 {
			t.Errorf("%s has %d lines, want 1", day, n)
		}
	}
}

func TestAccessLogRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for _, day := range []string{"2026-08-01", "2026-09-10", "2026-09-11"} {
		if err := os.WriteFile(filepath.Join(dir, "access-"+day+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := NewAccessLogger(dir, 30, time.Hour, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if _, err := os.Stat(filepath.Join(dir, "access-2026-08-01.jsonl")); !os.IsNotExist(err) {
		t.Errorf("old log not deleted (err=%v)", err)
	}
	for _, day := range []string{"2026-09-10", "2026-09-11"} {
		if _, err := os.Stat(filepath.Join(dir, "access-"+day+".jsonl")); err != nil {
			t.Errorf("recent log %s missing: %v", day, err)
		}
	}
}

func TestAccessLogOverflowDrops(t *testing.T) {
	// A full channel with no reader must drop without blocking.
	l := &AccessLogger{
		ch:     make(chan AccessEntry, 1),
		logger: slog.Default(),
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
	l.ch <- AccessEntry{Type: AccessTypeRedirect}
	done := make(chan struct{})
	go func() {
		l.Log(AccessEntry{Type: AccessTypeRedirect})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Log blocked on a full channel")
	}
	if got := l.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

func TestAccessLogDrainOnClose(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAccessLogger(dir, 30, time.Hour, func() time.Time {
		return time.Date(2026, 9, 11, 5, 0, 0, 0, time.UTC)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for i := 0; i < n; i++ {
		l.Log(AccessEntry{Type: AccessTypeAPI, Action: "resolve"})
	}
	l.Close()

	f, err := os.Open(filepath.Join(dir, "access-2026-09-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			count++
		}
	}
	if count != n {
		t.Fatalf("wrote %d lines, want %d", count, n)
	}
	// Logging after Close is a no-op.
	l.Log(AccessEntry{Type: AccessTypeAPI})
}

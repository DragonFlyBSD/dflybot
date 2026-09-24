// Copyright (c) 2026 Aaron LI
//
// Shared daily log file tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyLogOverflowDrops(t *testing.T) {
	// A full channel with no reader must drop without blocking.
	l := &dailyLog{
		ch:     make(chan []byte, 1),
		logger: slog.Default(),
		now:    func() time.Time { return time.Now().UTC() },
	}
	l.ch <- []byte("x\n")
	done := make(chan struct{})
	go func() {
		l.Write([]byte("y\n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked on a full channel")
	}
	if got := l.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

func TestDailyLogRotation(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Date(2026, 9, 11, 5, 0, 0, 0, time.UTC))
	l, err := newDailyLog(dir, "test-", ".log", 30, time.Hour, clock.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Write([]byte("first"))
	waitFor(t, "day 1 file", func() bool {
		_, err := os.Stat(filepath.Join(dir, "test-2026-09-11.log"))
		return err == nil
	})
	clock.set(time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC))
	l.Write([]byte("second\n"))
	waitFor(t, "day 2 file", func() bool {
		_, err := os.Stat(filepath.Join(dir, "test-2026-09-12.log"))
		return err == nil
	})
	l.Close()

	for day, want := range map[string]string{
		"2026-09-11": "first\n",
		"2026-09-12": "second\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, "test-"+day+".log"))
		if err != nil {
			t.Fatalf("read %s: %v", day, err)
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", day, b, want)
		}
	}
}

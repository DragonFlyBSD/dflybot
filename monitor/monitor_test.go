// Copyright (c) 2026 Aaron LI
//
// Tests for the shared monitor package.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package monitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveAndReadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if exists, err := ReadJSON(path, &map[string]int{}); err != nil || exists {
		t.Fatalf("ReadJSON on missing file = %v, %v", exists, err)
	}
	v := map[string]int{"a": 1, "b": 2}
	if err := SaveJSON(path, v); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	exists, err := ReadJSON(path, &got)
	if err != nil || !exists {
		t.Fatalf("ReadJSON = %v, %v", exists, err)
	}
	if got["a"] != 1 || got["b"] != 2 {
		t.Errorf("got %v", got)
	}
	// No leftover tmp files.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || strings.Contains(entries[0].Name(), ".tmp") {
		t.Errorf("unexpected dir entries: %v", entries)
	}
}

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.jsonl")
	h := NewHistory(path)

	// Batched appends: several lines flushed with one call.
	if err := h.Append(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.Append(map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	for i, l := range lines {
		var m map[string]int
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["n"] != i+1 {
			t.Errorf("line %d = %q", i, l)
		}
	}

	// Further appends in a later batch.
	if err := h.Append(map[string]int{"n": 3}); err != nil {
		t.Fatal(err)
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	if lines = readLines(t, path); len(lines) != 3 {
		t.Fatalf("lines after second flush = %v", lines)
	}

	// An empty flush is a no-op and must not create other files.
	if err := NewHistory(filepath.Join(dir, "nope.jsonl")).Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope.jsonl")); !os.IsNotExist(err) {
		t.Errorf("empty flush created a file")
	}

	// Close flushes the remaining lines and rejects further Appends.
	if err := h.Append(map[string]int{"n": 4}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if lines = readLines(t, path); len(lines) != 4 {
		t.Fatalf("lines after Close = %v", lines)
	}
	if err := h.Append(map[string]int{"n": 5}); err != ErrClosed {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int32
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	Loop(ctx, 10*time.Millisecond, func() { count.Add(1) })
	if n := count.Load(); n < 3 {
		t.Errorf("poll ran %d times, want >= 3", n)
	}
}

func TestLogLevel(t *testing.T) {
	var lv slog.LevelVar
	LogLevel(&lv, "debug")
	if lv.Level() != slog.LevelDebug {
		t.Errorf("level = %v", lv.Level())
	}
}

// Copyright (c) 2026 Aaron LI
//
// Tests for the shared monitor package.
//
// Co-authored-by: DeepSeek-v4-flash (wit Pi Coding Agent)
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

func TestAppendJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	for _, v := range []any{map[string]int{"n": 1}, map[string]int{"n": 2}} {
		if err := AppendJSONL(path, v); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	for i, l := range lines {
		var m map[string]int
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["n"] != i+1 {
			t.Errorf("line %d = %q", i, l)
		}
	}
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

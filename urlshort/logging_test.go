// Copyright (c) 2026 Aaron LI
//
// Logging wiring tests: console/error-file fan-out and server error routing.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testLogDay = "2026-09-11"

// newTestBundle builds a logging with a buffer console and, when withFile is
// true, an error file in dir.
func newTestBundle(t *testing.T, dir string, level slog.Level, console io.Writer, withFile bool) *logging {
	t.Helper()
	l := &logging{console: slog.NewTextHandler(console, &slog.HandlerOptions{Level: level})}
	if !withFile {
		return l
	}
	dl, err := newDailyLog(dir, "error-", ".log", 30, time.Hour,
		func() time.Time { return time.Date(2026, 9, 11, 5, 0, 0, 0, time.UTC) },
		slog.New(l.console))
	if err != nil {
		t.Fatal(err)
	}
	l.errorLog = dl
	l.file = slog.NewJSONHandler(dl, &slog.HandlerOptions{Level: level})
	return l
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			out = append(out, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLoggingWritesBothSinks(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newTestBundle(t, dir, slog.LevelDebug, &console, true)
	logger := l.logger()
	logger.Info("hello info")
	logger.Warn("hello warn")
	l.Close()

	lines := readLines(t, filepath.Join(dir, "error-"+testLogDay+".log"))
	if len(lines) != 2 {
		t.Fatalf("file lines = %d, want 2: %v", len(lines), lines)
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		for _, field := range []string{"time", "level", "msg"} {
			if _, ok := m[field]; !ok {
				t.Errorf("record %q missing %q", line, field)
			}
		}
	}
	if got := console.String(); !strings.Contains(got, "hello info") ||
		!strings.Contains(got, "hello warn") {
		t.Errorf("console missing records: %q", got)
	}
}

func TestLogLevelControlsBothSinks(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newTestBundle(t, dir, slog.LevelWarn, &console, true)
	logger := l.logger()
	logger.Info("nope")
	logger.Warn("yes")
	l.Close()

	lines := readLines(t, filepath.Join(dir, "error-"+testLogDay+".log"))
	if len(lines) != 1 || !strings.Contains(lines[0], "yes") {
		t.Fatalf("file lines = %v, want one 'yes'", lines)
	}
	if strings.Contains(console.String(), "nope") {
		t.Errorf("console contains filtered info: %q", console.String())
	}
	if !strings.Contains(console.String(), "yes") {
		t.Errorf("console missing warn: %q", console.String())
	}
}

func TestServerErrorLogRouting(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newTestBundle(t, dir, slog.LevelDebug, &console, true)
	elog := l.serverErrorLog()
	elog.Print("http: TLS handshake error from 1.2.3.4:5: EOF")
	elog.Print("http: panic serving 1.2.3.4:5: boom")
	l.Close()

	lines := readLines(t, filepath.Join(dir, "error-"+testLogDay+".log"))
	if len(lines) != 2 {
		t.Fatalf("file lines = %d, want 2: %v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"handshake error", "panic serving", `"comp":"http_server"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("file missing %q: %s", want, joined)
		}
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		if msg, _ := m["msg"].(string); strings.HasSuffix(msg, "\n") {
			t.Errorf("message keeps trailing newline: %q", msg)
		}
	}
	// Handshake noise must not reach the console; the panic must.
	if strings.Contains(console.String(), "handshake error") {
		t.Errorf("console got handshake noise: %q", console.String())
	}
	if !strings.Contains(console.String(), "panic serving") {
		t.Errorf("console missing panic: %q", console.String())
	}
}

func TestServerErrorLogWithoutFileFallsBackToConsole(t *testing.T) {
	var console bytes.Buffer
	l := newTestBundle(t, t.TempDir(), slog.LevelDebug, &console, false)
	l.serverErrorLog().Print("http: TLS handshake error from 1.2.3.4:5: EOF")
	if !strings.Contains(console.String(), "handshake error") {
		t.Errorf("console missing handshake error: %q", console.String())
	}
}

func TestNewLoggingCreatesErrorFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.LogLevel = "debug"
	l, err := newLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if l.file == nil || l.errorLog == nil {
		t.Fatal("error log not enabled")
	}
	l.logger().Warn("boom")
	l.Close()

	day := time.Now().UTC().Format("2006-01-02")
	if _, err := os.Stat(filepath.Join(cfg.LogsDir(), "error-"+day+".jsonl")); err != nil {
		t.Fatalf("error log not created: %v", err)
	}
}

func TestNewLoggingDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.ErrorLog.Enabled = false
	l, err := newLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.file != nil || l.errorLog != nil {
		t.Fatal("error log should be disabled")
	}
}

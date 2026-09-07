// Copyright (c) 2026 Aaron LI
//
// Unit tests for the per-channel IRC log collector (log.go).  The store is
// tested directly, and the record-extraction conventions are checked against
// goirc's ParseLine (pure line parsing; no IRC server involved).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	irc "github.com/fluffle/goirc/client"
)

func newLogStoreAt(t *testing.T, dir string, interval time.Duration) *LogStore {
	t.Helper()
	s, err := NewLogStore(dir, interval)
	if err != nil {
		t.Fatalf("NewLogStore() error: %v", err)
	}
	return s
}

func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestLogStoreJSONL(t *testing.T) {
	dir := t.TempDir()
	s := newLogStoreAt(t, dir, time.Hour)

	ts := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.Record(LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#ch",
		Nick: "aly", User: "aly", Host: "mail.liwt.net", Text: `hi "bob" ☺`})
	s.Record(LogRecord{Timestamp: ts.Add(time.Second), Type: LogTypeJoin, Channel: "#ch",
		Nick: "bob_", User: "bob", Host: "h"})
	s.Stop()

	path := filepath.Join(dir, "#ch", "2026-09-06.jsonl")
	lines := readLogLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	// self field must be omitted for non-self records.
	if strings.Contains(strings.Join(lines, ""), `"self"`) {
		t.Errorf("self field present in non-self records: %v", lines)
	}
	var got LogRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#ch",
		Nick: "aly", User: "aly", Host: "mail.liwt.net", Text: `hi "bob" ☺`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record = %+v, want %+v", got, want)
	}
}

func TestLogStoreDailyRotationUTC(t *testing.T) {
	dir := t.TempDir()
	s := newLogStoreAt(t, dir, time.Hour)

	// Two events on the same UTC day, one on the next day.
	s.Record(LogRecord{Timestamp: time.Date(2026, 9, 6, 23, 59, 59, 0, time.UTC),
		Type: LogTypeMessage, Channel: "#ch", Nick: "a", Text: "x"})
	s.Record(LogRecord{Timestamp: time.Date(2026, 9, 7, 0, 0, 1, 0, time.UTC),
		Type: LogTypeMessage, Channel: "#ch", Nick: "a", Text: "y"})
	s.Stop()

	entries, err := os.ReadDir(filepath.Join(dir, "#ch"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"2026-09-06.jsonl", "2026-09-07.jsonl"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
}

func TestLogStoreSeparateChannels(t *testing.T) {
	dir := t.TempDir()
	s := newLogStoreAt(t, dir, time.Hour)

	ts := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	s.Record(LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#a", Nick: "x", Text: "1"})
	s.Record(LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#b", Nick: "y", Text: "2"})
	s.Stop()

	for _, ch := range []string{"#a", "#b"} {
		lines := readLogLines(t, filepath.Join(dir, ch, "2026-09-06.jsonl"))
		if len(lines) != 1 {
			t.Errorf("channel %s: %d lines, want 1", ch, len(lines))
		}
	}
}

func TestLogStorePeriodicFlush(t *testing.T) {
	dir := t.TempDir()
	s := newLogStoreAt(t, dir, 15*time.Millisecond)
	defer s.Stop()

	s.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeMessage,
		Channel: "#ch", Nick: "a", Text: "hello"})

	// The line must hit the disk by the periodic flush before Stop().
	deadline := time.Now().Add(2 * time.Second)
	chDir := filepath.Join(dir, "#ch")
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(chDir)
		if err == nil && len(entries) == 1 {
			if fi, err := entries[0].Info(); err == nil && fi.Size() > 0 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("periodic flush did not write the log line")
}

func TestLogStoreStopDropsRecords(t *testing.T) {
	dir := t.TempDir()
	s := newLogStoreAt(t, dir, time.Hour)
	ts := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	s.Record(LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#ch", Nick: "a", Text: "x"})
	s.Stop()
	s.Stop() // idempotent
	s.Record(LogRecord{Timestamp: ts, Type: LogTypeMessage, Channel: "#ch", Nick: "a", Text: "y"})

	lines := readLogLines(t, filepath.Join(dir, "#ch", "2026-09-06.jsonl"))
	if len(lines) != 1 {
		t.Errorf("got %d lines after Stop, want 1", len(lines))
	}
}

func TestLogStoreConcurrentAccess(t *testing.T) {
	s := newLogStoreAt(t, t.TempDir(), time.Hour)
	defer s.Stop()

	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func(g int) {
			ch := "#a"
			if g%2 == 1 {
				ch = "#b"
			}
			nick := string(rune('a' + g))
			for i := 0; i < 200; i++ {
				s.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeMessage,
					Channel: ch, Nick: nick, Text: "hello"})
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 4; g++ {
		<-done
	}
}

// TestParseLineConventions pins down the goirc Line.Args layouts that the
// log record extraction in irc.go relies on (PART/QUIT/KICK reasons, NICK
// old/new, MODE args, 353 NAMES, CTCP ACTION rewrite).
func TestParseLineConventions(t *testing.T) {
	tests := []struct {
		raw  string
		cmd  string
		nick string
		args []string
		text string
	}{
		{":aly!u@h PRIVMSG #ch :hello", "PRIVMSG", "aly", []string{"#ch", "hello"}, "hello"},
		{":aly!u@h PART #ch :bye", "PART", "aly", []string{"#ch", "bye"}, "bye"},
		{":aly!u@h PART #ch", "PART", "aly", []string{"#ch"}, "#ch"},
		{":aly!u@h QUIT :Quit: bye", "QUIT", "aly", []string{"Quit: bye"}, "Quit: bye"},
		{":aly!u@h QUIT", "QUIT", "aly", nil, ""},
		{":op!o@h KICK #ch bob :spam", "KICK", "op", []string{"#ch", "bob", "spam"}, "spam"},
		{":old!o@h NICK new", "NICK", "old", []string{"new"}, "new"},
		{":op!o@h MODE #ch +o bob", "MODE", "op", []string{"#ch", "+o", "bob"}, "bob"},
		{":op!o@h TOPIC #ch :new topic", "TOPIC", "op", []string{"#ch", "new topic"}, "new topic"},
		{":srv 353 me = #ch :bob @alice +carol", "353", "", []string{"me", "=", "#ch", "bob @alice +carol"}, "bob @alice +carol"},
		{":aly!u@h PRIVMSG #ch :\x01ACTION waves\x01", "ACTION", "aly", []string{"#ch", "waves"}, "waves"},
	}
	for _, tt := range tests {
		l := irc.ParseLine(tt.raw)
		if l == nil {
			t.Fatalf("ParseLine(%q) = nil", tt.raw)
		}
		if l.Cmd != tt.cmd || l.Nick != tt.nick {
			t.Errorf("ParseLine(%q): cmd=%q nick=%q, want %q/%q", tt.raw, l.Cmd, l.Nick, tt.cmd, tt.nick)
		}
		if !reflect.DeepEqual(l.Args, tt.args) {
			t.Errorf("ParseLine(%q): args=%v, want %v", tt.raw, l.Args, tt.args)
		}
		if l.Text() != tt.text {
			t.Errorf("ParseLine(%q): text=%q, want %q", tt.raw, l.Text(), tt.text)
		}
	}
}

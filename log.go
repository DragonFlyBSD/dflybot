// Copyright (c) 2026 Aaron LI
//
// Per-channel IRC log collector writing daily JSONL files.
//
// Layout (shares the seen database's data_dir):
//   data_dir/<channel>/YYYY-MM-DD.jsonl   (dates in UTC)
//
// One JSON object per line (JSONL) so the logs are easy to consume and
// analyze with other tools, e.g. for browsing and searching.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log record types (the JSON "type" field).
const (
	LogTypeMessage = "message" // PRIVMSG to channel
	LogTypeAction  = "action"  // CTCP ACTION (/me)
	LogTypeNotice  = "notice"  // NOTICE to channel
	LogTypeJoin    = "join"
	LogTypePart    = "part"
	LogTypeQuit    = "quit"
	LogTypeKick    = "kick"
	LogTypeNick    = "nick" // nick change (rename)
	LogTypeMode    = "mode" // channel mode change (e.g., op/voice)
	LogTypeTopic   = "topic"
)

// LogRecord is the per-line JSON structure of the channel logs.
//
// Common fields:
//   - Timestamp (ts): RFC3339 (UTC)
//   - Type: one of the LogType* constants
//   - Channel: the channel this line belongs to (also the per-file channel)
//   - Nick/User/Host: the actor; empty for server-originated lines
//   - Self: set on lines sent by the bot itself (logged via say())
//
// Type-specific fields:
//   - message/action/notice/topic: Text
//   - part/quit: Text is the (possibly empty) reason
//   - kick: Target is the kicked nick, Text the reason, Nick the kicker
//   - nick: From/To are the old/new nick
//   - mode: Modes ("+ov") and Targets (mode args)
type LogRecord struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Channel   string    `json:"channel"`
	Nick      string    `json:"nick,omitempty"`
	User      string    `json:"user,omitempty"`
	Host      string    `json:"host,omitempty"`
	Target    string    `json:"target,omitempty"`
	Text      string    `json:"text,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Modes     string    `json:"modes,omitempty"`
	Targets   []string  `json:"targets,omitempty"`
	Self      bool      `json:"self,omitempty"`
}

// channelLog is the open daily log file of one channel.
type channelLog struct {
	day string // "2006-01-02" (UTC)
	f   *os.File
	w   *bufio.Writer
}

// LogStore manages the per-channel daily log files.
type LogStore struct {
	dir      string
	interval time.Duration

	mu      sync.Mutex
	writers map[string]*channelLog // by channel
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewLogStore(dir string, interval time.Duration) (*LogStore, error) {
	if interval <= 0 {
		interval = defaultSeenFlush // the shared default flush interval
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dir, err)
	}

	s := &LogStore{
		dir:      dir,
		interval: interval,
		writers:  make(map[string]*channelLog),
		stop:     make(chan struct{}),
	}

	s.wg.Add(1)
	go s.flushLoop()

	return s, nil
}

// Stop flushes and closes all open log files, and stops the periodic flush.
// Idempotent; subsequent Record() calls are dropped.
func (s *LogStore) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		for ch, cl := range s.writers {
			s.closeLocked(ch, cl)
		}
		s.closed = true
	})
}

// Record appends one log line to the channel's daily log file, rotating to
// a new file when the UTC day changes.
func (s *LogStore) Record(rec LogRecord) {
	if rec.Channel == "" || rec.Type == "" {
		slog.Warn("invalid log record", "record", rec)
		return
	}

	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	rec.Timestamp = rec.Timestamp.UTC()

	b, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("Log record marshal failed", "record", rec, "error", err)
		return
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	day := rec.Timestamp.Format("2006-01-02")
	cl := s.writers[rec.Channel]
	if cl == nil || cl.day != day {
		if cl != nil {
			s.closeLocked(rec.Channel, cl)
		}
		cl = s.open(rec.Channel, day)
		if cl == nil {
			return
		}
		s.writers[rec.Channel] = cl
	}
	if _, err := cl.w.Write(b); err != nil {
		slog.Warn("Log record write failed", "channel", rec.Channel, "error", err)
	}
}

// open opens or creates the daily log file of a channel.
func (s *LogStore) open(ch, day string) *channelLog {
	dir := filepath.Join(s.dir, ch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("Log channel dir creation failed", "dir", dir, "error", err)
		return nil
	}
	fp := filepath.Join(dir, day+".jsonl")
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Warn("Log file open failed", "filepath", fp, "error", err)
		return nil
	}
	return &channelLog{day: day, f: f, w: bufio.NewWriterSize(f, 16*1024)}
}

func (s *LogStore) closeLocked(ch string, cl *channelLog) {
	if err := cl.w.Flush(); err != nil {
		slog.Warn("Log flush failed", "channel", ch, "error", err)
	}
	if err := cl.f.Close(); err != nil {
		slog.Warn("Log file close failed", "channel", ch, "error", err)
	}
	delete(s.writers, ch)
}

// flushLoop periodically flushes the buffered log lines to disk.
func (s *LogStore) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			for ch, cl := range s.writers {
				if err := cl.w.Flush(); err != nil {
					slog.Warn("Log flush failed", "channel", ch, "error", err)
				}
			}
			s.mu.Unlock()
		}
	}
}

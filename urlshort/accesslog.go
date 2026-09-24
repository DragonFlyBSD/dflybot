// Copyright (c) 2026 Aaron LI
//
// Access logging: one JSON object per line, daily UTC rotation, bounded
// retention.
//
// AccessLogger marshals records and hands them to a shared dailyLog, which owns
// the file, rotation, and the single writer goroutine (C14).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Access log record types.
const (
	AccessTypeRedirect = "redirect"
	AccessTypeAPI      = "api"
	AccessTypeACME     = "acme"
	AccessTypeHome     = "home"
)

// AccessEntry is one JSONL record.
type AccessEntry struct {
	Timestamp  time.Time `json:"ts"`
	Type       string    `json:"type"`
	RemoteIP   string    `json:"remote_ip"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Key        string    `json:"key,omitempty"`
	Status     int       `json:"status"`
	Target     string    `json:"target,omitempty"`
	Rule       string    `json:"rule,omitempty"`
	Client     string    `json:"client,omitempty"`
	Action     string    `json:"action,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	Referer    string    `json:"referer,omitempty"`
	DurationMS float64   `json:"duration_ms"`
	Bytes      int64     `json:"bytes"`
}

// AccessLogStats summarises the on-disk logs for the status endpoint.
type AccessLogStats struct {
	CurrentFile      string `json:"current_file"`
	CurrentSizeBytes int64  `json:"current_size_bytes"`
	OldestRetained   string `json:"oldest_retained"`
	Files            int    `json:"files"`
}

// AccessLogger is the asynchronous JSONL access log writer.
type AccessLogger struct {
	base *slog.Logger
	log  *dailyLog
}

// NewAccessLogger creates the log directory, prunes old files, and starts the
// writer goroutine.
func NewAccessLogger(
	dir string,
	retentionDays int,
	flushInterval time.Duration,
	now func() time.Time,
	base *slog.Logger,
) (*AccessLogger, error) {
	if base == nil {
		base = slog.Default()
	}
	dl, err := newDailyLog(dir, "access-", ".jsonl", retentionDays,
		flushInterval, now, base.With(slog.String("comp", "access_logger")))
	if err != nil {
		return nil, err
	}
	return &AccessLogger{base: base, log: dl}, nil
}

// Log enqueues an entry without blocking. A full channel drops the entry.
func (l *AccessLogger) Log(e AccessEntry) {
	if e.Timestamp.IsZero() {
		e.Timestamp = l.log.now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		l.base.Warn("access log marshal failed", "error", err)
		return
	}
	l.log.enqueue(b)
}

// Dropped returns the number of dropped entries.
func (l *AccessLogger) Dropped() int64 {
	return l.log.Dropped()
}

// Close stops accepting entries, drains the channel, flushes, and closes the
// file. It is safe to call more than once.
func (l *AccessLogger) Close() {
	l.log.Close()
}

// Stats summarises the log directory.
func (l *AccessLogger) Stats() AccessLogStats {
	s := l.log.stats()
	return AccessLogStats{
		CurrentFile:      s.CurrentFile,
		CurrentSizeBytes: s.CurrentSizeBytes,
		OldestRetained:   s.OldestRetained,
		Files:            s.Files,
	}
}

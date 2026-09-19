// Copyright (c) 2026 Aaron LI
//
// Access logging: one JSON object per line, daily UTC rotation, bounded
// retention.
//
// A single writer goroutine owns the open file and the buffered writer.
// Requests only push entries into a bounded channel; on overflow entries are
// dropped and counted so request handling can never stall (C14).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Access log record types.
const (
	AccessTypeRedirect = "redirect"
	AccessTypeAPI      = "api"
	AccessTypeACME     = "acme"
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
	dir           string
	retentionDays int
	flushInterval time.Duration
	logger        *slog.Logger
	now           func() time.Time

	ch      chan AccessEntry
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once
	closed  atomic.Bool
	dropped atomic.Int64
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
	logger := base.With(slog.String("comp", "access_logger"))
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create access log dir %q: %w", dir, err)
	}
	l := &AccessLogger{
		dir:           dir,
		retentionDays: retentionDays,
		flushInterval: flushInterval,
		logger:        logger,
		now:           now,
		ch:            make(chan AccessEntry, 4096),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	l.cleanup()
	go l.run()
	return l, nil
}

// Log enqueues an entry without blocking. A full channel drops the entry.
func (l *AccessLogger) Log(e AccessEntry) {
	if l.closed.Load() {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = l.now()
	}
	select {
	case l.ch <- e:
	default:
		n := l.dropped.Add(1)
		if n == 1 || n%1000 == 0 {
			l.logger.Warn("access log overflow, dropping entries", "dropped", n)
		}
	}
}

// Dropped returns the number of dropped entries.
func (l *AccessLogger) Dropped() int64 { return l.dropped.Load() }

// Close stops accepting entries, drains the channel, flushes, and closes the
// file. It is safe to call more than once.
func (l *AccessLogger) Close() {
	l.stopped.Do(func() {
		l.closed.Store(true)
		close(l.stop)
		<-l.done
	})
}

func (l *AccessLogger) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.flushInterval)
	defer ticker.Stop()

	var (
		fileDate string
		file     *os.File
		w        *bufio.Writer
	)
	closeFile := func() {
		if w != nil {
			if err := w.Flush(); err != nil {
				l.logger.Warn("access log flush failed", "error", err)
			}
		}
		if file != nil {
			if err := file.Close(); err != nil {
				l.logger.Warn("access log close failed", "error", err)
			}
		}
		file, w = nil, nil
		fileDate = ""
	}
	rotate := func(t time.Time) {
		day := t.UTC().Format("2006-01-02")
		if file != nil && day == fileDate {
			return
		}
		closeFile()
		fp := filepath.Join(l.dir, "access-"+day+".jsonl")
		f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			l.logger.Warn("access log open failed", "file", fp, "error", err)
			return
		}
		file, w, fileDate = f, bufio.NewWriterSize(f, 16*1024), day
	}
	write := func(e AccessEntry) {
		rotate(l.now())
		if w == nil {
			return
		}
		b, err := json.Marshal(e)
		if err != nil {
			l.logger.Warn("access log marshal failed", "error", err)
			return
		}
		b = append(b, '\n')
		if _, err := w.Write(b); err != nil {
			l.logger.Warn("access log write failed", "error", err)
		}
	}

	defer closeFile()
	// Retention runs at startup (in NewAccessLogger) and once per UTC day.
	lastCleanupDay := l.now().UTC().Format("2006-01-02")
	for {
		select {
		case e := <-l.ch:
			write(e)
		case <-ticker.C:
			if w != nil {
				if err := w.Flush(); err != nil {
					l.logger.Warn("access log flush failed", "error", err)
				}
			}
			if day := l.now().UTC().Format("2006-01-02"); day != lastCleanupDay {
				l.cleanup()
				lastCleanupDay = day
			}
		case <-l.stop:
			for {
				select {
				case e := <-l.ch:
					write(e)
				default:
					return
				}
			}
		}
	}
}

// cleanup deletes log files older than retention_days. Failures only warn.
func (l *AccessLogger) cleanup() {
	if l.retentionDays <= 0 {
		return
	}
	cutoff := l.now().UTC().AddDate(0, 0, -l.retentionDays)
	files, err := logFiles(l.dir)
	if err != nil {
		l.logger.Warn("access log retention listing failed", "error", err)
		return
	}
	for _, f := range files {
		if f.date.Before(cutoff) {
			if err := os.Remove(f.path); err != nil {
				l.logger.Warn("access log retention delete failed",
					"file", f.path, "error", err)
			}
		}
	}
}

type dayFile struct {
	path string
	date time.Time
}

func logFiles(dir string) ([]dayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []dayFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "access-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, "access-"), ".jsonl")
		t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
		if err != nil {
			continue
		}
		out = append(out, dayFile{path: filepath.Join(dir, name), date: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.Before(out[j].date) })
	return out, nil
}

// Stats summarises the log directory.
func (l *AccessLogger) Stats() AccessLogStats {
	stats := AccessLogStats{}
	files, err := logFiles(l.dir)
	if err != nil {
		return stats
	}
	stats.Files = len(files)
	if len(files) > 0 {
		stats.OldestRetained = files[0].date.Format("2006-01-02")
	}
	day := l.now().UTC().Format("2006-01-02")
	stats.CurrentFile = filepath.Join(l.dir, "access-"+day+".jsonl")
	if fi, err := os.Stat(stats.CurrentFile); err == nil {
		stats.CurrentSizeBytes = fi.Size()
	}
	return stats
}

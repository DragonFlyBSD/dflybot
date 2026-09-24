// Copyright (c) 2026 Aaron LI
//
// Daily-rotated, retained log files. This is the shared rotation core used by
// the access log and the server error log.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bufio"
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

// dailyLog is a set of daily-rotated, retained log files written by a single
// goroutine. It implements io.Writer so a slog handler can feed records.
//
// Callers only enqueue opaque lines; formatting is their responsibility. A
// bounded channel decouples writers from disk I/O: on overflow a record is
// dropped and counted so logging can never stall the caller (C14).
type dailyLog struct {
	dir           string
	prefix        string
	suffix        string
	retentionDays int
	flushInterval time.Duration
	logger        *slog.Logger
	now           func() time.Time

	ch      chan []byte
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once
	closed  atomic.Bool
	dropped atomic.Int64
}

// newDailyLog creates the directory, prunes obsolete files, and starts the
// writer goroutine.
func newDailyLog(
	dir, prefix, suffix string,
	retentionDays int,
	flushInterval time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) (*dailyLog, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", dir, err)
	}
	l := &dailyLog{
		dir:           dir,
		prefix:        prefix,
		suffix:        suffix,
		retentionDays: retentionDays,
		flushInterval: flushInterval,
		logger:        logger,
		now:           now,
		ch:            make(chan []byte, 4096),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	l.cleanup()
	go l.run()
	return l, nil
}

// Write enqueues one record without blocking. The bytes are copied because slog
// handlers may reuse their buffers after Write returns.
func (l *dailyLog) Write(p []byte) (int, error) {
	if l.closed.Load() {
		return len(p), nil
	}
	b := make([]byte, len(p))
	copy(b, p)
	l.enqueue(b)
	return len(p), nil
}

// enqueue adds an already-owned record without copying. The caller must not
// reuse b. A trailing newline is added by the writer if missing.
func (l *dailyLog) enqueue(b []byte) {
	if l.closed.Load() {
		return
	}
	select {
	case l.ch <- b:
	default:
		dropped := l.dropped.Add(1)
		if dropped == 1 || dropped%1000 == 0 {
			l.logger.Warn("log overflow, dropping records",
				"prefix", l.prefix, "dropped", dropped)
		}
	}
}

// Dropped returns the number of records dropped on overflow.
func (l *dailyLog) Dropped() int64 {
	return l.dropped.Load()
}

// Close stops accepting records, drains the channel, flushes, and closes the
// file. It is safe to call more than once.
func (l *dailyLog) Close() {
	l.stopped.Do(func() {
		l.closed.Store(true)
		close(l.stop)
		<-l.done
	})
}

func (l *dailyLog) run() {
	var (
		fp       string
		fileDate string
		file     *os.File
		w        *bufio.Writer
	)
	openFile := func(day string) {
		fp = filepath.Join(l.dir, l.prefix+day+l.suffix)
		f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			l.logger.Warn("log open failed", "file", fp, "error", err)
			return
		}
		fileDate, file, w = day, f, bufio.NewWriterSize(f, 16*1024)
		l.logger.Info("log opened", "file", fp)
	}
	closeFile := func() {
		if w != nil {
			if err := w.Flush(); err != nil {
				l.logger.Warn("log flush failed", "file", fp, "error", err)
			}
		}
		if file != nil {
			if err := file.Close(); err != nil {
				l.logger.Warn("log close failed", "file", fp, "error", err)
			}
		}
		fileDate, file, w = "", nil, nil
	}
	write := func(b []byte) {
		day := l.now().UTC().Format("2006-01-02")
		if file == nil {
			openFile(day)
		} else if day != fileDate {
			closeFile()
			openFile(day)
		}
		if w == nil {
			l.logger.Warn("log file not opened", "file", fp)
			return
		}
		if len(b) == 0 || b[len(b)-1] != '\n' {
			b = append(b, '\n')
		}
		if _, err := w.Write(b); err != nil {
			l.logger.Warn("log write failed", "file", fp, "error", err)
		}
	}

	defer close(l.done)
	defer closeFile()

	// Retention runs at startup (in newDailyLog) and once per UTC day.
	lastCleanupDay := l.now().UTC().Format("2006-01-02")
	ticker := time.NewTicker(l.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case b := <-l.ch:
			write(b)
		case <-ticker.C:
			if w != nil {
				if err := w.Flush(); err != nil {
					l.logger.Warn("log flush failed", "file", fp, "error", err)
				}
			}
			if day := l.now().UTC().Format("2006-01-02"); day != lastCleanupDay {
				l.cleanup()
				lastCleanupDay = day
			}
		case <-l.stop:
			for {
				select {
				case b := <-l.ch:
					write(b)
				default:
					return
				}
			}
		}
	}
}

// cleanup deletes log files older than retentionDays. Failures only warn.
func (l *dailyLog) cleanup() {
	if l.retentionDays <= 0 {
		return
	}
	cutoff := l.now().UTC().AddDate(0, 0, -l.retentionDays)
	files, err := l.logFiles()
	if err != nil {
		l.logger.Warn("log retention listing failed",
			"prefix", l.prefix, "error", err)
		return
	}
	for _, f := range files {
		if f.date.Before(cutoff) {
			if err := os.Remove(f.path); err != nil {
				l.logger.Warn("log retention delete failed",
					"file", f.path, "error", err)
			} else {
				l.logger.Info("log retention removed", "file", f.path)
			}
		}
	}
}

type dayFile struct {
	path string
	date time.Time
}

func (l *dailyLog) logFiles() ([]dayFile, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []dayFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, l.prefix) ||
			!strings.HasSuffix(name, l.suffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, l.prefix), l.suffix)
		t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
		if err != nil {
			continue
		}
		out = append(out, dayFile{
			path: filepath.Join(l.dir, name),
			date: t,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.Before(out[j].date) })
	return out, nil
}

// logFileStats summarises the files owned by one dailyLog.
type logFileStats struct {
	CurrentFile      string
	CurrentSizeBytes int64
	OldestRetained   string
	Files            int
}

// stats summarises the files owned by this dailyLog.
func (l *dailyLog) stats() logFileStats {
	stats := logFileStats{}
	files, err := l.logFiles()
	if err != nil {
		return stats
	}
	stats.Files = len(files)
	if len(files) > 0 {
		stats.OldestRetained = files[0].date.Format("2006-01-02")
	}
	day := l.now().UTC().Format("2006-01-02")
	stats.CurrentFile = filepath.Join(l.dir, l.prefix+day+l.suffix)
	if fi, err := os.Stat(stats.CurrentFile); err == nil {
		stats.CurrentSizeBytes = fi.Size()
	}
	return stats
}

// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 Aaron LI
//
// Shared monitor scaffolding: signal handling, poll loop, state/history
// persistence, and log level setup.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// SignalContext returns a context that is cancelled on SIGINT or SIGTERM
// (the caller must call cancel to release resources).
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		slog.Info("signal received, shutting down...")
		cancel()
	}()
	return ctx, cancel
}

// Loop runs poll immediately and then every interval, until ctx is done.
func Loop(ctx context.Context, interval time.Duration, poll func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		poll()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SaveJSON atomically writes v as indented JSON to path (tmp file + rename).
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// ReadJSON loads v from path.  It returns (false, nil) if the file does not
// exist.
func ReadJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, err
	}
	return true, nil
}

// History buffers JSONL history lines and appends them to the file at path
// in batches: records are marshalled and buffered by Append(), and written
// with a single file write (plus fsync) by Flush(), e.g. once per poll
// round.  Close() flushes any remaining lines on shutdown and prevents
// further Appends.  No log rotation is performed.
type History struct {
	path string

	mu     sync.Mutex
	lines  [][]byte // marshalled lines, each terminated with '\n'
	closed bool
}

// ErrClosed is returned by Append after Close.
var ErrClosed = errors.New("monitor: history closed")

func NewHistory(path string) *History {
	return &History{path: path}
}

// Append marshals v and buffers it as one history line.
func (h *History) Append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	h.lines = append(h.lines, append(b, '\n'))
	return nil
}

// Flush writes all buffered lines to the history file with a single file
// write (plus fsync) and clears the buffer.  On error the buffered lines
// are dropped; callers should log the failure and go on (the next poll
// starts a fresh batch).
func (h *History) Flush() error {
	h.mu.Lock()
	if len(h.lines) == 0 {
		h.mu.Unlock()
		return nil
	}
	lines := h.lines
	h.lines = nil
	h.mu.Unlock()

	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		return err
	}
	return f.Sync()
}

// Close flushes any buffered lines and prevents further Appends.
func (h *History) Close() error {
	err := h.Flush()
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	return err
}

// LogLevel sets the log level from the config name ("debug", "info", "warn"
// or "error"); unknown names are logged as a warning and left unchanged.
func LogLevel(lv *slog.LevelVar, name string) {
	switch name {
	case "debug":
		lv.Set(slog.LevelDebug)
	case "info":
		lv.Set(slog.LevelInfo)
	case "warn":
		lv.Set(slog.LevelWarn)
	case "error":
		lv.Set(slog.LevelError)
	default:
		slog.Warn("unknown log level", "level", name)
	}
}

// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 Aaron LI
//
// Shared monitor scaffolding: signal handling, poll loop, state/history
// persistence, and log level setup.
//
// Co-authored-by: DeepSeek-v4-flash (wit Pi Coding Agent)
//

package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/signal"
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

// AppendJSONL appends one line (v marshalled as JSON) to the file at path,
// creating it if needed.
func AppendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
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

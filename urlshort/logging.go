// Copyright (c) 2026 Aaron LI
//
// Logging wiring: a console handler (stderr) plus an optional JSONL server
// error log file, and the loggers derived from them.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"
)

// logging bundles the two log destinations and the loggers derived from them.
type logging struct {
	level    slog.Level
	console  slog.Handler
	file     slog.Handler // nil when [error_log].enabled = false
	errorLog *dailyLog    // nil when disabled; owned here
}

// newLogging builds the console handler and, when enabled, the JSONL error log.
func newLogging(cfg *Config) (*logging, error) {
	l := newConsoleLogging(cfg.LogLevel)
	if !cfg.ErrorLog.Enabled {
		return l, nil
	}
	dl, err := newDailyLog(cfg.LogsDir(), "error-", ".jsonl",
		cfg.ErrorLog.RetentionDays,
		time.Duration(cfg.ErrorLog.FlushInterval)*time.Second,
		nil, slog.New(l.console).With(slog.String("comp", "error_logger")))
	if err != nil {
		return nil, err
	}
	l.errorLog = dl
	l.file = slog.NewJSONHandler(dl, &slog.HandlerOptions{Level: l.level})
	return l, nil
}

// newConsoleLogging returns a console-only logging, used as the fallback when
// no bundle is supplied.
func newConsoleLogging(level string) *logging {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return &logging{
		level:   lvl,
		console: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}),
	}
}

// logger returns the logger for the program's own records.
func (l *logging) logger() *slog.Logger {
	if l.file == nil {
		return slog.New(l.console)
	}
	return slog.New(&multiHandler{handlers: []slog.Handler{l.console, l.file}})
}

// serverErrorLog returns the *log.Logger for http.Server.ErrorLog.
func (l *logging) serverErrorLog() *log.Logger {
	h := slog.Handler(&serverErrorHandler{
		console: l.console,
		file:    l.file,
	})
	h = h.WithAttrs([]slog.Attr{slog.String("comp", "http_server")})
	return slog.NewLogLogger(h, slog.LevelWarn)
}

// Close closes the error log file, if any.
func (l *logging) Close() {
	if l.errorLog != nil {
		l.errorLog.Close()
	}
}

// multiHandler fans a record out to several handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, x := range h.handlers {
		if x.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, x := range h.handlers {
		if !x.Enabled(ctx, r.Level) {
			continue
		}
		if err := x.Handle(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := &multiHandler{handlers: make([]slog.Handler, len(h.handlers))}
	for i, x := range h.handlers {
		out.handlers[i] = x.WithAttrs(attrs)
	}
	return out
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	out := &multiHandler{handlers: make([]slog.Handler, len(h.handlers))}
	for i, x := range h.handlers {
		out.handlers[i] = x.WithGroup(name)
	}
	return out
}

// serverErrorHandler routes http.Server.ErrorLog records. Client-induced TLS
// handshake errors go to the error file only; any other server error also
// reaches the console. When the error file is disabled, everything falls back
// to the console.
type serverErrorHandler struct {
	console slog.Handler
	file    slog.Handler
}

func (h *serverErrorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.file != nil && h.file.Enabled(ctx, level) {
		return true
	}
	return h.console.Enabled(ctx, level)
}

func (h *serverErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	if h.file != nil {
		if err := h.file.Handle(ctx, r); err != nil {
			firstErr = err
		}
		// Log client-induced TLS handshake noise to error file only.
		if strings.HasPrefix(r.Message, "http: TLS handshake error") {
			return firstErr
		}
	}
	if err := h.console.Handle(ctx, r); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (h *serverErrorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := &serverErrorHandler{console: h.console.WithAttrs(attrs)}
	if h.file != nil {
		out.file = h.file.WithAttrs(attrs)
	}
	return out
}

func (h *serverErrorHandler) WithGroup(name string) slog.Handler {
	out := &serverErrorHandler{console: h.console.WithGroup(name)}
	if h.file != nil {
		out.file = h.file.WithGroup(name)
	}
	return out
}

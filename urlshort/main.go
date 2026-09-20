// Copyright (c) 2026 Aaron LI
//
// urlshort entry point: configuration, wiring, certificate pre-warm, signal
// handling, and orderly shutdown.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	configPath := flag.String("config", "urlshort.toml", "path to the configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", programName, version)
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		logger.Warn("config warning", "message", w)
	}

	status := NewStatusState()
	certs, err := buildCertManager(cfg, status, logger)
	if err != nil {
		return err
	}

	store, err := OpenBoltStore(filepath.Join(cfg.DataDir, "links.db"),
		cfg.Backup.CompactTxMaxBytes)
	if err != nil {
		return err
	}

	srv, err := NewServer(cfg, store, certs, status, logger)
	if err != nil {
		store.Close()
		return err
	}

	// Shutdown order (C16): Serve drains the listeners, then the store is
	// closed and finally the access log is flushed and closed.
	defer func() {
		store.Close()
		srv.Close()
	}()

	maint := NewMaintenance(cfg, store, logger)
	srv.SetMaintenance(maint)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	maint.Start(ctx)
	defer maint.Stop()

	if cfg.ACME.Enabled {
		prewarm(certs, cfg.PublicHost(), status, logger)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		runErr = <-serveErr
	case runErr = <-serveErr:
		stop()
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	logger.Info("shutdown complete")
	return nil
}

func newLogger(level string) *slog.Logger {
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
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

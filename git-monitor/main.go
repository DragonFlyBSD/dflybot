// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Simple git monitor for DragonFly BSD that reports new commits to IRC via
// dflybot's webhook.
//

package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/go-playground/validator/v10"

	"github.com/liweitianux/dflybot/monitor"
)

// Only need to use a single instance of Validate, which caches struct info.
var validate = validator.New(validator.WithRequiredStructEnabled())

type Config struct {
	// Logger level: debug, info, warn, error
	LogLevel string `toml:"log_level" validate:"required,oneof=debug info warn error"`
	// Work directory to hold the repos and states.
	// NOTE: Must end with a slash (/) as required by the 'dirpath' validator.
	DataDir string `toml:"data_dir" validate:"dirpath"`
	// Webhook settings
	Webhook monitor.ConfigWebhook `toml:"webhook" validate:"required"`
	// URL shortener settings (optional; when absent the full commit URL is
	// announced)
	URLShort *monitor.ConfigURLShort `toml:"urlshort"`
	// List of monitor repos
	Repos []ConfigRepo `toml:"repos" validate:"required"`
}

type ConfigRepo struct {
	// Whether enabled?
	Enabled bool `toml:"enabled"`
	// Name of this repo (also used as the directory name)
	Name string `toml:"name" validate:"required"`
	// URL to clone the repo
	URL string `toml:"url" validate:"required"`
	// CommitURL is a text/template for the web URL of a commit, e.g.
	// "https://gitweb.dragonflybsd.org/dragonfly.git/commit/{{ .Hash }}".
	// The rendered URL is shortened when [urlshort] is configured.
	CommitURL string `toml:"commit_url" validate:"required"`
	// Poll interval in seconds
	Interval int `toml:"interval" validate:"required,min=1"`
}

func main() {
	logLevel := &slog.LevelVar{} // INFO
	logOpts := &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, logOpts))
	slog.SetDefault(logger)

	configFile := flag.String("config", "git-monitor.toml", "configuration file")
	isDebug := flag.Bool("debug", false, "debug mode")
	flag.Parse()

	if *isDebug {
		logLevel.Set(slog.LevelDebug)
	}

	config := Config{}
	if _, err := toml.DecodeFile(*configFile, &config); err != nil {
		slog.Error("failed to read config", "file", *configFile, "error", err)
		os.Exit(1)
	}
	slog.Debug("read config", "file", *configFile, "data", config)

	if err := validate.Struct(&config); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	if *isDebug {
		config.LogLevel = "debug"
	}
	if config.LogLevel != "" {
		monitor.LogLevel(logLevel, config.LogLevel)
	}

	if fi, err := os.Stat(config.DataDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("data directory not exists", "dir", config.DataDir)
		} else {
			slog.Error("data directory stat failed", "dir", config.DataDir, "error", err)
		}
		os.Exit(1)
	} else if !fi.IsDir() {
		slog.Error("not a directory", "data_dir", config.DataDir)
		os.Exit(1)
	}

	var shortener monitor.Shortener
	if config.URLShort != nil {
		s, err := monitor.NewURLShortener(config.URLShort)
		if err != nil {
			slog.Error("invalid urlshort config", "error", err)
			os.Exit(1)
		}
		shortener = s
	}

	webhook := monitor.NewWebhook(&config.Webhook)

	monitors := make([]*Monitor, 0, len(config.Repos))
	for _, repo := range config.Repos {
		if !repo.Enabled {
			slog.Info("skip disabled repo", "name", repo.Name, "url", repo.URL)
			continue
		}
		m, err := NewMonitor(&MonitorConfig{
			Name:      repo.Name,
			RepoURL:   repo.URL,
			CommitURL: repo.CommitURL,
			RepoDir:   filepath.Join(config.DataDir, repo.Name+".git"),
			StatePath: filepath.Join(config.DataDir, repo.Name+".state"),
			Interval:  time.Duration(repo.Interval) * time.Second,
			Poster:    webhook,
			Shortener: shortener,
		}, nil)
		if err != nil {
			slog.Error("invalid monitor config", "repo", repo.Name, "error", err)
			os.Exit(1)
		}
		monitors = append(monitors, m)
	}

	// Setup context and signal handling
	ctx, cancel := monitor.SignalContext()
	defer cancel()

	var wg sync.WaitGroup
	for _, m := range monitors {
		wg.Add(1)
		go m.Start(ctx, &wg)
	}

	wg.Wait()
	slog.Info("all monitors exited")
}

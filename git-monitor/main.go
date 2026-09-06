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

	// Setup context and signal handling
	ctx, cancel := monitor.SignalContext()
	defer cancel()

	webhook := monitor.NewWebhook(&config.Webhook)
	wg := &sync.WaitGroup{}

	for _, repo := range config.Repos {
		if !repo.Enabled {
			slog.Info("skip disabled repo", "name", repo.Name, "url", repo.URL)
			continue
		}
		monitor := NewMonitor(&MonitorConfig{
			Name:      repo.Name,
			RepoURL:   repo.URL,
			RepoDir:   filepath.Join(config.DataDir, repo.Name+".git"),
			StatePath: filepath.Join(config.DataDir, repo.Name+".state"),
			Interval:  time.Duration(repo.Interval) * time.Second,
			Poster:    webhook,
		}, nil)
		wg.Add(1)
		go monitor.Start(ctx, wg)
	}

	wg.Wait()
	slog.Info("all monitors exited")
}

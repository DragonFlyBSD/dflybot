// Copyright (c) 2026 Aaron LI
//
// Redmine monitor that announces bugtracker issue activity to IRC via
// dflybot's webhook.
//
// It polls the project activity Atom feed (one request per project per
// poll, deduplicated against a per-project watermark) and announces the
// configured issue actions, batched per poll.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/go-playground/validator/v10"

	"github.com/liweitianux/dflybot/monitor"
)

// Only need to use a single instance of Validate, which caches struct info.
var validate = validator.New(validator.WithRequiredStructEnabled())

type Config struct {
	// Logger level: debug, info, warn, error
	LogLevel string `toml:"log_level" validate:"required,oneof=debug info warn error"`
	// Work directory to hold the per-project state and history files.
	// NOTE: Must end with a slash (/) as required by the 'dirpath' validator.
	DataDir string `toml:"data_dir" validate:"dirpath"`
	// Webhook settings.
	Webhook monitor.ConfigWebhook `toml:"webhook" validate:"required"`
	// List of Redmine projects to monitor.
	Projects []ConfigProject `toml:"projects" validate:"required,min=1,dive"`
}

type ConfigProject struct {
	// Whether enabled?
	Enabled bool `toml:"enabled"`
	// Unique name (also names the state/history files).
	Name string `toml:"name" validate:"required"`
	// URL of the project activity Atom feed, e.g.:
	// https://bugs.dragonflybsd.org/projects/dragonfly/activity.atom?key=...
	FeedURL string `toml:"feed_url" validate:"required,url"`
	// Poll interval in seconds.
	Interval int `toml:"interval" validate:"required,min=1"`
	// Actions to announce; empty means all supported actions
	// (create, comment, close, resolve, reopen, update).
	Actions []string `toml:"actions" validate:"omitempty,dive,oneof=create comment close resolve reopen update"`
}

func main() {
	logLevel := &slog.LevelVar{} // INFO
	logOpts := &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, logOpts))
	slog.SetDefault(logger)

	configFile := flag.String("config", "redmine-monitor.toml", "configuration file")
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
	if err := checkNames(config.Projects); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	if *isDebug {
		config.LogLevel = "debug"
	}
	if config.LogLevel != "" {
		monitor.LogLevel(logLevel, config.LogLevel)
	}

	if err := os.MkdirAll(config.DataDir, 0o755); err != nil {
		slog.Error("data directory creation failed", "dir", config.DataDir, "error", err)
		os.Exit(1)
	}

	// Setup context and signal handling.
	ctx, cancel := monitor.SignalContext()
	defer cancel()

	client := newAtomClient()
	webhook := monitor.NewWebhook(&config.Webhook)
	wg := &sync.WaitGroup{}

	for i := range config.Projects {
		project := &config.Projects[i]
		if !project.Enabled {
			slog.Info("skip disabled project", "name", project.Name)
			continue
		}
		mon := NewProjectMonitor(project, client, webhook, config.DataDir, nil)
		wg.Add(1)
		go mon.Start(ctx, wg)
	}

	wg.Wait()
	slog.Info("all monitors exited")
}

// checkNames verifies that the enabled project names are unique.
func checkNames(projects []ConfigProject) error {
	seen := make(map[string]bool)
	for _, project := range projects {
		if !project.Enabled {
			continue
		}
		if seen[project.Name] {
			return fmt.Errorf("duplicate project name %q", project.Name)
		}
		seen[project.Name] = true
	}
	return nil
}

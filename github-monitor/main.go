// Copyright (c) 2026 Aaron LI
//
// GitHub monitor that announces issue/PR activity of the configured repos
// to IRC via dflybot's webhook.
//
// It polls the repository Events API (one request per repo per poll,
// deduplicated against a per-repo watermark) and announces the configured
// issue and pull request actions, batched per repo per poll.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"flag"
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
	// Work directory to hold the per-repo state and history files.
	// NOTE: Must end with a slash (/) as required by the 'dirpath' validator.
	DataDir string `toml:"data_dir" validate:"dirpath"`
	// GitHub settings (optional token).
	GitHub ConfigGitHub `toml:"github"`
	// List of repos to monitor.
	Repos []ConfigRepo `toml:"repos" validate:"required,min=1,dive"`
	// Webhook settings.
	Webhook monitor.ConfigWebhook `toml:"webhook" validate:"required"`
}

type ConfigGitHub struct {
	// API token; optional for public repos but recommended (rate limits),
	// and required for private repos.
	Token string `toml:"token"`
}

type ConfigRepo struct {
	// Whether enabled?
	Enabled bool `toml:"enabled"`
	// Project (owner) and repository name.
	Project string `toml:"project" validate:"required"`
	Repo    string `toml:"repo" validate:"required"`
	// Issue actions to announce; empty means all supported actions.
	IssueActions []string `toml:"issue_actions" validate:"omitempty,dive,oneof=create comment close reopen"`
	// Pull request actions to announce; empty means all supported actions.
	PRActions []string `toml:"pr_actions" validate:"omitempty,dive,oneof=create comment update close merge reopen"`
	// Poll interval in seconds.
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

	configFile := flag.String("config", "github-monitor.toml", "configuration file")
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

	if err := os.MkdirAll(config.DataDir, 0o755); err != nil {
		slog.Error("data directory creation failed", "dir", config.DataDir, "error", err)
		os.Exit(1)
	}

	// Setup context and signal handling.
	ctx, cancel := monitor.SignalContext()
	defer cancel()

	github := newGitHubClient(config.GitHub.Token)
	webhook := monitor.NewWebhook(&config.Webhook)
	wg := &sync.WaitGroup{}

	for _, repo := range config.Repos {
		if !repo.Enabled {
			slog.Info("skip disabled repo", "project", repo.Project, "repo", repo.Repo)
			continue
		}
		mon := NewRepoMonitor(&repo, github, webhook, config.DataDir, nil)
		wg.Add(1)
		go mon.Start(ctx, wg)
	}

	wg.Wait()
	slog.Info("all monitors exited")
}

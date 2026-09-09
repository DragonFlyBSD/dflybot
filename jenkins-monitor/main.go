// Copyright (c) 2026 Aaron LI
//
// Jenkins CI monitor that reports build failures, recoveries, and node
// offline/online changes to IRC via dflybot's webhook.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// Work directory to hold the state and history files.
	// NOTE: Must end with a slash (/) as required by the 'dirpath' validator.
	DataDir string `toml:"data_dir" validate:"dirpath"`
	// The Jenkins to monitor.
	Jenkins ConfigJenkins `toml:"jenkins" validate:"required"`
	// Webhook settings
	Webhook monitor.ConfigWebhook `toml:"webhook" validate:"required"`
}

type ConfigJenkins struct {
	// Instance name (also used to name the state/history files)
	Name string `toml:"name" validate:"required"`
	// Base URL, e.g., "https://ci.dragonflybsd.org/"
	URL string `toml:"url" validate:"required,url"`
	// Poll and state/history flush interval in seconds
	Interval int `toml:"interval" validate:"required,min=1"`
	// List of jobs to monitor
	Jobs []string `toml:"jobs" validate:"required,min=1,dive,required"`
	// Optional list of the execution nodes to monitor.
	// An empty list disables node monitoring entirely.
	Nodes []string `toml:"nodes"`
	// A queued build (waiting for an executor) not started after this
	// many seconds is announced as stuck; defaults to 300.
	QueueStuckAfter int `toml:"queue_stuck_after" validate:"omitempty,min=1"`
	// Optional credentials for private instances (basic auth).
	User     string `toml:"user"`
	Password string `toml:"password"`
	APIToken string `toml:"api_token"`
}

// nodeWanted reports whether a computer should be monitored.
// Only the nodes explicitly listed in the config are tracked; an empty list
// disables node monitoring.
func (cfg *ConfigJenkins) nodeWanted(name string) bool {
	for _, n := range cfg.Nodes {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func main() {
	logLevel := &slog.LevelVar{} // INFO
	logOpts := &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, logOpts))
	slog.SetDefault(logger)

	configFile := flag.String("config", "jenkins-monitor.toml", "configuration file")
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

	// Setup context and signal handling
	ctx, cancel := monitor.SignalContext()
	defer cancel()

	jenkins := newJenkinsClient(&config.Jenkins)
	webhook := monitor.NewWebhook(&config.Webhook)

	monitor := NewMonitor(&config.Jenkins, jenkins, webhook,
		filepath.Join(config.DataDir, config.Jenkins.Name+".state"),
		filepath.Join(config.DataDir, config.Jenkins.Name+".history"),
		nil)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go monitor.Start(ctx, wg)
	wg.Wait()
	slog.Info("jenkins monitor exited")
}

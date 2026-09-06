// Copyright (c) 2026 Aaron LI
//
// Jenkins CI monitor that reports build failures, recoveries, and node
// offline/online changes to IRC via dflybot's webhook.
//
// Co-authored-by: Deepseek-v4-flash (wit Pi Coding Agent)
//

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/go-playground/validator/v10"
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
	Webhook ConfigWebhook `toml:"webhook" validate:"required"`
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
	// Optional list of permanent execution nodes to monitor;
	// empty means monitor all computers.
	Nodes []string `toml:"nodes"`
	// Optional credentials for private instances (basic auth).
	User     string `toml:"user"`
	Password string `toml:"password"`
	APIToken string `toml:"api_token"`
}

// nodeWanted reports whether a computer should be monitored, given the
// optional whitelist of permanent nodes.
func (cfg *ConfigJenkins) nodeWanted(name string) bool {
	if len(cfg.Nodes) == 0 {
		return true
	}
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
	switch config.LogLevel {
	case "", "info":
		logLevel.Set(slog.LevelInfo)
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		slog.Warn("unknown log level", "level", config.LogLevel)
	}

	if err := os.MkdirAll(config.DataDir, 0o755); err != nil {
		slog.Error("data directory creation failed", "dir", config.DataDir, "error", err)
		os.Exit(1)
	}

	// Setup context and signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		slog.Info("signal received, shutting down...")
		cancel()
	}()

	jenkins := newJenkinsClient(&config.Jenkins)
	webhook := NewWebhook(&config.Webhook)

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

// Copyright (c) 2026 Aaron LI
//
// Web monitor that periodically probes the configured web services and
// announces site failures/recoveries (with hysteresis) and expiring TLS
// certificates to IRC via dflybot's webhook.
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
	// Work directory to hold the per-web state and history files.
	// NOTE: Must end with a slash (/) as required by the 'dirpath' validator.
	DataDir string `toml:"data_dir" validate:"dirpath"`
	// TLS/certificate settings.
	TLS ConfigTLS `toml:"tls"`
	// Down/up hysteresis.
	Alert ConfigAlert `toml:"alert" validate:"required"`
	// Probe timeouts (milliseconds).
	Timeouts ConfigTimeouts `toml:"timeouts"`
	// Webhook settings.
	Webhook monitor.ConfigWebhook `toml:"webhook" validate:"required"`
	// List of webs to monitor.
	Webs []ConfigWeb `toml:"webs" validate:"required,min=1,dive"`
}

type ConfigTLS struct {
	// Optional PEM CA bundle; empty uses the system trust store.
	CAFile string `toml:"ca_file"`
	// Certificate expiry warning thresholds (days), e.g. [15,7,3,2,1].
	// Empty uses the default [15,7,3,2,1].
	ExpiringDays []int `toml:"expiring_days" validate:"omitempty,dive,min=1"`
}

type ConfigAlert struct {
	// Consecutive failures to declare a web down.
	DownRepeats int `toml:"down_repeats" validate:"required,min=1"`
	// Consecutive successes to declare a recovery.
	UpRepeats int `toml:"up_repeats" validate:"required,min=1"`
}

type ConfigTimeouts struct {
	DNS     int `toml:"dns"`
	Connect int `toml:"connect"`
	Header  int `toml:"header"`
	Total   int `toml:"total"`
}

type ConfigWeb struct {
	// Whether enabled?
	Enabled bool `toml:"enabled"`
	// Unique name (also names the state/history files).
	Name string `toml:"name" validate:"required"`
	// URL to probe, e.g. "https://www.dragonflybsd.org/".
	URL string `toml:"url" validate:"required,url"`
	// Poll interval in seconds.
	Interval int `toml:"interval" validate:"required,min=1"`
	// Expected HTTP status codes; empty means any 2xx.
	StatusCodes []int `toml:"status_codes" validate:"omitempty,dive,min=100,max=599"`
	// Whether to follow HTTP redirects (default: true).
	FollowRedirection *bool `toml:"follow_redirection"`
	// Whether to verify the TLS certificate (default: true; https only).
	TLSVerify *bool `toml:"tls_verify"`
}

func main() {
	logLevel := &slog.LevelVar{} // INFO
	logOpts := &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, logOpts))
	slog.SetDefault(logger)

	configFile := flag.String("config", "web-monitor.toml", "configuration file")
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
	if err := checkNames(config.Webs); err != nil {
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

	// Parse the CA bundle once at startup; shared by all webs.
	caPool, err := loadCAPool(config.TLS.CAFile)
	if err != nil {
		slog.Error("CA bundle load failed", "ca_file", config.TLS.CAFile, "error", err)
		os.Exit(1)
	}
	webhook := monitor.NewWebhook(&config.Webhook)
	wg := &sync.WaitGroup{}

	for i := range config.Webs {
		web := &config.Webs[i]
		if !web.Enabled {
			slog.Info("skip disabled web", "name", web.Name)
			continue
		}
		follow := true
		if web.FollowRedirection != nil {
			follow = *web.FollowRedirection
		}
		verify := true
		if web.TLSVerify != nil {
			verify = *web.TLSVerify
		}
		prober, err := newProber(web, &config.Timeouts, caPool, follow, verify)
		if err != nil {
			slog.Error("prober setup failed", "name", web.Name, "error", err)
			os.Exit(1)
		}
		mon := newWebMonitor(web, prober, webhook, &config.Alert, &config.TLS,
			config.DataDir, nil)
		wg.Add(1)
		go mon.Start(ctx, wg)
	}

	wg.Wait()
	slog.Info("all monitors exited")
}

// checkNames verifies that the enabled web names are unique.
func checkNames(webs []ConfigWeb) error {
	seen := make(map[string]bool)
	for _, web := range webs {
		if !web.Enabled {
			continue
		}
		if seen[web.Name] {
			return fmt.Errorf("duplicate web name %q", web.Name)
		}
		seen[web.Name] = true
	}
	return nil
}

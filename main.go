// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Simple bot for the DragonFly project.
//

package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Logger level: debug, info, warn, error
	LogLevel string `toml:"log_level"`
	// The IRC to interact with.
	IRC struct {
		// Nickname
		Nick string `toml:"nick"`
		// Server address
		Server string `toml:"server"`
		// Server port
		Port uint16 `toml:"port"`
		// Whether to use SSL?
		SSL bool `toml:"ssl"`
		// Channels to join; must prefix with '#'
		Channels []string `toml:"channels"`
	} `toml:"irc"`
}

func main() {
	logLevel := &slog.LevelVar{} // INFO
	logOpts := &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, logOpts))
	slog.SetDefault(logger)

	configFile := flag.String("config", "dflybot.toml", "configuration file")
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

	ibot := NewIrcBot(&IrcConfig{
		Nick:     config.IRC.Nick,
		Server:   config.IRC.Server,
		Port:     config.IRC.Port,
		SSL:      config.IRC.SSL,
		Channels: config.IRC.Channels,
	})
	go ibot.Start()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	ibot.Stop()
}

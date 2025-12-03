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
	"github.com/go-playground/validator/v10"
)

const (
	busBufferSize int = 100
)

// Only need to use a single instance of Validate, which caches struct info.
var validate = validator.New(validator.WithRequiredStructEnabled())

type Config struct {
	// Logger level: debug, info, warn, error
	LogLevel string `toml:"log_level" validate:"required,oneof=debug info warn error"`
	// Webhook service to accept external notifications.
	Webhook struct {
		// Listen address, e.g., "127.0.0.1:2018"
		Listen string `toml:"listen" validate:"tcp_addr"`
		// Bearer token to authenticate.
		Token string `toml:"token" validate:"required,min=20"`
	} `toml:"webhook" validate:"required"`
	// The IRC to interact with.
	IRC struct {
		// Nickname
		Nick string `toml:"nick" validate:"required"`
		// Server address
		Server string `toml:"server" validate:"fqdn|ip"`
		// Server port
		Port uint16 `toml:"port" validate:"port"`
		// Whether to use SSL?
		SSL bool `toml:"ssl"`
		// List of channels to join
		Channels []struct {
			// The channel name (must prefix with '#')
			Name string `toml:"name" validate:"startswith=#"`
			// The usernames and passwords to authenticate the
			// "!opme" command
			OpMe map[string]string `toml:"op_me"`
		} `toml:"channels"`
	} `toml:"irc" validate:"required"`
	// Telegram settings.
	Telegram struct {
		// The bot token.
		Token string `toml:"token" validate:"required"`
		// The Chat IDs where to post messages.
		Chats []int64 `toml:"chats"`
	} `toml:"telegram" validate:"required"`
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
		panic(err)
	}
	slog.Debug("read config", "file", *configFile, "data", config)

	if err := validate.Struct(&config); err != nil {
		slog.Error("invalid config", "error", err)
		panic(err)
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

	bus := NewBus(busBufferSize)

	webhook, err := NewWebhook(config.Webhook.Listen, config.Webhook.Token, bus)
	if err != nil {
		panic(err)
	}
	go webhook.Start()

	tgbot, err := NewTgBot(config.Telegram.Token, config.Telegram.Chats)
	if err != nil {
		panic(err)
	}
	go tgbot.Start()

	go func() {
		sub := bus.Subscribe("telegram", busBufferSize)
		for msg := range sub.C {
			tgbot.Post(msg)
		}
	}()

	icfg := &IrcConfig{
		Nick:   config.IRC.Nick,
		Server: config.IRC.Server,
		Port:   config.IRC.Port,
		SSL:    config.IRC.SSL,
	}
	for _, ch := range config.IRC.Channels {
		icfg.Channels = append(icfg.Channels, struct {
			Name string
			OpMe map[string]string
		}{ch.Name, ch.OpMe})
	}
	ibot := NewIrcBot(icfg, bus)
	go ibot.Start()

	go func() {
		sub := bus.Subscribe("irc", busBufferSize)
		for msg := range sub.C {
			ibot.Post(msg)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	ibot.Stop()
	tgbot.Stop()
	webhook.Stop()
	bus.Close()
}

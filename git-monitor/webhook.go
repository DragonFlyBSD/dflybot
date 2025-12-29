// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Webhook integration.
//

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type ConfigWebhook struct {
	// Webhook URL
	URL string `toml:"url" validate:"url"`
	// Request method
	Method string `toml:"method" validate:"required,oneof=GET PUT POST"`
	// Bearer token to authenticate
	Token string `toml:"token"`
	// Name to identify this tool
	From string `toml:"from" validate:"required"`
	// Target to announce the messages (e.g., IRC channel `#dragonflybsd`)
	Target string `toml:"target" validate:"required"`
}

type Webhook struct {
	config      *ConfigWebhook
	client      *http.Client
	backoff     time.Duration
	maxAttempts int
}

func NewWebhook(cfg *ConfigWebhook) *Webhook {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// short dial timeout
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
			Proxy: http.ProxyFromEnvironment,
		},
	}
	return &Webhook{
		config:      cfg,
		client:      client,
		backoff:     time.Second,
		maxAttempts: 3,
	}
}

func (w *Webhook) Post(ctx context.Context, text string) error {
	var payload = struct {
		From   string `json:"from"`
		Target string `json:"target"`
		Text   string `json:"text"`
	}{w.config.From, w.config.Target, text}
	b, err := json.Marshal(&payload)
	if err != nil {
		slog.Error("Webhook payload marshal failure", "payload", payload, "error", err)
		return err
	}

	var lastErr error
	for attempt := 0; attempt < w.maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, w.config.Method, w.config.URL,
			bytes.NewReader(b))
		if err != nil {
			slog.Error("Webhook request creation failed", "error", err)
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+w.config.Token)

		resp, err := w.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				slog.Debug("Webhook posted", "payload", string(b))
				return nil
			}
			err = fmt.Errorf("http status %d", resp.StatusCode)
		}
		slog.Warn("Webhook request failed", "error", err)
		if ctx.Err() != nil {
			return ctx.Err() // context was cancelled
		}

		lastErr = err
		time.Sleep(time.Duration(1<<attempt) * w.backoff)
	}
	slog.Error("Webhook post failure after retries", "payload", string(b), "last_error", lastErr)
	return lastErr
}

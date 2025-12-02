// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Webhook service to accept external notifications.
//

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Webhook struct {
	listener net.Listener
	server   *http.Server
	token    string
	bus      *Bus
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewWebhook(listen string, token string, bus *Bus) (*Webhook, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("Webhook service listening failed", "address", listen)
		return nil, err
	}
	slog.Info("Webhook service listened at", "address", listen)

	webhook := &Webhook{
		listener: listener,
		token:    token,
		bus:      bus,
	}

	// NOTE: Don't use the new pattern syntax since v1.22 because we're
	// supporting Go v1.21.
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", webhook.handleNotify)

	webhook.server = &http.Server{
		Handler: webhook.authMiddleware(mux),
	}

	return webhook, nil
}

type WebhookMessage struct {
	// nickname/user/from
	From string `json:"from" validate:"required"`
	// where the message was posted (#channel or nick)
	Target string `json:"target" validate:"required"`
	// message content
	Text string `json:"text" validate:"required"`
}

func (h *Webhook) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mediaType := ""
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	if mediaType != "application/json" {
		http.Error(w, fmt.Sprintf("JSON expected but got %s", mediaType),
			http.StatusBadRequest)
		return
	}

	var msg WebhookMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validate.Struct(&msg); err != nil {
		slog.Warn("Webhook message invalid", "message", msg, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Debug("Webhook received", "message", msg)

	h.bus.Produce(Message{
		Source:    SourceWebhook,
		Timestamp: time.Now(),
		From:      msg.From,
		Target:    msg.Target,
		Text:      msg.Text,
	})
}

func (h *Webhook) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.token != "" {
			token := r.Header.Get("Authorization")
			token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
			if token != h.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (h *Webhook) Start() {
	h.wg.Add(1)
	defer h.wg.Done()

	h.server.Serve(h.listener)
}

func (h *Webhook) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		slog.Error("Webhook shutdown failed", "error", err)
		h.server.Close()
	}

	h.wg.Wait()
	slog.Info("Webhook service stopped")
}

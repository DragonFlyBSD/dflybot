// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Bus to manage message relays.
//

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type BusSource string

const (
	SourceIRC     BusSource = "irc"
	SourceWebhook           = "webhook"
)

type Message struct {
	Source    BusSource `json:"source" validate:"required,oneof=irc webhook"`
	Timestamp time.Time `json:"timestamp" validate:"required"`
	// extra event info (e.g., IRC action)
	Event string `json:"event"`
	// nickname/user/from
	From string `json:"from" validate:"required"`
	// where the message was posted or to post (#channel or nick)
	Target string `json:"target" validate:"required"`
	// message content
	Text string `json:"text" validate:"required"`
}

type Subscriber struct {
	C      chan Message
	name   string
	cancel context.CancelFunc
}

func (s *Subscriber) Close() {
	s.cancel()
}

type Bus struct {
	mu          sync.RWMutex
	subscribers map[*Subscriber]struct{}
	in          chan Message
}

func NewBus(buffer int) *Bus {
	b := &Bus{
		subscribers: make(map[*Subscriber]struct{}),
		in:          make(chan Message, buffer),
	}
	go b.start()
	return b
}

func (b *Bus) start() {
	for msg := range b.in {
		b.mu.RLock()
		for s := range b.subscribers {
			select {
			case s.C <- msg:
			default:
				slog.Warn("message dispatching failed", "subscriber", s.name, "message", msg)
			}
		}
		b.mu.RUnlock()
	}
}

func (b *Bus) Close() {
	for s := range b.subscribers {
		s.Close()
	}
}

func (b *Bus) Produce(msg Message) error {
	if err := validate.Struct(&msg); err != nil {
		slog.Error("Bus message invalid", "message", msg, "error", err)
		return err
	}

	b.in <- msg
	return nil
}

func (b *Bus) Subscribe(name string, buffer int) *Subscriber {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Subscriber{
		C:      make(chan Message, buffer),
		name:   name,
		cancel: cancel,
	}

	b.mu.Lock()
	b.subscribers[s] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		close(s.C)
		b.mu.Lock()
		delete(b.subscribers, s)
		b.mu.Unlock()
	}()

	return s
}

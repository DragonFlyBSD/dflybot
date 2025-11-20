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
	SourceIRC BusSource = "irc"
)

type Message struct {
	Source    BusSource
	Timestamp time.Time
	From      string // nickname/user/from
	Target    string // where the message was posted (#channel or nick)
	Text      string // content
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

func (b *Bus) Produce(msg Message) {
	b.in <- msg
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
		b.mu.Lock()
		if _, ok := b.subscribers[s]; ok {
			delete(b.subscribers, s)
			close(s.C)
		}
		b.mu.Unlock()
	}()

	return s
}

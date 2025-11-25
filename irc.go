// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// IRC bot to fetch messages.
//

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	irc "github.com/fluffle/goirc/client"
)

const (
	baseBackoff = 5 * time.Second
	maxBackoff  = 5 * time.Minute
	pingFreq    = 60 * time.Second
)

type IrcConfig struct {
	Nick     string
	Server   string
	Port     uint16
	SSL      bool
	Channels []string
}

type IrcBot struct {
	conn     *irc.Conn
	channels []string
	bus      *Bus
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewIrcBot(cfg *IrcConfig, bus *Bus) *IrcBot {
	ic := irc.NewConfig(cfg.Nick)
	ic.Server = net.JoinHostPort(cfg.Server, strconv.Itoa(int(cfg.Port)))
	ic.Timeout = 30 * time.Second
	if cfg.SSL {
		ic.SSL = true
		ic.SSLConfig = &tls.Config{
			ServerName:         cfg.Server,
			InsecureSkipVerify: true,
		}
	}
	// NOTE: Don't set PingFreq as we'll also perform PINGs in
	// startWatchdog().

	conn := irc.Client(ic)
	ibot := &IrcBot{
		conn:     conn,
		channels: cfg.Channels, // copy for the HandleFunc() closure below
		bus:      bus,
	}

	conn.EnableStateTracking()
	conn.HandleFunc(irc.CONNECTED, func(c *irc.Conn, _ *irc.Line) {
		slog.Info("IRC connected", "server", c.Config().Server, "nick", c.Me().Nick)
		for _, ch := range ibot.channels {
			c.Join(ch)
			slog.Info("IRC joined", "channel", ch)
		}
	})
	conn.HandleFunc(irc.DISCONNECTED, func(c *irc.Conn, _ *irc.Line) {
		slog.Info("IRC disconnected", "server", c.Config().Server)
	})
	conn.HandleFunc(irc.PING, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC PING from server", "line", l.Raw)
	})
	conn.HandleFunc(irc.PRIVMSG, func(c *irc.Conn, l *irc.Line) {
		slog.Debug("IRC received message", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		me := c.Me().Nick
		re := regexp.MustCompile(`^@?` + me + `\s*[:,]?\s+`)
		text := strings.TrimSpace(l.Text())
		if loc := re.FindStringIndex(text); loc != nil {
			text = text[loc[1]:]
		}
		if !ibot.tryCommand(text, l.Target()) {
			bus.Produce(Message{
				Source:    SourceIRC,
				Timestamp: time.Now(),
				From:      l.Nick,
				Target:    l.Target(),
				Text:      text,
			})
		}
	})

	return ibot
}

func (b *IrcBot) tryCommand(text, target string) bool {
	if !strings.HasPrefix(text, "!") {
		return false
	}

	cmd, args, _ := strings.Cut(strings.TrimPrefix(text, "!"), " ")
	cmd = strings.ToLower(cmd)
	args = strings.TrimSpace(args)
	slog.Debug("IRC received command", "cmd", cmd, "args", args)

	// TODO: more commands
	switch cmd {
	case "ping":
		b.conn.Privmsg(target, "pong")
		return true
	default:
		slog.Warn("IRC unknown command", "cmd", cmd, "args", args)
		return false
	}
}

func (b *IrcBot) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	b.wg.Add(1)
	go b.startWatchdog(ctx)

	defer func() {
		b.conn.Quit("shutting down; bye :P")
		time.Sleep(500 * time.Millisecond) // wait a moment
		b.conn.Close()
		slog.Info("IRC bot closed")
		b.wg.Done()
	}()

	b.wg.Add(1)
	backoff := baseBackoff
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		server := b.conn.Config().Server
		slog.Debug("IRC attempting to connect", "server", server)
		if err := b.conn.Connect(); err != nil {
			slog.Error("IRC connection failed", "server", server, "error", err, "backoff", backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = baseBackoff

		// Block until disconnected or context cancelled.
		for {
			if !b.conn.Connected() {
				slog.Warn("IRC connection lost; reconnecting ...")
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
		}
	}
}

// The watchdog periodically pings the server to proactively detect the
// disconnection (e.g., network lost, laptop suspension) and then force a
// reconnection.  This is needed because goirc doesn't support to disable the
// TCP keepalive and doesn't expose the underlying connection to archieve that.
// Without disabling TCP keepalive, I observed that goirc waited about 15
// minutes before detecting the disconnection.
//
// NOTE: Using PING might not work with some IRC servers, because the standard
// only defines the server->client PING but not the client->server PING.
func (b *IrcBot) startWatchdog(ctx context.Context) {
	var lastPong atomic.Int64
	remover := b.conn.HandleFunc(irc.PONG, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC PONG from server", "line", l.Raw)
		lastPong.Store(time.Now().UnixNano())
	})

	defer func() {
		remover.Remove()
		b.wg.Done()
	}()

	timeout := time.Duration(1.5*pingFreq.Seconds()) * time.Second
	ticker := time.NewTicker(pingFreq)
	lastPong.Store(time.Now().UnixNano())

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			if !b.conn.Connected() {
				lastPong.Store(time.Now().UnixNano())
				continue // handled in Start() above
			}

			last := time.Unix(0, lastPong.Load())
			if time.Since(last) >= timeout {
				slog.Warn("IRC health check failed", "last_pong", last)
				b.conn.Close()
				lastPong.Store(time.Now().UnixNano())
				continue
			}

			b.conn.Ping(fmt.Sprintf("healthcheck-%d", time.Now().UnixNano()))
			slog.Debug("IRC sent PING to server")
		}
	}
}

func (b *IrcBot) Stop() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.wg.Wait()
	slog.Info("IRC bot stopped")
}

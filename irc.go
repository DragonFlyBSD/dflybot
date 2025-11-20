package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	irc "github.com/fluffle/goirc/client"
)

const (
	baseBackoff = 5 * time.Second
	maxBackoff  = 5 * time.Minute
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
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewIrcBot(cfg *IrcConfig) *IrcBot {
	ic := irc.NewConfig(cfg.Nick)
	ic.Server = fmt.Sprintf("%s:%d", cfg.Server, cfg.Port)
	ic.PingFreq = 60 * time.Second
	ic.Timeout = 30 * time.Second
	if cfg.SSL {
		ic.SSL = true
		ic.SSLConfig = &tls.Config{
			ServerName:         cfg.Server,
			InsecureSkipVerify: true,
		}
	}

	conn := irc.Client(ic)
	ibot := &IrcBot{
		conn:     conn,
		channels: cfg.Channels, // copy for the HandleFunc() closure below
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
		text := strings.TrimSpace(l.Text())
		if strings.HasPrefix(text, "!") {
			ibot.handleCommand(l)
		} else {
			slog.Debug("IRC received message", "target", l.Target(), "sender", l.Nick, "text", l.Text())
			// TODO: relay
		}
	})

	return ibot
}

func (b *IrcBot) handleCommand(l *irc.Line) {
	command := strings.TrimPrefix(strings.TrimSpace(l.Text()), "!")
	cmd, args, _ := strings.Cut(command, " ")
	cmd = strings.ToLower(cmd)
	args = strings.TrimSpace(args)
	slog.Debug("IRC received command", "target", l.Target(), "sender", l.Nick, "cmd", cmd, "args", args)

	// TODO: more commands
	switch cmd {
	case "ping":
		b.conn.Privmsg(l.Target(), "pong")
	default:
		slog.Warn("IRC unknown command", "cmd", cmd, "args", args)
	}
}

func (b *IrcBot) start() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)

	defer func() {
		b.conn.Quit("shutting down; bye :P")
		time.Sleep(500 * time.Millisecond) // wait a moment
		b.conn.Close()
		slog.Info("IRC bot closed")
		b.wg.Done()
	}()

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

func (b *IrcBot) stop() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.wg.Wait()
	slog.Info("IRC bot stopped")
}

// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025-2026 Aaron LI
//
// IRC bot that fetches messages, handles commands, routes to message bus,
// maintains seen database, logs messages.
//

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
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

	ttlcache "github.com/liweitianux/dflybot/ttlcache"
)

const (
	baseBackoff   = 5 * time.Second
	maxBackoff    = 5 * time.Minute
	pingFreq      = 60 * time.Second
	nickCheckFreq = 60 * time.Second
	opmeLeeway    = 60 * time.Second
)

type IrcConfig struct {
	Nick     string
	Server   string
	Port     uint16
	SSL      bool
	Channels []struct {
		Name string
		OpMe map[string]string
	}
}

type IrcBot struct {
	config *IrcConfig
	conn   *irc.Conn
	bus    *Bus
	seen   *SeenStore
	log    *LogStore
	cache  *ttlcache.Cache
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Current membership (lowercased nicks) of every joined channel, used
	// to attribute the channel-less NICK/QUIT events to the right channel
	// logs.  Only touched from the IRC foreground handlers, which goirc
	// runs sequentially per event.
	members map[string]map[string]struct{}
}

func NewIrcBot(cfg *IrcConfig, bus *Bus, seen *SeenStore, log *LogStore) *IrcBot {
	ic := irc.NewConfig(cfg.Nick)
	ic.Server = net.JoinHostPort(cfg.Server, strconv.Itoa(int(cfg.Port)))
	ic.Timeout = 30 * time.Second
	ic.NewNick = func(old string) string { return old + "_" }
	if cfg.SSL {
		ic.SSL = true
		ic.SSLConfig = &tls.Config{
			ServerName:         cfg.Server,
			InsecureSkipVerify: true,
		}
	}
	// NOTE: Clear PingFreq to disable the builtin PING loop as we'll also
	// perform PINGs in startWatchdog().
	ic.PingFreq = 0

	conn := irc.Client(ic)
	ibot := &IrcBot{
		config:  cfg,
		conn:    conn,
		bus:     bus,
		seen:    seen,
		log:     log,
		cache:   ttlcache.New(opmeLeeway*2, 0, nil),
		members: make(map[string]map[string]struct{}),
	}

	conn.EnableStateTracking()
	conn.HandleFunc(irc.CONNECTED, func(c *irc.Conn, _ *irc.Line) {
		slog.Info("IRC connected", "server", c.Config().Server, "nick", c.Me().Nick)
		for _, ch := range ibot.config.Channels {
			c.Join(ch.Name)
			slog.Info("IRC joined", "channel", ch.Name)
		}
	})
	conn.HandleFunc(irc.DISCONNECTED, func(c *irc.Conn, _ *irc.Line) {
		slog.Info("IRC disconnected", "server", c.Config().Server)
		// Membership is stale after a disconnect; NAMES re-seeds on rejoin.
		ibot.members = make(map[string]map[string]struct{})
	})
	conn.HandleFunc(irc.PING, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC PING from server", "line", l.Raw)
	})
	conn.HandleFunc(irc.JOIN, func(c *irc.Conn, l *irc.Line) {
		slog.Debug("IRC join", "channel", l.Target(), "nick", l.Nick)
		now := time.Now()
		ch := l.Target()
		ibot.seen.Join(ch, l.Nick, now)
		if l.Nick == c.Me().Nick {
			// We (re)joined: reset the channel membership and let the
			// NAMES reply that follows re-seed it.
			ibot.members[ch] = make(map[string]struct{})
		}
		ibot.addMember(ch, l.Nick)
		ibot.log.Record(LogRecord{Timestamp: now, Type: LogTypeJoin, Channel: ch,
			Nick: l.Nick, User: l.Ident, Host: l.Host})
	})
	conn.HandleFunc(irc.PART, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC part", "channel", l.Target(), "nick", l.Nick)
		now := time.Now()
		ch := l.Target()
		ibot.seen.Leave(ch, l.Nick, now)
		ibot.delMember(ch, l.Nick)
		rec := LogRecord{Timestamp: now, Type: LogTypePart, Channel: ch,
			Nick: l.Nick, User: l.Ident, Host: l.Host}
		if len(l.Args) > 1 { // the optional part message
			rec.Text = l.Args[1]
		}
		ibot.log.Record(rec)
	})
	conn.HandleFunc(irc.KICK, func(c *irc.Conn, l *irc.Line) {
		if len(l.Args) < 2 {
			return
		}
		slog.Debug("IRC kick", "channel", l.Target(), "nick", l.Args[1], "by", l.Nick)
		now := time.Now()
		ch := l.Target()
		victim := l.Args[1]
		ibot.seen.Leave(ch, victim, now)
		if strings.EqualFold(victim, c.Me().Nick) {
			delete(ibot.members, ch) // we are no longer in this channel
		} else {
			ibot.delMember(ch, victim)
		}
		rec := LogRecord{Timestamp: now, Type: LogTypeKick, Channel: ch,
			Nick: l.Nick, User: l.Ident, Host: l.Host, Target: victim}
		if len(l.Args) > 2 { // the optional kick reason
			rec.Text = l.Args[2]
		}
		ibot.log.Record(rec)
	})
	conn.HandleFunc(irc.PRIVMSG, func(c *irc.Conn, l *irc.Line) {
		slog.Debug("IRC received message", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		me := c.Me().Nick
		re := regexp.MustCompile(`^@?` + me + `\s*[:,]?\s+`)
		text := strings.TrimSpace(l.Text())
		if loc := re.FindStringIndex(text); loc != nil {
			text = text[loc[1]:]
		}
		target := l.Target()
		if target == l.Nick {
			// Private message to me.
			ibot.tryCommand(text, target, l)
		} else {
			ibot.seen.Message(target, l.Nick, time.Now())
			ibot.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeMessage,
				Channel: target, Nick: l.Nick, User: l.Ident, Host: l.Host, Text: l.Text()})
			if !ibot.tryCommand(text, target, l) {
				ibot.bus.Produce(Message{
					Source:    SourceIRC,
					Timestamp: time.Now(),
					From:      l.Nick,
					Target:    target,
					Text:      text,
				})
			}
		}
	})
	conn.HandleFunc(irc.QUIT, func(_ *irc.Conn, l *irc.Line) {
		ibot.tryRecoverNick()
		now := time.Now()
		// A QUIT carries no channel; log it to every channel of which the
		// nick was a member (our membership mirror).
		for _, ch := range ibot.memberChannels(l.Nick) {
			rec := LogRecord{Timestamp: now, Type: LogTypeQuit, Channel: ch,
				Nick: l.Nick, User: l.Ident, Host: l.Host}
			if len(l.Args) > 0 { // the optional quit message
				rec.Text = l.Args[0]
			}
			ibot.log.Record(rec)
		}
		ibot.delMemberAll(l.Nick)
		ibot.seen.Quit(l.Nick, now)
	})
	conn.HandleFunc(irc.NICK, func(c *irc.Conn, l *irc.Line) {
		if l.Nick != c.Me().Nick {
			ibot.tryRecoverNick()
		}
		now := time.Now()
		old, neu := l.Nick, l.Args[0]
		// A NICK carries no channel; log it to every channel of which the
		// nick was a member (our membership mirror).
		for _, ch := range ibot.memberChannels(old) {
			ibot.log.Record(LogRecord{Timestamp: now, Type: LogTypeNick,
				Channel: ch, User: l.Ident, Host: l.Host, From: old, To: neu})
		}
		ibot.renameMember(old, neu)
		ibot.seen.Rename(old, neu)
	})
	conn.HandleFunc(irc.ACTION, func(c *irc.Conn, l *irc.Line) {
		slog.Debug("IRC received action", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		if target := l.Target(); target != l.Nick {
			ibot.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeAction,
				Channel: target, Nick: l.Nick, User: l.Ident, Host: l.Host, Text: l.Text()})
			ibot.seen.Message(target, l.Nick, time.Now())
		}
		ibot.bus.Produce(Message{
			Source:    SourceIRC,
			Timestamp: time.Now(),
			Event:     "ACTION",
			From:      l.Nick,
			Target:    l.Target(),
			Text:      l.Text(),
		})
	})
	conn.HandleFunc(irc.NOTICE, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC notice", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		// Only channel NOTICEs are logged (private/server ones ignored).
		if target := l.Target(); strings.HasPrefix(target, "#") {
			ibot.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeNotice,
				Channel: target, Nick: l.Nick, User: l.Ident, Host: l.Host, Text: l.Text()})
		}
	})
	conn.HandleFunc(irc.MODE, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC mode", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		// l.Args: [channel, modes, mode args...]; log only channel modes.
		if len(l.Args) < 2 || !strings.HasPrefix(l.Target(), "#") {
			return
		}
		ibot.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeMode,
			Channel: l.Target(), Nick: l.Nick, User: l.Ident, Host: l.Host,
			Modes: l.Args[1], Targets: l.Args[2:]})
	})
	conn.HandleFunc(irc.TOPIC, func(_ *irc.Conn, l *irc.Line) {
		slog.Debug("IRC topic", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		ibot.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeTopic,
			Channel: l.Target(), Nick: l.Nick, User: l.Ident, Host: l.Host, Text: l.Text()})
	})
	conn.HandleFunc("353", func(_ *irc.Conn, l *irc.Line) {
		// The server auto sends the NAMES replies on a success JOIN.
		// NAMES reply: "<me> <symbol> <channel> :<names>", which re-seeds
		// the membership of a channel we just joined.
		slog.Debug("IRC 353/names", "target", l.Target(), "sender", l.Nick, "text", l.Text())
		if len(l.Args) < 4 {
			return
		}
		ch := l.Args[2]
		if ibot.members[ch] == nil {
			return
		}
		for _, nick := range strings.Fields(l.Args[len(l.Args)-1]) {
			if nick = strings.TrimLeft(nick, "~&@%+"); nick != "" {
				ibot.addMember(ch, nick)
			}
		}
	})

	return ibot
}

// say sends a message to target (a channel or a nick), and logs the bot's
// own channel messages (self: true) into the channel log.
func (b *IrcBot) say(target, text string) {
	b.conn.Privmsg(target, text)
	if strings.HasPrefix(target, "#") {
		// Only log messages to a channel.
		b.log.Record(LogRecord{Timestamp: time.Now(), Type: LogTypeMessage,
			Channel: target, Nick: b.conn.Me().Nick, Text: text, Self: true})
	}
}

// The following track the current membership of each joined channel.

func (b *IrcBot) addMember(ch, nick string) {
	if nick == "" {
		return
	}
	set := b.members[ch]
	if set == nil {
		set = make(map[string]struct{})
		b.members[ch] = set
	}
	set[strings.ToLower(nick)] = struct{}{}
}

func (b *IrcBot) delMember(ch, nick string) {
	if set := b.members[ch]; set != nil {
		delete(set, strings.ToLower(nick))
	}
}

// memberChannels returns the channels in which the nick is currently a
// member (the membership mirror; case-insensitive).
func (b *IrcBot) memberChannels(nick string) []string {
	if nick == "" {
		return nil
	}
	lnick := strings.ToLower(nick)
	var chs []string
	for ch, set := range b.members {
		if _, ok := set[lnick]; ok {
			chs = append(chs, ch)
		}
	}
	return chs
}

// renameMember moves the membership of a nick across all channels (NICK
// events carry no channel).
func (b *IrcBot) renameMember(old, neu string) {
	lo, ln := strings.ToLower(old), strings.ToLower(neu)
	if lo == ln {
		return
	}
	for _, set := range b.members {
		if _, ok := set[lo]; ok {
			delete(set, lo)
			set[ln] = struct{}{}
		}
	}
}

// delMemberAll removes a nick (QUIT) from every channel.
func (b *IrcBot) delMemberAll(nick string) {
	ln := strings.ToLower(nick)
	for _, set := range b.members {
		delete(set, ln)
	}
}

func (b *IrcBot) tryCommand(text, target string, l *irc.Line) bool {
	if !strings.HasPrefix(text, "!") {
		return false
	}

	cmd, arg, _ := strings.Cut(strings.TrimPrefix(text, "!"), " ")
	cmd = strings.ToLower(cmd)
	arg = strings.TrimSpace(arg)
	slog.Debug("IRC received command", "cmd", cmd, "arg", arg)

	// TODO: more commands
	switch cmd {
	case "ping":
		b.say(target, "pong")
		return true
	case "opme":
		if !strings.HasPrefix(target, "#") {
			b.say(target, "command opme only works in channel")
			return true
		}
		if !b.hasModeOp(target) {
			b.say(target, l.Nick+": I don't have the permission yet")
			return true
		}
		b.handleOpMe(target, l.Nick, arg)
		return true
	case "seen":
		if !strings.HasPrefix(target, "#") {
			b.say(target, "command seen only works in channel")
			return true
		}
		b.handleSeen(target, arg)
		return true
	default:
		b.say(target, "unknown command: "+cmd)
		slog.Warn("IRC unknown command", "cmd", cmd, "arg", arg)
		return false
	}
}

func (b *IrcBot) tryRecoverNick() {
	nick := b.config.Nick
	state := b.conn.StateTracker()
	if me := state.Me().Nick; me == nick {
		return
	} else if state.GetNick(nick) == nil {
		slog.Info("IRC tried to recover nick", "current", me, "wanted", nick)
		b.conn.Nick(nick)
	} else {
		slog.Debug("IRC wanted nick not available", "nick", nick)
	}
}

func (b *IrcBot) hasModeOp(ch string) bool {
	state := b.conn.StateTracker()
	channel := state.GetChannel(ch)
	if channel == nil {
		slog.Warn("IRC state tracker cannot find", "channel", ch)
		return false
	}

	me := b.conn.Me().Nick
	privs, ok := channel.Nicks[me]
	if !ok {
		slog.Warn("IRC privileges not found", "channel", ch, "me", me)
		return false
	}

	slog.Debug("IRC bot mode info", "channel", ch, "me", me, "privileges", privs)
	return privs.Op
}

func (b *IrcBot) handleOpMe(channel, nick, arg string) {
	var creds map[string]string
	for _, ch := range b.config.Channels {
		if ch.Name == channel {
			creds = ch.OpMe
			break
		}
	}
	if creds == nil {
		b.say(nick, "unsupported opme channel: "+channel)
		return
	}

	// arg: <username>:<timestamp>:<hmac>
	args := strings.Split(arg, ":")
	if len(args) != 3 {
		b.say(nick, "invalid opme argument: "+arg)
		return
	}
	username, timestamp, mac := args[0], args[1], strings.ToLower(args[2])
	authID := username + ":" + timestamp

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		b.say(nick, "invalid opme argument: "+arg)
		slog.Debug("IRC opme timestamp invalid", "timestamp", timestamp)
		return
	}
	d := time.Since(time.Unix(ts, 0))
	if d.Abs() > opmeLeeway {
		b.say(nick, "invalid opme argument: "+arg)
		slog.Debug("IRC opme timestamp out-of-range", "timestamp", timestamp)
		return
	}

	// NOTE: The nick may be occupied by someone else, so don't require
	// the sender has the exact nick as configured.
	var macKey string
	for n, k := range creds {
		if n == username {
			macKey = k
			break
		}
	}
	if macKey == "" {
		b.say(nick, "opme denied")
		slog.Debug("IRC opme username invalid", "username", username)
		return
	}

	h := hmac.New(sha256.New, []byte(macKey))
	h.Write([]byte(authID))
	expected := hex.EncodeToString(h.Sum(nil))
	if expected != mac {
		b.say(nick, "opme denied")
		slog.Debug("IRC opme mac invalid", "mac", mac, "expected", expected)
		return
	}

	if _, exists := b.cache.Get(authID); exists {
		b.say(nick, "opme denied")
		slog.Debug("IRC opme auth replayed", "authID", authID)
		return
	}
	b.cache.Add(authID, struct{}{}, ttlcache.DefaultTTL)

	b.conn.Mode(channel, "+o", nick)
	slog.Info("IRC opme granted", "channel", channel, "nick", nick, "username", username)
}

// handleSeen implements the !seen command: report the presence and last
// activity times of a nick in the given channel, from the live state tracker
// (presence) and the per-channel seen database (history).
func (b *IrcBot) handleSeen(channel, query string) {
	query = strings.TrimSpace(strings.TrimPrefix(query, "@"))
	if query == "" {
		b.say(channel, "usage: !seen <nick>")
		return
	}

	var present []string
	if ch := b.conn.StateTracker().GetChannel(channel); ch != nil {
		for nick := range ch.Nicks {
			present = append(present, nick)
		}
	}
	res := b.seen.Lookup(channel, query, present)
	b.say(channel, res.Text())
}

func (b *IrcBot) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	b.wg.Add(1)
	go b.startWatchdog(ctx)

	b.wg.Add(1)
	go b.startNickCheck(ctx)

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

func (b *IrcBot) startNickCheck(ctx context.Context) {
	defer b.wg.Done()

	ticker := time.NewTicker(nickCheckFreq)
	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			if !b.conn.Connected() {
				continue
			}
			b.tryRecoverNick()
		}
	}
}

func (b *IrcBot) Stop() {
	b.cache.Close()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.wg.Wait()
	slog.Info("IRC bot stopped")
}

func (b *IrcBot) Post(msg Message) {
	if msg.Source == SourceIRC {
		return // Ignore messages originated from self.
	}

	if b.conn == nil || !b.conn.Connected() {
		slog.Error("IRC bot not started/connected")
		return
	}

	state := b.conn.StateTracker()
	if strings.HasPrefix(msg.Target, "#") {
		if state.GetChannel(msg.Target) == nil {
			slog.Warn("IRC bot not joined", "channel", msg.Target)
			return
		}
	} else {
		if state.GetNick(msg.Target) == nil {
			slog.Warn("IRC bot not seen", "nick", msg.Target)
			return
		}
	}

	var from string
	switch msg.Source {
	case SourceIRC:
		from = fmt.Sprintf("[IRC %s]💬 ", msg.From)
	case SourceWebhook:
		from = fmt.Sprintf("[Webhook %s]📢 ", msg.From)
	default:
		from = fmt.Sprintf("[❓ %s] ", msg.From)
	}
	text := from + msg.Text
	b.say(msg.Target, text)
	slog.Debug("IRC bot posted message", "target", msg.Target, "text", text)
}

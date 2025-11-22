// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Telegram bot to post messages.
//

// Telegram setup:
// 1. Search "@BotFather" and open a chat;
// 2. Send "/newbot" to create the bot;
// 3. Send "/mybots" and choose the created bot to edit it;
// 4. Choose "Edit Bot" and then "Edit Commands";
// 5. Send "start - Get the ChatID to start using the bot";
// 6. Search and find the created bot, or add the bot to the group;
// 7. Send "/start" to the bot to get the ChatID;
// 8. Update the config file of this program and restart.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	// Shorten the poll timeout to get rid of the error (unharmful) logs:
	// "error get update, error do request for method getUpdates ...
	// unexpected EOF".
	pollTimeout = 30 * time.Second
)

type TgBot struct {
	bot    *bot.Bot
	chats  []int64
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewTgBot(token string, chats []int64) (*TgBot, error) {
	b, err := bot.New(token,
		bot.WithHTTPClient(pollTimeout, &http.Client{
			Timeout: 2 * pollTimeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				// Disable keepalive allows the TG bot to
				// quickly reconnect after a network
				// disconnection (e.g., laptop suspend+resume);
				// otherwise, there would be multiple errors
				// logs: "context deadline exceeded
				// (Client.Timeout exceeded while awaiting
				// headers)"; which might lasts >30 minutes.
				DisableKeepAlives: true,
			},
		}),
		bot.WithMiddlewares(func(next bot.HandlerFunc) bot.HandlerFunc {
			return func(ctx context.Context, b *bot.Bot, update *models.Update) {
				if m := update.Message; m != nil {
					slog.Debug("TG bot received message",
						"from", m.From.Username, "text", m.Text)
				} else {
					slog.Debug("TG bot received update", "id", update.ID)
				}
				next(ctx, b, update)
			}
		}),
	)
	if err != nil {
		slog.Error("TG bot creation failed", "error", err)
		return nil, err
	}

	tgbot := &TgBot{
		bot:   b,
		chats: chats,
	}

	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeExact,
		func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			chat := update.Message.Chat.ID
			tgbot.send(ctx, &bot.SendMessageParams{
				ChatID:    chat,
				Text:      fmt.Sprintf("The Chat ID is <code>%d</code>.", chat),
				ParseMode: models.ParseModeHTML,
			})
		})
	b.RegisterHandler(bot.HandlerTypeMessageText, "/whoami", bot.MatchTypeExact,
		func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			chat := update.Message.Chat.ID
			tgbot.send(ctx, &bot.SendMessageParams{
				ChatID:    chat,
				Text:      fmt.Sprintf("You are in Chat ID: <code>%d</code>.", chat),
				ParseMode: models.ParseModeHTML,
			})
		})
	b.RegisterHandler(bot.HandlerTypeMessageText, "", bot.MatchTypePrefix,
		func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			m := update.Message
			if !strings.HasPrefix(m.Text, "/") {
				slog.Debug("TG bot received non-command message",
					"from", m.From.Username, "text", m.Text)
				return
			}
			tgbot.send(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "Sorry, I don't know that command.",
				ReplyParameters: &models.ReplyParameters{
					MessageID: m.ID,
				},
			})
		})

	return tgbot, nil
}

func (b *TgBot) send(ctx context.Context, msg *bot.SendMessageParams) {
	if _, err := b.bot.SendMessage(ctx, msg); err != nil {
		slog.Error("TG bot sending failed",
			"to", msg.ChatID, "text", msg.Text, "error", err)
	}
}

func (b *TgBot) Start() {
	b.ctx, b.cancel = context.WithCancel(context.Background())
	if me, err := b.bot.GetMe(b.ctx); err != nil {
		slog.Error("TG bot GetMe failed", "error", err)
		return
	} else {
		slog.Info("TG bot started", "id", me.ID, "username", me.Username)
	}

	b.wg.Add(1)
	b.bot.Start(b.ctx)

	slog.Info("TG bot polling stopped")
	b.wg.Done()
}

func (b *TgBot) Stop() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
		b.ctx = nil
	}
	b.wg.Wait()
	slog.Info("TG bot stopped")
}

func (b *TgBot) Post(msg Message) {
	if b.ctx == nil {
		slog.Error("TG bot called but not started")
		return
	}

	text := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(msg.Text)
	text = fmt.Sprintf("<b>[IRC %s]</b> <code>%s</code>💬 %s",
		msg.Target, msg.From, text)

	for _, chatID := range b.chats {
		b.send(b.ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      text,
			ParseMode: models.ParseModeHTML,
		})
	}
}

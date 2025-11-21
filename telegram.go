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
	"fmt"
	"log/slog"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type TgBot struct {
	bot   *tgbotapi.BotAPI
	chats []int64
}

func NewTgBot(token string, chats []int64) (*TgBot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		slog.Error("TG bot creation failed", "error", err)
		return nil, err
	}

	return &TgBot{bot: bot, chats: chats}, nil
}

func (b *TgBot) Start() {
	slog.Info("TG bot authorized", "username", b.bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.bot.GetUpdatesChan(u)

	for update := range updates {
		m := update.Message
		if m == nil { // ignore any non-message updates
			continue
		}

		slog.Debug("TG bot received message", "from", m.From.UserName, "text", m.Text)

		if !m.IsCommand() { // ignore any non-command messages
			continue
		}

		msg := tgbotapi.NewMessage(m.Chat.ID, "")
		switch m.Command() {
		case "help", "start":
			msg.Text = fmt.Sprintf("The Chat ID is `%d`.", m.Chat.ID)
			msg.ParseMode = tgbotapi.ModeMarkdown
		default:
			msg.Text = "Sorry, I don't know that command."
			msg.ReplyToMessageID = m.MessageID
		}

		if _, err := b.bot.Send(msg); err != nil {
			slog.Error("TG bot sending failed", "message", msg, "error", err)
		}
	}
}

func (b *TgBot) Post(msg Message) {
	text := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(msg.Text)
	text = fmt.Sprintf("<b>[IRC %s]</b> &lt;%s&gt; %s",
		msg.Target, msg.From, text)

	for _, chatID := range b.chats {
		message := tgbotapi.NewMessage(chatID, text)
		message.ParseMode = tgbotapi.ModeHTML
		if _, err := b.bot.Send(message); err != nil {
			slog.Error("TG bot sending failed", "to", chatID, "message", msg, "error", err)
		}
	}
}

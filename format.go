// Copyright (c) 2026 Aaron LI
//
// Configurable message formatting for the IRC and Telegram sinks, using Go
// text/template.  Each sink renders a Message through its template; empty
// config templates use the built-in defaults below (identical to the
// historical hard-coded formatting).
//
// Available data fields (MessageView):
//
//	Source    # message source ("irc"/"webhook")
//	Target    # channel or nick the message is about
//	From      # nick/label of the origin
//	Text      # the raw message text
//	Event     # e.g. "ACTION" for IRC /me messages
//	IsAction  # true for IRC ACTION (/me) messages
//	IsPrivate # true when the target is a nick, not a channel
//
// Template functions: html (escape & < > for Telegram HTML), plus the
// standard text/template funcs (eq, ne, printf, ...).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"bytes"
	"strings"
	"text/template"
)

// MessageView is the data passed to the sink message templates.
type MessageView struct {
	Source    string
	Target    string
	From      string
	Text      string
	Event     string
	IsAction  bool
	IsPrivate bool
}

// ircDefaultTemplate formats messages for the IRC sink (plain text).
const ircDefaultTemplate = `
{{- if eq .Source "irc" -}}
[IRC {{.From}}]💬
{{- else if eq .Source "webhook" -}}
[Webhook {{.From}}]📢
{{- else -}}
[❓ {{.From}}]
{{- end -}}
{{" "}}{{.Text}}
`

// telegramDefaultTemplate formats messages for the Telegram sink (HTML);
// only the message text is HTML-escaped.
const telegramDefaultTemplate = `
{{- if eq .Source "irc" -}}
<b>[IRC {{.Target}}]</b>{{" "}}
{{- if .IsAction -}}
👉 <code>{{.From}}</code>
{{- else -}}
<code>{{.From}}</code>💬
{{- end -}}
{{- else if eq .Source "webhook" -}}
<b>[Webhook {{.Target}}]</b> <code>{{.From}}</code>📢
{{- else -}}
<b>[❓ {{.Target}}]</b> <code>{{.From}}</code>
{{- end -}}
{{" "}}{{html .Text}}
`

var templateFuncs = template.FuncMap{
	"html": func(s string) string {
		return strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
		).Replace(s)
	},
}

// parseFormat parses a sink message template.  An empty text yields the
// built-in default for the given sink name.  Leading and trailing
// whitespace of the template is trimmed.
func parseFormat(name, text string) (*template.Template, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		switch name {
		case "irc":
			text = ircDefaultTemplate
		case "telegram":
			text = telegramDefaultTemplate
		}
		text = strings.TrimSpace(text)
	}
	return template.New(name).Funcs(templateFuncs).Parse(text)
}

// renderFormat executes the template with the message's data view.
func renderFormat(tpl *template.Template, msg Message) (string, error) {
	var buf bytes.Buffer
	view := MessageView{
		Source:    string(msg.Source),
		Target:    msg.Target,
		From:      msg.From,
		Text:      msg.Text,
		Event:     msg.Event,
		IsAction:  msg.Event == "ACTION",
		IsPrivate: !strings.HasPrefix(msg.Target, "#"),
	}
	err := tpl.Execute(&buf, view)
	return buf.String(), err
}

// Copyright (c) 2026 Aaron LI
//
// Tests for the configurable message templates (format.go).
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"strings"
	"testing"
)

func msgOf(source, target, from, event, text string) Message {
	return Message{Source: BusSource(source), Target: target, From: from,
		Event: event, Text: text}
}

func TestDefaultIRCTemplate(t *testing.T) {
	tpl, err := parseFormat("irc", "")
	if err != nil {
		t.Fatal(err)
	}
	webhookText := `[dragonfly:master] Aaron <aly@aaronly.me> & friends`
	got, err := renderFormat(tpl, msgOf("webhook", "#chan", "git-monitor", "", webhookText))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Webhook git-monitor]📢 " + webhookText // plain text, unescaped
	if got != want {
		t.Errorf("irc render = %q, want %q", got, want)
	}
}

func TestDefaultTelegramTemplate(t *testing.T) {
	tpl, err := parseFormat("telegram", "")
	if err != nil {
		t.Fatal(err)
	}

	// Webhook message: HTML text is escaped.
	got, err := renderFormat(tpl, msgOf("webhook", "#dragonflybsd", "git-monitor", "",
		`[dragonfly:master] aly <aly@aaronly.me> & co`))
	if err != nil {
		t.Fatal(err)
	}
	want := "<b>[Webhook #dragonflybsd]</b> <code>git-monitor</code>📢 " +
		"[dragonfly:master] aly &lt;aly@aaronly.me&gt; &amp; co"
	if got != want {
		t.Errorf("webhook tg render = %q, want %q", got, want)
	}

	// IRC ACTION message.
	got, err = renderFormat(tpl, msgOf("irc", "#chan", "aly", "ACTION", "waves & winks"))
	if err != nil {
		t.Fatal(err)
	}
	want = "<b>[IRC #chan]</b> 👉 <code>aly</code> waves &amp; winks"
	if got != want {
		t.Errorf("action tg render = %q, want %q", got, want)
	}

	// Plain IRC message.
	got, err = renderFormat(tpl, msgOf("irc", "#chan", "aly", "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	want = "<b>[IRC #chan]</b> <code>aly</code>💬 hello"
	if got != want {
		t.Errorf("plain tg render = %q, want %q", got, want)
	}

	// Unknown source falls into the generic branch.
	got, err = renderFormat(tpl, msgOf("other", "#chan", "alice", "", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<b>[❓ #chan]</b> <code>alice</code> x") {
		t.Errorf("unknown source render = %q", got)
	}
}

func TestCustomTemplate(t *testing.T) {
	tpl, err := parseFormat("telegram", `{{if .IsAction}}* {{.From}} {{.Text}}{{else}}{{.From}}: {{.Text}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderFormat(tpl, msgOf("irc", "#chan", "aly", "ACTION", "waves"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "* aly waves" {
		t.Errorf("custom action render = %q", got)
	}
	got, err = renderFormat(tpl, msgOf("webhook", "#chan", "mon", "", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "mon: hi" {
		t.Errorf("custom plain render = %q", got)
	}
}

func TestParseFormatTrimsWhitespace(t *testing.T) {
	// A template configured with TOML """ quoting has leading/trailing
	// newlines and indentation; those must not leak into the output.
	raw := `

{{if eq .From "mon"}}mon-msg: {{.Text}}{{end}}

`
	tpl, err := parseFormat("irc", raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderFormat(tpl, msgOf("webhook", "#chan", "mon", "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "mon-msg: hello" {
		t.Errorf("trimmed render = %q", got)
	}
}

func TestParseFormatErrors(t *testing.T) {
	if _, err := parseFormat("irc", "{{if .Broken}}"); err == nil {
		t.Fatal("invalid template did not error")
	}
	// A custom template using the html helper parses fine.
	if _, err := parseFormat("telegram", "{{html .Text}}"); err != nil {
		t.Fatalf("valid template errored: %v", err)
	}
}

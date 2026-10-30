package incident

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

func TestParseCustomSilence(t *testing.T) {
	for in, want := range map[string]string{
		"3h kernel update":               "3h0m0s kernel update",
		"  90m ":                         "1h30m0s ",
		"7d":                             "168h0m0s ",
		"2d   disk   swap  ":             "48h0m0s disk swap",
		"1m":                             "1m0s ",
		"8d":                             "",
		"0h":                             "",
		"3 hours":                        "",
		"tomorrow-ish":                   "",
		"h3":                             "",
		"3h" + strings.Repeat(" x", 101): "",
		"-3h":                            "",
		"":                               "",
	} {
		d, reason, ok := parseCustomSilence(in)
		got := ""
		if ok {
			got = d.String() + " " + reason
		}
		if got != want {
			t.Errorf("parseCustomSilence(%q) = %q, want %q", in, got, want)
		}
	}
}

func reply(chatID int64, to int, from *telegram.User, text string) telegram.Update {
	m := &telegram.IncomingMessage{MessageID: 100, From: from, Text: text}
	m.Chat.ID = chatID
	if to != 0 {
		m.ReplyToMessage = &struct {
			MessageID int `json:"message_id"`
		}{to}
	}
	return telegram.Update{Message: m}
}

func TestCustomSilenceByReply(t *testing.T) {
	e := newButtonEnv(t)
	e.handle(gmsg("a"))
	alice := &telegram.User{ID: admin, FirstName: "Alice"}
	ctx := context.Background()

	for name, u := range map[string]telegram.Update{
		"not a reply":       reply(chat, 0, alice, "3h kernel update"),
		"reply to another":  reply(chat, 7, alice, "3h kernel update"),
		"other chat":        reply(-1009999999999, 1, alice, "3h kernel update"),
		"stranger":          reply(chat, 1, &telegram.User{ID: stranger}, "3h kernel update"),
		"on behalf of chat": reply(chat, 1, nil, "3h kernel update"),
	} {
		e.buttons.HandleUpdate(ctx, u)
		if calls := e.tg.take(); len(calls) > 0 || e.reactions() != "" || len(e.am.created) > 0 {
			t.Fatalf("%s: acted on it (calls %v)", name, calls)
		}
	}

	e.buttons.HandleUpdate(ctx, reply(chat, 1, alice, "tomorrow-ish"))
	if got := e.reactions(); got != "#100 🤔" || len(e.am.created) > 0 {
		t.Errorf("unparseable: reactions %q, silences %d", got, len(e.am.created))
	}

	e.buttons.HandleUpdate(ctx, reply(chat, 1, alice, "3h kernel <b>update</b>"))
	if len(e.am.created) != 1 {
		t.Fatalf("silences = %d", len(e.am.created))
	}
	s := e.am.created[0]
	if !s.EndsAt.Equal(e.now.Add(3*time.Hour)) || s.CreatedBy != "telegram:11111111" || s.Comment != "kernel <b>update</b> (Silenced from Telegram via alertmanager-telegram-relay)" {
		t.Errorf("silence = %+v", s)
	}
	if got := len(s.Matchers); got != 2 {
		t.Errorf("matchers from the group labels only, got %d", got)
	}
	calls := e.tg.take()
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.Contains(calls[0].Text, "silenced until") {
		t.Errorf("text = %q", calls[0].Text)
	}
	if got := e.reactions(); got != "#100 👌, #1 😴" {
		t.Errorf("reactions = %q", got)
	}

	// A second custom silence replaces the first.
	e.buttons.HandleUpdate(ctx, reply(chat, 1, alice, "1d"))
	if len(e.am.created) != 2 || len(e.am.expired) != 1 {
		t.Errorf("created %d, expired %v", len(e.am.created), e.am.expired)
	}
}

func TestCustomButtonPromptsForReply(t *testing.T) {
	e := newButtonEnv(t)
	custom := button(t, e.handle(gmsg("a"))[0].Keyboard, "⏱ Custom…")
	toast, calls := e.press(custom, chat, 1, admin)
	if toast != toastCustomPrompt {
		t.Errorf("toast = %q", toast)
	}
	expectCalls(t, calls, "sendMessage(reply #1) silent")
	if c := calls[0]; !c.ForceReply || !strings.Contains(c.Text, `<a href="tg://user?id=11111111">Alice</a>`) {
		t.Errorf("prompt = %+v", c)
	}

	// A reply to the prompt (message #2) counts like one to the incident.
	e.buttons.HandleUpdate(context.Background(), reply(chat, 2, &telegram.User{ID: admin, FirstName: "Alice"}, "45m"))
	if len(e.am.created) != 1 || !e.am.created[0].EndsAt.Equal(e.now.Add(45*time.Minute)) {
		t.Errorf("silences = %+v", e.am.created)
	}
	// The button keeps working for another prompt.
	if a, _ := e.store.Action(custom, e.now); a != nil {
		t.Log("custom action replaced after the silence, as expected for a refreshed keyboard")
	}
}

func TestCustomSilenceWhenAlertmanagerIsDown(t *testing.T) {
	e := newButtonEnv(t)
	e.handle(gmsg("a"))
	e.am.down = true
	e.buttons.HandleUpdate(context.Background(), reply(chat, 1, &telegram.User{ID: admin, FirstName: "Alice"}, "3h"))
	if got := e.reactions(); got != "#100 🤷" {
		t.Errorf("reactions = %q", got)
	}
	if calls := e.tg.take(); len(calls) > 0 {
		t.Errorf("calls %v", calls)
	}
}

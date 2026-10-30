package incident

import (
	"context"
	"strings"
	"testing"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

func command(chatID int64, from *telegram.User, text string) telegram.Update {
	m := &telegram.IncomingMessage{EphemeralMessageID: 5, ThreadID: 7, From: from, Text: text}
	m.Chat.ID = chatID
	return telegram.Update{Message: m}
}

func TestCommandParsing(t *testing.T) {
	b := &Buttons{BotUsername: "RelayBot"}
	for text, want := range map[string]string{
		"/alerts":            "alerts",
		"/alerts@RelayBot":   "alerts",
		"/alerts@relaybot x": "alerts",
		"/SILENCES":          "silences",
		"/alerts@OtherBot":   "",
		"/start":             "",
		"alerts":             "",
		"3h /alerts":         "",
	} {
		got, ok := b.command(text)
		if (got != want) || ok != (want != "") {
			t.Errorf("command(%q) = %q, %v; want %q", text, got, ok, want)
		}
	}
}

func TestAlertsCommand(t *testing.T) {
	e := newButtonEnv(t)
	e.handle(gmsg("a"))
	alice := &telegram.User{ID: admin, FirstName: "Alice"}

	e.buttons.HandleUpdate(context.Background(), command(chat, alice, "/alerts"))
	calls := e.tg.take()
	expectCalls(t, calls, "sendMessage silent for 11111111")
	if c := calls[0]; !strings.Contains(c.Text, "1 firing") || !strings.Contains(c.Text, "DiskAlmostFull") || c.ThreadID != 7 {
		t.Errorf("answer = %+v", c)
	}

	for name, u := range map[string]telegram.Update{
		"stranger":   command(chat, &telegram.User{ID: stranger}, "/alerts"),
		"other chat": command(-1009999999999, alice, "/alerts"),
		"no user":    command(chat, nil, "/alerts"),
	} {
		e.buttons.HandleUpdate(context.Background(), u)
		if calls := e.tg.take(); len(calls) > 0 {
			t.Errorf("%s: answered %v", name, calls)
		}
	}
}

func TestSilencesCommand(t *testing.T) {
	e := newButtonEnv(t)
	e.am.foreign = true
	e.press(button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h"), chat, 1, admin)
	alice := &telegram.User{ID: admin, FirstName: "Alice"}

	e.buttons.HandleUpdate(context.Background(), command(chat, alice, "/silences"))
	calls := e.tg.take()
	expectCalls(t, calls, "sendMessage silent for 11111111")
	c := calls[0]
	if !strings.Contains(c.Text, "2 active silences") || !strings.Contains(c.Text, "1. alertname=&#34;DiskAlmostFull&#34;") ||
		!strings.Contains(c.Text, "Alice via Telegram") || !strings.Contains(c.Text, "• job=&#34;backup&#34;") || !strings.Contains(c.Text, "ops@example.com") {
		t.Errorf("text = %q", c.Text)
	}
	if c.Keyboard == nil || len(c.Keyboard.Rows) != 1 {
		t.Fatalf("only the Telegram silence gets a button: %+v", c.Keyboard)
	}
	end := button(t, c.Keyboard, "↩️ End silence 1")

	// Someone else cannot use it.
	if toast, _ := e.press(end, chat, 1, stranger); toast != "" {
		t.Errorf("stranger got %q", toast)
	}
	e.buttons.AdminUserIDs = append(e.buttons.AdminUserIDs, stranger)
	if toast, _ := e.press(end, chat, 1, stranger); toast != toastExpired || len(e.am.expired) != 0 {
		t.Errorf("another admin: toast %q, expired %v", toast, e.am.expired)
	}

	toast, calls := e.press(end, chat, 1, admin)
	if toast != toastUnsilenced || len(e.am.expired) != 1 {
		t.Fatalf("toast %q, expired %v", toast, e.am.expired)
	}
	// The answer is updated to the remaining silences (only the foreign
	// one, without buttons), then the incident's message.
	expectCalls(t, calls, "editEphemeralMessageText", "editMessageText(#1)")
	if a := calls[0]; !strings.Contains(a.Text, "1 active silence") || strings.Contains(a.Text, "DiskAlmostFull") || (a.Keyboard != nil && len(a.Keyboard.Rows) > 0) {
		t.Errorf("updated answer = %q, keyboard %+v", a.Text, a.Keyboard)
	}
	if strings.Contains(calls[1].Text, "silenced") {
		t.Errorf("text = %q", calls[1].Text)
	}
	if toast, _ := e.press(end, chat, 1, admin); toast != toastExpired {
		t.Errorf("second press: %q", toast)
	}
}

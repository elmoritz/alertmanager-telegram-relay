//go:build live

package telegram_test

// Checks the client's assumptions about the Bot API against the real thing:
// which requests it accepts, and which error texts the Is* helpers rely on.
// See docs/LIVE-TESTING.md.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/livetest"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// unknownMessageID is a message ID the test chat is assumed not to have.
const unknownMessageID = 2_000_000_000

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return c
}

func send(t *testing.T, tg *telegram.Client, c context.Context, m telegram.Message) (int, error) {
	t.Helper()
	var id int
	err := livetest.Do(t, func() (err error) {
		id, err = tg.SendMessage(c, m)
		return err
	})
	return id, err
}

func edit(t *testing.T, tg *telegram.Client, c context.Context, chat int64, id int, text string, kb *telegram.Keyboard) error {
	t.Helper()
	return livetest.Do(t, func() error { return tg.EditMessageText(c, chat, id, text, kb) })
}

func randomData(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func apiError(t *testing.T, err error) *telegram.Error {
	t.Helper()
	var e *telegram.Error
	if !errors.As(err, &e) {
		t.Fatalf("not a *telegram.Error: %v", err)
	}
	return e
}

func TestLiveGetMe(t *testing.T) {
	tg, _ := livetest.Telegram(t)
	me, err := tg.GetMe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if me.ID == 0 || me.Username == "" {
		t.Errorf("getMe returned an incomplete user: %+v", me)
	}
}

func TestLiveWrongTokenIsRedacted(t *testing.T) {
	token := livetest.Token(t)
	bad := token + "0"
	_, err := telegram.New(telegram.DefaultBaseURL, bad).GetMe(ctx(t))
	if err == nil {
		t.Fatal("getMe with a wrong token succeeded")
	}
	if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), token) {
		t.Fatal("error contains the token")
	}
	t.Logf("wrong token: %v", err)
}

func TestLiveMessageLifecycle(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	c := ctx(t)
	kb := &telegram.Keyboard{Rows: [][]telegram.Button{
		{{Text: "🔕 1h", Data: randomData(t)}, {Text: "🔕 24h", Data: randomData(t)}},
		{{Text: "📈 Grafana", URL: "https://grafana.example.com/d/abc?var-instance=server-1&from=now-1h"}},
		{{Text: "📋 Copy command", CopyText: "journalctl -u node-exporter --since -1h"}},
	}}
	text := livetest.Prefix + ": lifecycle\n🔥 <b>HostDown</b> · server-1\n<blockquote expandable>line 1\nline 2</blockquote>"

	id, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: text, Keyboard: kb})
	if err != nil {
		t.Fatalf("send with keyboard: %v", err)
	}
	if id == 0 {
		t.Fatal("sendMessage returned message ID 0")
	}

	t.Run("edit to identical content is not an error", func(t *testing.T) {
		if err := edit(t, tg, c, chat, id, text, kb); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("edit to silenced: disabled status, styled button", func(t *testing.T) {
		silenced := &telegram.Keyboard{Rows: [][]telegram.Button{
			{{Text: "🔕 Silenced until 18:00 by Alice", Disabled: true}},
			{{Text: "↩️ Unsilence", Data: randomData(t), Style: telegram.StylePrimary}},
		}}
		if err := edit(t, tg, c, chat, id, text+"\n🔕 silenced", silenced); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("edit to resolved: one disabled button", func(t *testing.T) {
		resolved := &telegram.Keyboard{Rows: [][]telegram.Button{{{Text: "✅ Resolved after 2h 5m", Disabled: true}}}}
		if err := edit(t, tg, c, chat, id, text+"\n✅ resolved", resolved); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("edit removing the keyboard", func(t *testing.T) {
		if err := edit(t, tg, c, chat, id, text+"\n✅ resolved", nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("edit without a keyboard again is not an error", func(t *testing.T) {
		if err := edit(t, tg, c, chat, id, text+"\n✅ resolved", nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("silent reply", func(t *testing.T) {
		_, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: livetest.Prefix + ": silent reply", ReplyTo: id, Silent: true})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reply to a missing message is sent anyway", func(t *testing.T) {
		_, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: livetest.Prefix + ": reply to a missing message", ReplyTo: unknownMessageID})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestLiveEditMissingMessageIsGone(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	err := tg.EditMessageText(ctx(t), chat, unknownMessageID, livetest.Prefix, nil)
	if err == nil {
		t.Fatal("editing a missing message succeeded")
	}
	if !telegram.IsMessageGone(err) {
		t.Errorf("IsMessageGone = false for %v", err)
	}
	if telegram.IsRetryable(err) {
		t.Errorf("IsRetryable = true for %v", err)
	}
}

func TestLiveFormatErrors(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	c := ctx(t)
	for name, text := range map[string]string{
		"unclosed tag":    livetest.Prefix + ": <b>unclosed",
		"unsupported tag": livetest.Prefix + ": <div>x</div>",
		"bare ampersand":  livetest.Prefix + ": a & b <x",
		"4097 characters": strings.Repeat("a", 4097),
		"4097 emoji":      strings.Repeat("😀", 4097),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: text})
			if err == nil {
				t.Fatal("Telegram accepted it")
			}
			if !telegram.IsFormatError(err) {
				t.Errorf("IsFormatError = false for %v", err)
			}
		})
	}
}

// The length limit is 4096 code points of text after entity parsing, which is
// what render.Len counts. An emoji outside the BMP counts once, although it is
// two UTF-16 units.
func TestLiveLengthLimit(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	c := ctx(t)
	for name, text := range map[string]string{
		"4096 characters":             strings.Repeat("a", 4096),
		"4096 emoji":                  strings.Repeat("😀", 4096),
		"4096 characters plus markup": "<b>" + strings.Repeat("&amp;", 4096) + "</b>",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: text}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Rejected buttons must be recognised, so the relay can resend without them.
// Telegram may also accept some of these; that is fine and logged.
func TestLiveButtonErrors(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	c := ctx(t)
	for name, b := range map[string]telegram.Button{
		"host without a dot":      {Text: "📊", URL: "http://grafana:3000/d/abc"},
		"bare IP":                 {Text: "📊", URL: "http://192.0.2.1:3000/d/abc"},
		"localhost":               {Text: "📊", URL: "http://localhost:3000/d/abc"},
		"65-byte callback data":   {Text: "🔕", Data: strings.Repeat("a", 65)},
		"257-character copy text": {Text: "📋", CopyText: strings.Repeat("a", 257)},
		"unknown style":           {Text: "🔕", Data: "x", Style: "nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			kb := &telegram.Keyboard{Rows: [][]telegram.Button{{b}}}
			_, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: livetest.Prefix + ": button " + name, Keyboard: kb})
			if err == nil {
				t.Logf("Telegram accepted it")
				return
			}
			t.Logf("rejected: %v", err)
			if !telegram.IsButtonError(err) {
				t.Errorf("IsButtonError = false for %v", err)
			}
			if telegram.IsFormatError(err) {
				t.Errorf("IsFormatError = true for %v", err)
			}
		})
	}
}

// getUpdates fails with 409 if a webhook is set on the bot or another process
// (a running relay) polls with the same token.
func TestLiveGetUpdates(t *testing.T) {
	tg, _ := livetest.Telegram(t)
	if _, err := tg.GetUpdates(ctx(t), 0, 0); err != nil {
		if apiError(t, err).Code == 409 {
			t.Fatalf("%v: stop the relay, or remove the bot's webhook", err)
		}
		t.Fatal(err)
	}
}

func TestLiveAnswerUnknownCallback(t *testing.T) {
	tg, _ := livetest.Telegram(t)
	err := tg.AnswerCallbackQuery(ctx(t), "1234567890", "x")
	if err == nil {
		t.Fatal("answering an unknown callback query succeeded")
	}
	t.Logf("unknown callback: %v", err)
	if telegram.IsRetryable(err) {
		t.Errorf("IsRetryable = true for %v", err)
	}
}

// Needs a person: press the button within 90 seconds. Checks that a real
// callback query decodes into the fields the relay's checks rely on.
func TestLiveCallbackQuery(t *testing.T) {
	livetest.Interactive(t)
	tg, chat := livetest.Telegram(t)
	c := ctx(t)

	updates, err := tg.GetUpdates(c, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var offset int64
	for _, u := range updates {
		offset = u.UpdateID + 1
	}

	data := randomData(t)
	text := livetest.Prefix + ": press the button within 90 seconds"
	kb := &telegram.Keyboard{Rows: [][]telegram.Button{{{Text: "👉 Press me", Data: data}}}}
	id, err := send(t, tg, c, telegram.Message{ChatID: chat, Text: text, Keyboard: kb})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		updates, err := tg.GetUpdates(c, offset, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			cq := u.CallbackQuery
			if cq == nil || cq.Data != data {
				continue
			}
			if cq.ID == "" || cq.From.ID == 0 {
				t.Errorf("callback query without ID or user: %+v", cq)
			}
			if cq.Message == nil {
				t.Fatal("callback query without message")
			}
			if cq.Message.Chat.ID != chat || cq.Message.MessageID != id {
				t.Errorf("callback query for chat/message %d/%d, want %d/%d",
					cq.Message.Chat.ID, cq.Message.MessageID, chat, id)
			}
			if err := tg.AnswerCallbackQuery(c, cq.ID, "Live test OK."); err != nil {
				t.Errorf("answer: %v", err)
			}
			if err := edit(t, tg, c, chat, id, livetest.Prefix+": button pressed ✅", nil); err != nil {
				t.Errorf("edit after press: %v", err)
			}
			t.Logf("press by user %d in chat %d", cq.From.ID, cq.Message.Chat.ID)
			// Confirm the update so the relay does not see it later.
			_, _ = tg.GetUpdates(c, offset, 0)
			return
		}
	}
	t.Fatal("no button press within 90 seconds")
}

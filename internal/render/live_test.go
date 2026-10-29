//go:build live

package render

// Sends rendered output to a real chat: Telegram must accept every golden
// file and the longest messages the length control produces. See
// docs/LIVE-TESTING.md.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/livetest"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

func liveSend(t *testing.T, tg *telegram.Client, chat int64, text string) {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := livetest.Do(t, func() error {
		_, err := tg.SendMessage(c, telegram.Message{ChatID: chat, Text: text})
		return err
	})
	if err != nil {
		t.Fatalf("Len %d: %v", Len(text), err)
	}
}

func TestLiveGoldenFilesAreAccepted(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Golden files hold the message and its replies, separated by "---".
		for i, part := range strings.Split(string(b), "\n---\n") {
			t.Run(filepath.Base(f)+"/"+string(rune('0'+i)), func(t *testing.T) {
				liveSend(t, tg, chat, part)
			})
		}
	}
}

func TestLiveLongestMessagesAreAccepted(t *testing.T) {
	tg, chat := livetest.Telegram(t)
	r := renderer(t, "")
	for _, tc := range []struct {
		name   string
		alerts int
		text   string
	}{
		{"one huge alert", 1, ""},
		{"many alerts", 300, ""},
		{"huge label values", 50, strings.Repeat("&", 5000)},
		{"huge alertname, astral plane", 3, strings.Repeat("😀", 5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &alertmanager.Message{Status: alertmanager.StatusFiring}
			for i := range tc.alerts {
				m.Alerts = append(m.Alerts, bigAlert(i, tc.text))
			}
			d := NewData(m)
			d.Added = d.Alerts
			msg, err := r.Message(d)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Len %d", Len(msg))
			liveSend(t, tg, chat, msg)
			added, err := r.Added(d)
			if err != nil {
				t.Fatal(err)
			}
			liveSend(t, tg, chat, added)
		})
	}
	t.Run("minimal", func(t *testing.T) {
		m := &alertmanager.Message{Status: alertmanager.StatusFiring, Alerts: []alertmanager.Alert{bigAlert(0, strings.Repeat("😀", 5000))}}
		liveSend(t, tg, chat, r.Minimal(NewData(m)))
	})
}

//go:build live

// Package livetest connects the live tests to a real Telegram bot and a real
// Alertmanager. It is compiled only with -tags=live and every test skips
// unless its environment variables are set, so CI never reaches the real
// APIs. See docs/LIVE-TESTING.md.
//
// Use a throwaway bot and a private test chat: the tests send messages and
// create silences.
package livetest

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Prefix starts every message the live tests send, so they are easy to spot
// and delete.
const Prefix = "🧪 <b>relay live test</b>"

// Token returns the test bot's token from RELAY_LIVE_TELEGRAM_BOT_TOKEN or
// the file named by RELAY_LIVE_TELEGRAM_BOT_TOKEN_FILE, or skips the test.
func Token(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("RELAY_LIVE_TELEGRAM_BOT_TOKEN"); v != "" {
		return v
	}
	if f := os.Getenv("RELAY_LIVE_TELEGRAM_BOT_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			// The path is not secret; the content is never printed.
			t.Fatalf("reading RELAY_LIVE_TELEGRAM_BOT_TOKEN_FILE: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	t.Skip("RELAY_LIVE_TELEGRAM_BOT_TOKEN(_FILE) not set")
	return ""
}

// Telegram returns a client for the test bot and the test chat ID from
// RELAY_LIVE_CHAT_ID, or skips the test.
func Telegram(t *testing.T) (*telegram.Client, int64) {
	t.Helper()
	token := Token(t)
	v := os.Getenv("RELAY_LIVE_CHAT_ID")
	if v == "" {
		t.Skip("RELAY_LIVE_CHAT_ID not set")
	}
	chat, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatal("RELAY_LIVE_CHAT_ID is not an integer")
	}
	return telegram.New(telegram.DefaultBaseURL, token), chat
}

// Interactive skips the test unless RELAY_LIVE_INTERACTIVE=1: it needs
// someone to press a button in the test chat.
func Interactive(t *testing.T) {
	t.Helper()
	if os.Getenv("RELAY_LIVE_INTERACTIVE") != "1" {
		t.Skip("RELAY_LIVE_INTERACTIVE not set to 1")
	}
}

// AlertmanagerURL returns RELAY_LIVE_ALERTMANAGER_URL, or skips the test.
func AlertmanagerURL(t *testing.T) *url.URL {
	t.Helper()
	v := os.Getenv("RELAY_LIVE_ALERTMANAGER_URL")
	if v == "" {
		t.Skip("RELAY_LIVE_ALERTMANAGER_URL not set")
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		t.Fatal("RELAY_LIVE_ALERTMANAGER_URL is not an http(s) URL")
	}
	return u
}

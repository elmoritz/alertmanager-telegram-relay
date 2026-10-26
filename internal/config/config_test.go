package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

const (
	testToken  = "123:TEST-TOKEN-not-real"
	testSecret = "test-webhook-secret"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":      testToken,
		"TELEGRAM_CHAT_IDS":       "-1001234567890, -1001234567891",
		"TELEGRAM_ADMIN_USER_IDS": "11111111",
		"WEBHOOK_SECRET":          testSecret,
	}
}

func noFiles(string) ([]byte, error) { return nil, fs.ErrNotExist }

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(valid()), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:9095" {
		t.Errorf("ListenAddr = %q, want loopback default", c.ListenAddr)
	}
	if c.DefaultChatID() != -1001234567890 || len(c.ChatIDs) != 2 {
		t.Errorf("ChatIDs = %v", c.ChatIDs)
	}
	if c.Reminders != RemindersReply || c.ResolveReply || c.IncidentTTL.Hours() != 720 || c.Location.String() != "UTC" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", c.LogLevel)
	}
}

func TestLoadRequired(t *testing.T) {
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_IDS", "TELEGRAM_ADMIN_USER_IDS", "WEBHOOK_SECRET"} {
		t.Run(key, func(t *testing.T) {
			m := valid()
			m[key] = "  "
			if _, err := load(env(m), noFiles); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want one naming %s", err, key)
			}
		})
	}
}

func TestLoadRejectsBadIDsWithoutEchoing(t *testing.T) {
	m := valid()
	m["TELEGRAM_CHAT_IDS"] = "-1001234567890,secret-looking-value"
	_, err := load(env(m), noFiles)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-looking-value") || strings.Contains(err.Error(), "-1001234567890") {
		t.Errorf("error echoes the value: %v", err)
	}
}

func TestLoadSecretFromFile(t *testing.T) {
	m := valid()
	delete(m, "TELEGRAM_BOT_TOKEN")
	m["TELEGRAM_BOT_TOKEN_FILE"] = "/run/secrets/token"
	read := func(p string) ([]byte, error) {
		if p == "/run/secrets/token" {
			return []byte(testToken + "\n"), nil
		}
		return nil, fs.ErrNotExist
	}
	c, err := load(env(m), read)
	if err != nil {
		t.Fatal(err)
	}
	if c.BotToken != testToken {
		t.Errorf("token not read from file (or not trimmed)")
	}

	m["TELEGRAM_BOT_TOKEN"] = testToken
	if _, err := load(env(m), read); err == nil {
		t.Error("value and _FILE both set: want error")
	}

	delete(m, "TELEGRAM_BOT_TOKEN")
	m["TELEGRAM_BOT_TOKEN_FILE"] = "/missing"
	if _, err := load(env(m), read); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
}

func TestLoadInvalidValues(t *testing.T) {
	for key, val := range map[string]string{
		"REMINDERS":     "loud",
		"RESOLVE_REPLY": "maybe",
		"INCIDENT_TTL":  "-1h",
		"TIMEZONE":      "Mars/Olympus",
		"LOG_LEVEL":     "chatty",
	} {
		t.Run(key, func(t *testing.T) {
			m := valid()
			m[key] = val
			if _, err := load(env(m), noFiles); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestConfigNeverPrintsSecrets(t *testing.T) {
	c, err := load(env(valid()), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "config", c)

	for _, out := range []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), c.String(),
		fmt.Sprintf("%v", *c), fmt.Sprintf("%+v", *c), fmt.Sprintf("%#v", *c),
		logged.String(),
	} {
		for _, secret := range []string{testToken, testSecret, "1001234567890", "11111111"} {
			if strings.Contains(out, secret) {
				t.Errorf("output contains %q: %s", secret, out)
			}
		}
	}
}

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
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

func TestAlertmanagerAndButtons(t *testing.T) {
	m := valid()
	c, err := load(env(m), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.AlertmanagerURL != nil || len(c.SilenceDurations) != 2 || c.SilenceDurations[1] != 24*time.Hour || c.ShowSourceButton {
		t.Errorf("defaults: %+v", c)
	}

	m["ALERTMANAGER_URL"] = "http://alertmanager:9093"
	m["ALERTMANAGER_BEARER_TOKEN"] = "am-test-token"
	m["SILENCE_DURATIONS"] = "30m, 4h, 30m"
	m["GRAFANA_URL"] = "https://grafana.example.com"
	if c, err = load(env(m), noFiles); err != nil {
		t.Fatal(err)
	}
	if c.AlertmanagerURL.String() != "http://alertmanager:9093" || c.AlertmanagerToken != "am-test-token" || len(c.SilenceDurations) != 2 {
		t.Errorf("got %+v", c)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("c", "c", c)
	if strings.Contains(logged.String()+c.String(), "am-test-token") {
		t.Error("alertmanager token printed")
	}
}

func TestAlertmanagerAndButtonsInvalid(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"credentials in URL":   {"ALERTMANAGER_URL": "http://user:pass@alertmanager:9093"},
		"not http":             {"ALERTMANAGER_URL": "ftp://alertmanager"},
		"relative":             {"ALERTMANAGER_URL": "alertmanager:9093"},
		"username only":        {"ALERTMANAGER_USERNAME": "relay"},
		"basic and bearer":     {"ALERTMANAGER_USERNAME": "relay", "ALERTMANAGER_PASSWORD": "p", "ALERTMANAGER_BEARER_TOKEN": "t"},
		"duration over 7 days": {"SILENCE_DURATIONS": "1h,169h"},
		"zero duration":        {"SILENCE_DURATIONS": "0s"},
		"too many durations":   {"SILENCE_DURATIONS": "1h,2h,3h,4h,5h"},
		"empty durations":      {"SILENCE_DURATIONS": ","},
		"grafana javascript":   {"GRAFANA_URL": "javascript:alert(1)"},
		"source not bool":      {"SHOW_SOURCE_BUTTON": "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			maps.Copy(m, set)
			_, err := load(env(m), noFiles)
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), "pass@") {
				t.Errorf("error echoes credentials: %v", err)
			}
		})
	}
}

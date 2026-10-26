// Package config reads the relay's configuration from the environment.
//
// Secrets come only from environment variables or from files named by the
// matching *_FILE variable (SECURITY.md §1), never from flags.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo
)

// Reminder modes for REMINDERS.
const (
	RemindersReply  = "reply"
	RemindersSilent = "silent"
	RemindersOff    = "off"
)

// Config is the validated configuration. It holds secrets: String and
// LogValue redact them, so printing a Config by accident is harmless.
type Config struct {
	BotToken     string
	ChatIDs      []int64
	AdminUserIDs []int64

	WebhookSecret string
	ListenAddr    string

	StateDir     string
	TemplateFile string
	Location     *time.Location

	Reminders    string
	ResolveReply bool
	IncidentTTL  time.Duration
	LogLevel     slog.Level
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv, os.ReadFile)
}

func load(getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	var errs []error
	fail := func(err error) { errs = append(errs, err) }

	secret := func(name string) string {
		v, file := getenv(name), getenv(name+"_FILE")
		switch {
		case v != "" && file != "":
			fail(fmt.Errorf("%s and %s_FILE are both set; use one", name, name))
			return ""
		case file != "":
			b, err := readFile(file)
			if err != nil {
				// The path is not secret, the content is; *PathError carries only the path.
				fail(fmt.Errorf("%s_FILE: %w", name, err))
				return ""
			}
			return strings.TrimSpace(string(b))
		default:
			return strings.TrimSpace(v)
		}
	}
	str := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}

	c := &Config{
		BotToken:      secret("TELEGRAM_BOT_TOKEN"),
		WebhookSecret: secret("WEBHOOK_SECRET"),
		ListenAddr:    str("LISTEN_ADDR", "127.0.0.1:9095"),
		StateDir:      str("STATE_DIR", "/data"),
		TemplateFile:  str("TEMPLATE_FILE", ""),
		Reminders:     str("REMINDERS", RemindersReply),
	}

	if c.BotToken == "" {
		fail(errors.New("TELEGRAM_BOT_TOKEN is required"))
	}
	if c.WebhookSecret == "" {
		fail(errors.New("WEBHOOK_SECRET is required"))
	}

	var err error
	if c.ChatIDs, err = idList(getenv("TELEGRAM_CHAT_IDS")); err != nil {
		fail(fmt.Errorf("TELEGRAM_CHAT_IDS: %w", err))
	}
	if c.AdminUserIDs, err = idList(getenv("TELEGRAM_ADMIN_USER_IDS")); err != nil {
		fail(fmt.Errorf("TELEGRAM_ADMIN_USER_IDS: %w", err))
	}

	if c.Location, err = time.LoadLocation(str("TIMEZONE", "UTC")); err != nil {
		fail(fmt.Errorf("TIMEZONE: %w", err))
	}

	switch c.Reminders {
	case RemindersReply, RemindersSilent, RemindersOff:
	default:
		fail(fmt.Errorf("REMINDERS: must be reply, silent or off, got %q", c.Reminders))
	}

	if c.ResolveReply, err = strconv.ParseBool(str("RESOLVE_REPLY", "false")); err != nil {
		fail(fmt.Errorf("RESOLVE_REPLY: %w", err))
	}

	if c.IncidentTTL, err = time.ParseDuration(str("INCIDENT_TTL", "720h")); err != nil {
		fail(fmt.Errorf("INCIDENT_TTL: %w", err))
	} else if c.IncidentTTL <= 0 {
		fail(errors.New("INCIDENT_TTL: must be positive"))
	}

	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		fail(fmt.Errorf("LOG_LEVEL: %w", err))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// idList parses a required, comma-separated list of Telegram IDs. An empty
// list is an error: there is no "allow everyone" mode (SECURITY.md §2).
func idList(s string) ([]int64, error) {
	var ids []int64
	seen := map[int64]bool{}
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id == 0 {
			// Do not echo the value: it may be a real ID.
			return nil, errors.New("not a list of numeric IDs")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("required, at least one ID")
	}
	return ids, nil
}

// DefaultChatID is the chat notifications go to until routing exists (v0.3).
func (c *Config) DefaultChatID() int64 { return c.ChatIDs[0] }

// String returns a redacted description safe to log. Value receivers, so
// both Config and *Config are covered.
func (c Config) String() string {
	return fmt.Sprintf("Config{ListenAddr:%s StateDir:%s Chats:%d Admins:%d BotToken:%s WebhookSecret:%s}",
		c.ListenAddr, c.StateDir, len(c.ChatIDs), len(c.AdminUserIDs), redact(c.BotToken), redact(c.WebhookSecret))
}

// GoString keeps %#v from printing the fields.
func (c Config) GoString() string { return c.String() }

// LogValue implements slog.LogValuer with the same redaction as String.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen_addr", c.ListenAddr),
		slog.String("state_dir", c.StateDir),
		slog.Int("chats", len(c.ChatIDs)),
		slog.Int("admins", len(c.AdminUserIDs)),
		slog.String("reminders", c.Reminders),
		slog.Bool("resolve_reply", c.ResolveReply),
		slog.String("incident_ttl", c.IncidentTTL.String()),
		slog.Bool("custom_template", c.TemplateFile != ""),
	)
}

func redact(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

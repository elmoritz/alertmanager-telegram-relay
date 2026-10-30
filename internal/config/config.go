// Package config reads the relay's configuration from the environment.
//
// Secrets come only from environment variables or from files named by the
// matching *_FILE variable (SECURITY.md §1), never from flags.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo

	"github.com/elmoritz/alertmanager-telegram-relay/internal/route"
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

	// AlertmanagerURL is nil when unset; silence buttons are then not offered.
	AlertmanagerURL      *url.URL
	AlertmanagerUsername string
	AlertmanagerPassword string
	AlertmanagerToken    string
	SilenceDurations     []time.Duration

	GrafanaURL string
	// GrafanaToken is a service-account token for rendering panels (graph
	// images, SPEC §8); empty: no graph images.
	GrafanaToken     string
	ShowSourceButton bool
	ProtectContent   bool

	// Routes pick a chat and topic by labels; without a match, notifications
	// go to the first of ChatIDs.
	Routes          []route.Rule
	QuietSeverities []string
	QuietHours      *route.Window // nil: none
	// EscalateAfter is ESCALATE_AFTER; 0 turns escalation off.
	EscalateAfter time.Duration
	StatusBoard   bool
	// DigestAt is DIGEST_AT in minutes after midnight in TIMEZONE; nil: off.
	DigestAt *int
}

// MaxSilence is the longest silence a button may create (SECURITY.md §3).
const MaxSilence = 7 * 24 * time.Hour

const maxSilenceButtons = 4

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

	if v := str("ALERTMANAGER_URL", ""); v != "" {
		if c.AlertmanagerURL, err = httpBaseURL(v); err != nil {
			fail(fmt.Errorf("ALERTMANAGER_URL: %w", err))
		}
	}
	c.AlertmanagerUsername = secret("ALERTMANAGER_USERNAME")
	c.AlertmanagerPassword = secret("ALERTMANAGER_PASSWORD")
	c.AlertmanagerToken = secret("ALERTMANAGER_BEARER_TOKEN")
	switch {
	case (c.AlertmanagerUsername == "") != (c.AlertmanagerPassword == ""):
		fail(errors.New("ALERTMANAGER_USERNAME and ALERTMANAGER_PASSWORD must be set together"))
	case c.AlertmanagerUsername != "" && c.AlertmanagerToken != "":
		fail(errors.New("ALERTMANAGER_BEARER_TOKEN and ALERTMANAGER_USERNAME are both set; use one"))
	}

	if c.SilenceDurations, err = durations(str("SILENCE_DURATIONS", "1h,24h")); err != nil {
		fail(fmt.Errorf("SILENCE_DURATIONS: %w", err))
	}

	if v := str("GRAFANA_URL", ""); v != "" {
		if u, err := httpBaseURL(v); err != nil {
			fail(fmt.Errorf("GRAFANA_URL: %w", err))
		} else {
			c.GrafanaURL = u.String()
		}
	}
	c.GrafanaToken = secret("GRAFANA_TOKEN")
	if c.GrafanaToken != "" && c.GrafanaURL == "" {
		fail(errors.New("GRAFANA_TOKEN needs GRAFANA_URL"))
	}
	if c.ShowSourceButton, err = strconv.ParseBool(str("SHOW_SOURCE_BUTTON", "false")); err != nil {
		fail(fmt.Errorf("SHOW_SOURCE_BUTTON: %w", err))
	}
	if c.ProtectContent, err = strconv.ParseBool(str("PROTECT_CONTENT", "false")); err != nil {
		fail(fmt.Errorf("PROTECT_CONTENT: %w", err))
	}

	if c.Routes, err = route.Parse(getenv("ROUTES"), c.ChatIDs); err != nil {
		fail(fmt.Errorf("ROUTES: %w", err))
	}
	for _, s := range strings.Split(str("QUIET_SEVERITIES", ""), ",") {
		switch s = strings.TrimSpace(s); s {
		case "":
		case route.Critical:
			fail(errors.New("QUIET_SEVERITIES: critical always rings and cannot be quiet"))
		default:
			c.QuietSeverities = append(c.QuietSeverities, s)
		}
	}
	if v := str("DIGEST_AT", ""); v != "" {
		t, err := time.Parse("15:04", v)
		if err != nil {
			fail(errors.New(`DIGEST_AT: want "HH:MM"`))
		} else {
			m := t.Hour()*60 + t.Minute()
			c.DigestAt = &m
		}
	}
	if c.StatusBoard, err = strconv.ParseBool(str("STATUS_BOARD", "false")); err != nil {
		fail(fmt.Errorf("STATUS_BOARD: %w", err))
	}
	if v := str("ESCALATE_AFTER", ""); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			fail(fmt.Errorf("ESCALATE_AFTER: %w", err))
		case d < time.Minute:
			fail(errors.New("ESCALATE_AFTER: must be at least 1m"))
		default:
			c.EscalateAfter = d
		}
	}
	if v := str("QUIET_HOURS", ""); v != "" {
		w, err := route.ParseWindow(v)
		if err != nil {
			fail(fmt.Errorf("QUIET_HOURS: %w", err))
		}
		c.QuietHours = &w
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

// httpBaseURL accepts an absolute http(s) URL without credentials, query or
// fragment. Credentials belong in their own variables, where they are
// handled as secrets.
func httpBaseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return nil, errors.New("not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("must start with http:// or https://")
	case u.Host == "":
		return nil, errors.New("missing host")
	case u.User != nil:
		return nil, errors.New("must not contain credentials")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("must not contain a query or fragment")
	}
	return u, nil
}

// durations parses the silence button list: positive, at most MaxSilence
// each, no more than fit in one keyboard row.
func durations(s string) ([]time.Duration, error) {
	var ds []time.Duration
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		d, err := time.ParseDuration(f)
		if err != nil {
			return nil, err
		}
		if d < time.Minute || d > MaxSilence {
			return nil, fmt.Errorf("%s: must be between 1m and %s", f, MaxSilence)
		}
		if !slices.Contains(ds, d) {
			ds = append(ds, d)
		}
	}
	if len(ds) == 0 {
		return nil, errors.New("at least one duration required")
	}
	if len(ds) > maxSilenceButtons {
		return nil, fmt.Errorf("at most %d durations", maxSilenceButtons)
	}
	return ds, nil
}

// Router routes notifications by ROUTES, defaulting to the first chat.
func (c *Config) Router() route.Router {
	return route.Router{Rules: c.Routes, Default: route.Target{ChatID: c.ChatIDs[0]}}
}

// Quiet is the QUIET_SEVERITIES and QUIET_HOURS setting, in TIMEZONE.
func (c *Config) Quiet() route.Quiet {
	return route.Quiet{Severities: c.QuietSeverities, Hours: c.QuietHours, Location: c.Location}
}

// String returns a redacted description safe to log. Value receivers, so
// both Config and *Config are covered.
func (c Config) String() string {
	return fmt.Sprintf("Config{ListenAddr:%s StateDir:%s Chats:%d Admins:%d BotToken:%s WebhookSecret:%s AlertmanagerAuth:%s GrafanaToken:%s}",
		c.ListenAddr, c.StateDir, len(c.ChatIDs), len(c.AdminUserIDs), redact(c.BotToken), redact(c.WebhookSecret), c.alertmanagerAuth(), redact(c.GrafanaToken))
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
		slog.Bool("alertmanager", c.AlertmanagerURL != nil),
		slog.String("alertmanager_auth", c.alertmanagerAuth()),
		slog.Any("silence_durations", durationStrings(c.SilenceDurations)),
		slog.Bool("grafana_url", c.GrafanaURL != ""),
		slog.Bool("graph_images", c.GrafanaToken != ""),
		slog.Bool("show_source_button", c.ShowSourceButton),
		slog.Bool("protect_content", c.ProtectContent),
		slog.Int("routes", len(c.Routes)),
		slog.Any("quiet_severities", c.QuietSeverities),
		slog.Bool("quiet_hours", c.QuietHours != nil),
		slog.String("escalate_after", c.EscalateAfter.String()),
		slog.Bool("status_board", c.StatusBoard),
		slog.Bool("digest", c.DigestAt != nil),
	)
}

func (c Config) alertmanagerAuth() string {
	switch {
	case c.AlertmanagerToken != "":
		return "bearer"
	case c.AlertmanagerUsername != "":
		return "basic"
	}
	return "none"
}

func durationStrings(ds []time.Duration) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.String())
	}
	return out
}

func redact(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

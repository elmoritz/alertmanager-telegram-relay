// Package render turns Alertmanager notifications into Telegram HTML.
//
// Templates are html/template, so every value from an alert is escaped for
// HTML by construction (SECURITY.md §5). Telegram accepts the numeric
// entities html/template produces (&#39;, &#34;, …) besides &lt; &gt; &amp;.
package render

import (
	"bytes"
	"cmp"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
)

// MaxLen is Telegram's message length limit, in UTF-16 code units after
// entity parsing.
const MaxLen = 4096

//go:embed default.tmpl
var defaultTemplate string

// Alerts is a list of alerts with the helpers Alertmanager templates offer.
type Alerts []alertmanager.Alert

// Firing returns the firing alerts.
func (as Alerts) Firing() Alerts { return as.with(alertmanager.StatusFiring) }

// Resolved returns the resolved alerts.
func (as Alerts) Resolved() Alerts { return as.with(alertmanager.StatusResolved) }

func (as Alerts) with(status string) Alerts {
	var out Alerts
	for _, a := range as {
		if a.Status == status {
			out = append(out, a)
		}
	}
	return out
}

// Data is what templates see. The first block mirrors Alertmanager's
// template data, so existing templates port with little change.
type Data struct {
	Receiver          string
	Status            string
	GroupLabels       map[string]string
	CommonLabels      map[string]string
	CommonAnnotations map[string]string
	ExternalURL       string
	Alerts            Alerts
	TruncatedAlerts   int

	// Added holds the alerts that started firing since the last message
	// (template "added").
	Added Alerts
	// OpenedAt is when the incident's first message was sent.
	OpenedAt time.Time
	// Now is the time of rendering.
	Now time.Time
	// Limits shrink step by step until the message fits; templates should
	// respect them (see default.tmpl).
	Limits Limits
}

// Limits bound how much of a notification a template shows.
type Limits struct {
	MaxAlerts   int // alerts listed before "…and N more"
	Description int // characters of the description; 0 hides it
	Value       int // characters of any single label or annotation in a line
}

// steps are tried in order until the rendered message fits.
var steps = []Limits{
	{MaxAlerts: 6, Description: 1500, Value: 200},
	{MaxAlerts: 6, Description: 500, Value: 150},
	{MaxAlerts: 3, Description: 200, Value: 100},
	{MaxAlerts: 1, Description: 0, Value: 60},
}

// NewData builds template data from a notification.
func NewData(m *alertmanager.Message) Data {
	return Data{
		Receiver:          m.Receiver,
		Status:            m.Status,
		GroupLabels:       orEmpty(m.GroupLabels),
		CommonLabels:      orEmpty(m.CommonLabels),
		CommonAnnotations: orEmpty(m.CommonAnnotations),
		ExternalURL:       m.ExternalURL,
		Alerts:            Alerts(m.Alerts),
		TruncatedAlerts:   m.TruncatedAlerts,
	}
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Renderer renders messages. It is safe for concurrent use.
type Renderer struct {
	tmpl *template.Template
	loc  *time.Location
	now  func() time.Time
}

// New parses the built-in templates and, if customFile is not empty, a user
// template file whose definitions replace the built-in ones. The result is
// tried on a sample notification so that a broken template fails at startup.
// now is the clock (time.Now if nil); loc is TIMEZONE.
func New(customFile string, loc *time.Location, now func() time.Time) (*Renderer, error) {
	if now == nil {
		now = time.Now
	}
	r := &Renderer{loc: loc, now: now}
	t, err := template.New("default").Funcs(r.funcs()).Parse(defaultTemplate)
	if err != nil {
		return nil, fmt.Errorf("built-in template: %w", err)
	}
	if customFile != "" {
		if t, err = t.ParseFiles(customFile); err != nil {
			return nil, fmt.Errorf("TEMPLATE_FILE: %w", err)
		}
	}
	r.tmpl = t
	for _, d := range sampleData(now()) {
		for _, name := range []string{"message", "added", "reminder", "resolve_reply"} {
			if _, err := r.execute(name, d); err != nil {
				return nil, fmt.Errorf("template %q fails on a sample notification: %w", name, err)
			}
		}
	}
	return r, nil
}

// Message renders the main incident message. It always returns text that
// fits Telegram's limit: if the template output is too long even with the
// smallest limits, or the template fails, it falls back to Minimal. A
// non-nil error reports such a template failure (the text is still usable).
func (r *Renderer) Message(d Data) (string, error) {
	d.Now = r.now()
	var tmplErr error
	for _, l := range steps {
		d.Limits = l
		s, err := r.execute("message", d)
		if err != nil {
			tmplErr = err
			break
		}
		if n := Len(s); n > 0 && n <= MaxLen {
			return s, nil
		}
	}
	return r.Minimal(d), tmplErr
}

// Added renders the reply announcing newly firing alerts.
func (r *Renderer) Added(d Data) (string, error) {
	return r.reply("added", d, fmt.Sprintf("➕ %d more firing", len(d.Added)))
}

// Reminder renders the reply to a repeated notification with no change.
func (r *Renderer) Reminder(d Data) (string, error) {
	return r.reply("reminder", d, "⏰ still firing")
}

// ResolveReply renders the optional reply sent when an incident resolves.
func (r *Renderer) ResolveReply(d Data) (string, error) {
	return r.reply("resolve_reply", d, "✅ resolved")
}

func (r *Renderer) reply(name string, d Data, fallback string) (string, error) {
	d.Now = r.now()
	d.Limits = steps[0]
	s, err := r.execute(name, d)
	if err != nil {
		return fallback, err
	}
	if n := Len(s); n == 0 || n > MaxLen {
		return fallback, nil
	}
	return s, nil
}

// Minimal renders a short message without any template. Its length is
// bounded by construction, so it always fits: this is what a notification
// degrades to instead of being lost.
func (r *Renderer) Minimal(d Data) string {
	var b strings.Builder
	if d.Status == alertmanager.StatusResolved {
		b.WriteString("✅ ")
	} else {
		b.WriteString(severityEmoji(severity(d)) + " ")
	}
	b.WriteString("<b>" + html.EscapeString(clip(100, oneline(alertname(d)))) + "</b>")
	if d.Status == alertmanager.StatusResolved {
		b.WriteString(" resolved")
	} else {
		fmt.Fprintf(&b, " · %d firing", len(d.Alerts.Firing())+d.TruncatedAlerts)
	}
	b.WriteString("\n\n(details too long to show here, see Alertmanager)")
	return b.String()
}

func (r *Renderer) execute(name string, d Data) (string, error) {
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, name, d); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// Len returns the length Telegram counts for an HTML message: UTF-16 code
// units of the text after tags are removed and entities decoded. Every "<"
// in rendered output is either a tag from the relay's own template or
// escaped, so stripping tags this way is exact.
func Len(s string) int {
	text := html.UnescapeString(tagRE.ReplaceAllString(s, ""))
	return len(utf16.Encode([]rune(strings.TrimSpace(text))))
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"clip":      clip,
		"oneline":   oneline,
		"first":     first,
		"more":      more,
		"summary":   summary,
		"alertname": alertname,
		"severity":  severity,
		"earliest":  earliest,
		"duration":  duration,
		"httpURL":   httpURL,
		"tgtime":    r.tgtime,
		"tgago":     r.tgago,
		"description": func(a alertmanager.Alert) string {
			return cmp.Or(a.Annotations["description"], a.Annotations["message"])
		},
	}
}

// clip shortens s to at most n characters, marking the cut with "…".
func clip(n int, s string) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return strings.TrimSpace(string(rs[:n-1])) + "…"
}

// oneline collapses all whitespace, including newlines, to single spaces.
func oneline(s string) string { return strings.Join(strings.Fields(s), " ") }

// first returns at most n alerts.
func first(n int, as Alerts) Alerts {
	if n < 0 {
		n = 0
	}
	return as[:min(n, len(as))]
}

// more returns how many alerts are not shown when listing n of as, counting
// those Alertmanager already truncated.
func more(n int, as Alerts, truncated int) int {
	return max(len(as)-n, 0) + truncated
}

// summary is the one-line text for an alert.
func summary(a alertmanager.Alert) string {
	return cmp.Or(a.Annotations["summary"], a.Labels["instance"], a.Labels["alertname"], "alert")
}

func alertname(d Data) string {
	if n := d.CommonLabels["alertname"]; n != "" {
		return n
	}
	names := map[string]bool{}
	for _, a := range d.Alerts {
		names[a.Labels["alertname"]] = true
	}
	if len(names) == 1 && len(d.Alerts) > 0 && d.Alerts[0].Labels["alertname"] != "" {
		return d.Alerts[0].Labels["alertname"]
	}
	return fmt.Sprintf("%d alerts", len(d.Alerts))
}

// severity is the group's severity: the common label, else the most severe
// firing alert's.
func severity(d Data) string {
	if s := d.CommonLabels["severity"]; s != "" {
		return s
	}
	rank := map[string]int{"critical": 3, "warning": 2, "info": 1}
	best := ""
	for _, a := range d.Alerts.Firing() {
		if s := a.Labels["severity"]; rank[s] > rank[best] {
			best = s
		}
	}
	return best
}

// severityEmoji matches the "severity_emoji" template; Minimal cannot use
// templates.
func severityEmoji(s string) string {
	switch s {
	case "critical":
		return "🔥"
	case "warning":
		return "⚠️"
	}
	return "ℹ️"
}

// earliest returns the earliest start time among as, or the zero time.
func earliest(as Alerts) time.Time {
	if len(as) == 0 {
		return time.Time{}
	}
	return slices.MinFunc(as, func(a, b alertmanager.Alert) int { return a.StartsAt.Compare(b.StartsAt) }).StartsAt
}

// duration formats to-from compactly ("45m", "2h 5m", "3d 4h"), or "" if
// either is unset or the span is negative.
func duration(from, to time.Time) string {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return ""
	}
	d := to.Sub(from)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, h)
	}
}

// httpURL returns s if it is an absolute http(s) URL, else "" (SECURITY.md §5).
func httpURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

var tgFormatRE = regexp.MustCompile(`^(r|w?[dD]?[tT]?)$`)

// tgtime renders t as a Telegram date-time entity, shown in each reader's
// own time zone. The text inside is the fallback for clients that do not
// support it, in TIMEZONE.
func (r *Renderer) tgtime(t time.Time, format string) (template.HTML, error) {
	if !tgFormatRE.MatchString(format) {
		return "", errors.New("tgtime: invalid format")
	}
	if t.IsZero() {
		return "", nil
	}
	fallback := t.In(r.loc).Format("Mon 15:04 MST")
	if !strings.ContainsAny(format, "dD") && r.now().Sub(t) > 6*24*time.Hour {
		fallback = t.In(r.loc).Format("Mon 2 Jan 15:04 MST")
	}
	return template.HTML(fmt.Sprintf(`<tg-time unix="%d" format="%s">%s</tg-time>`, t.Unix(), format, html.EscapeString(fallback))), nil
}

// tgago renders t relative to now ("2h 23m ago"), live-updating in clients
// that support date-time entities.
func (r *Renderer) tgago(t time.Time) template.HTML {
	d := duration(t, r.now())
	if d == "" {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<tg-time unix="%d" format="r">%s ago</tg-time>`, t.Unix(), html.EscapeString(d)))
}

// sampleData is used to try templates at startup.
func sampleData(now time.Time) []Data {
	a := alertmanager.Alert{
		Status:      alertmanager.StatusFiring,
		Labels:      map[string]string{"alertname": "Sample", "instance": "host-1", "severity": "warning"},
		Annotations: map[string]string{"summary": "sample summary", "description": "sample description"},
		StartsAt:    now.Add(-time.Hour),
	}
	b := a
	b.Status, b.EndsAt = alertmanager.StatusResolved, now
	firing := Data{Status: alertmanager.StatusFiring, Alerts: Alerts{a, b}, Added: Alerts{a},
		GroupLabels: map[string]string{"alertname": "Sample"}, CommonLabels: map[string]string{"alertname": "Sample"},
		CommonAnnotations: map[string]string{}, OpenedAt: now.Add(-time.Hour), Limits: steps[0]}
	resolved := firing
	resolved.Status, resolved.Alerts = alertmanager.StatusResolved, Alerts{b}
	return []Data{firing, resolved}
}

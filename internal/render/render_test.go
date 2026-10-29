package render

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testNow = time.Date(2026, 3, 2, 7, 19, 0, 0, time.UTC)

func renderer(t *testing.T, custom string) *Renderer {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Berlin")
	r, err := New(custom, loc, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func fixture(t *testing.T, name string) *alertmanager.Message {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m alertmanager.Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return &m
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./... -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGolden(t *testing.T) {
	r := renderer(t, "")
	for _, name := range []string{"firing_single", "firing_group", "firing_many", "resolved_single", "resolved_group", "hostile"} {
		t.Run(name, func(t *testing.T) {
			d := NewData(fixture(t, name))
			d.OpenedAt = time.Date(2026, 3, 2, 4, 57, 0, 0, time.UTC)
			d.Added = d.Alerts.Firing()
			if name == "firing_single" {
				d.Silence = &Silence{Until: time.Date(2026, 3, 2, 8, 19, 0, 0, time.UTC), By: "Alice <b>"}
			}
			msg, err := r.Message(d)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			out.WriteString(msg + "\n")
			replies := []func(Data) (string, error){r.Added, r.Reminder}
			if d.Status == alertmanager.StatusResolved {
				replies = []func(Data) (string, error){r.ResolveReply}
			}
			for _, f := range replies {
				s, err := f(d)
				if err != nil {
					t.Fatal(err)
				}
				out.WriteString("---\n" + s + "\n")
			}
			golden(t, name, out.String())
			assertWellFormed(t, msg)
		})
	}
}

var allowedTag = regexp.MustCompile(`^(<(b|s|blockquote( expandable)?|tg-time unix="\d+" format="[a-zA-Z]*")>|</(b|s|blockquote|tg-time)>)$`)

// assertWellFormed checks that every tag in s comes from the relay's
// template: alert content must never produce markup.
func assertWellFormed(t *testing.T, s string) {
	t.Helper()
	for _, tag := range tagRE.FindAllString(s, -1) {
		if !allowedTag.MatchString(tag) {
			t.Errorf("unexpected tag %q in:\n%s", tag, s)
		}
	}
	for _, ent := range regexp.MustCompile(`&[a-zA-Z]+;`).FindAllString(s, -1) {
		switch ent {
		case "&lt;", "&gt;", "&amp;", "&quot;":
		default:
			t.Errorf("named entity %s is not supported by Telegram", ent)
		}
	}
}

func TestHostileContentIsEscaped(t *testing.T) {
	d := NewData(fixture(t, "hostile"))
	msg, err := renderer(t, "").Message(d)
	if err != nil {
		t.Fatal(err)
	}
	// As escaped text these strings are harmless; as markup they are not.
	for _, bad := range []string{"<script", "<a ", "<tg-spoiler", "<b>bold"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message contains %q:\n%s", bad, msg)
		}
	}
	assertWellFormed(t, msg)
}

func bigAlert(i int, text string) alertmanager.Alert {
	return alertmanager.Alert{
		Status: alertmanager.StatusFiring,
		Labels: map[string]string{"alertname": "Huge" + text, "instance": fmt.Sprintf("server-%d-%s", i, text), "severity": "critical"},
		Annotations: map[string]string{
			"summary":     strings.Repeat("summary & <x> ", 400),
			"description": strings.Repeat("😀 description line & more\n", 2000),
		},
		StartsAt:    testNow.Add(-time.Hour),
		Fingerprint: fmt.Sprint(i),
	}
}

func TestLengthAlwaysFits(t *testing.T) {
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
			for _, status := range []string{alertmanager.StatusFiring, alertmanager.StatusResolved} {
				d := NewData(m)
				d.Status = status
				d.Added = d.Alerts
				msg, err := r.Message(d)
				if err != nil {
					t.Fatal(err)
				}
				if n := Len(msg); n == 0 || n > MaxLen {
					t.Errorf("%s: length %d", status, n)
				}
				if !strings.Contains(msg, "Huge") {
					t.Errorf("%s: alert name lost:\n%.300s", status, msg)
				}
				assertWellFormed(t, msg)
				added, _ := r.Added(d)
				if n := Len(added); n == 0 || n > MaxLen {
					t.Errorf("added reply length %d", n)
				}
			}
		})
	}
}

func TestShrinksBeforeFallingBack(t *testing.T) {
	m := &alertmanager.Message{Status: alertmanager.StatusFiring}
	for i := range 6 {
		a := bigAlert(i, "")
		a.Annotations["summary"] = strings.Repeat("s", 300)
		m.Alerts = append(m.Alerts, a)
	}
	msg, _ := renderer(t, "").Message(NewData(m))
	if !strings.Contains(msg, "<blockquote expandable>") {
		t.Errorf("expected a shortened description, not the minimal fallback:\n%s", msg)
	}
}

// An alert can start a little in the future when the Prometheus host's clock
// runs ahead of the relay's; there is no "ago" then, and no empty "()".
func TestStartInTheFuture(t *testing.T) {
	a := bigAlert(0, "")
	a.Annotations = map[string]string{"summary": "s"}
	a.StartsAt = testNow.Add(5 * time.Second)
	msg, err := renderer(t, "").Message(NewData(&alertmanager.Message{Status: alertmanager.StatusFiring, Alerts: []alertmanager.Alert{a}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "since <tg-time") || strings.Contains(msg, "()") {
		t.Errorf("unexpected message:\n%s", msg)
	}
}

func TestLen(t *testing.T) {
	for s, want := range map[string]int{
		"<b>ab</b>":    2,
		"a &amp; b":    5,
		"&#39;x&#34;":  3,
		"😀":            1, // one code point, although two UTF-16 units
		"  <s>x</s>\n": 1,
		`<tg-time unix="1" format="r">2h ago</tg-time>`: 6,
	} {
		if got := Len(s); got != want {
			t.Errorf("Len(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := clip(5, "abcdefgh"); got != "abcd…" {
		t.Errorf("clip = %q", got)
	}
	if got := clip(5, "abc"); got != "abc" {
		t.Errorf("clip short = %q", got)
	}
	if got := clip(0, "abc"); got != "" {
		t.Errorf("clip 0 = %q", got)
	}
	for in, want := range map[string]string{
		"https://grafana.example.com/d/x": "https://grafana.example.com/d/x",
		"http://example.com":              "http://example.com",
		"javascript:alert(1)":             "",
		"tg://user?id=1":                  "",
		"//example.com/x":                 "",
		"data:text/html,x":                "",
	} {
		if got := HTTPURL(in); got != want {
			t.Errorf("httpURL(%q) = %q", in, got)
		}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		30 * time.Second:            "<1m",
		45 * time.Minute:            "45m",
		2 * time.Hour:               "2h",
		2*time.Hour + 5*time.Minute: "2h 5m",
		76 * time.Hour:              "3d 4h",
		48 * time.Hour:              "2d",
		-time.Minute:                "",
	} {
		if got := duration(base, base.Add(d)); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
	if duration(time.Time{}, base) != "" {
		t.Error("zero start should give empty duration")
	}
}

func TestCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom.tmpl")
	if err := os.WriteFile(custom, []byte(`{{define "severity_emoji"}}🚨{{end}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, err := renderer(t, custom).Message(NewData(fixture(t, "firing_single")))
	if err != nil || !strings.HasPrefix(msg, "🚨 <b>HostDown</b>") {
		t.Errorf("custom emoji not used: %q %v", msg, err)
	}

	if err := os.WriteFile(custom, []byte(`{{define "message"}}{{.NoSuchField}}{{end}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("UTC")
	if _, err := New(custom, loc, nil); err == nil {
		t.Error("broken template should fail at startup")
	}
	if _, err := New(filepath.Join(dir, "missing.tmpl"), loc, nil); err == nil {
		t.Error("missing template file should fail at startup")
	}
}

func TestTemplateErrorFallsBackToMinimal(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.tmpl")
	// Passes the startup sample (no instance label), fails on a real alert.
	tmpl := `{{define "message"}}{{if .CommonLabels.instance}}{{index .Alerts 99}}{{end}}x{{end}}`
	if err := os.WriteFile(custom, []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, err := renderer(t, custom).Message(NewData(fixture(t, "firing_single")))
	if err == nil {
		t.Error("want the template error reported")
	}
	if !strings.Contains(msg, "<b>HostDown</b>") || !strings.Contains(msg, "1 firing") {
		t.Errorf("fallback message = %q", msg)
	}
}

package incident

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"iter"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// ButtonConfig controls the inline keyboard (SPEC §5).
type ButtonConfig struct {
	// Silences enables the silence buttons (ALERTMANAGER_URL is set).
	Silences  bool
	Durations []time.Duration
	// ActionTTL is how long a button's action stays valid. Buttons are
	// re-issued once they are past half of it.
	ActionTTL  time.Duration
	GrafanaURL string
	ShowSource bool
}

// newActionID returns 128 random bits, URL-safe: all a button carries.
func newActionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return base64.RawURLEncoding.EncodeToString(b)
}

// issueButtons makes sure inc carries valid callback buttons for its state:
// silence buttons, or an unsilence button while a silence is active, and
// "I'm on it", or "Release" while someone is on it. New actions are stored
// right away; issued reports whether there are any, and stale lists the IDs
// they replace, to be deleted once the new keyboard is out.
//
// No silence buttons are offered for a group without group labels: a
// silence for it would have no matchers (SECURITY.md §3).
func (p *Processor) issueButtons(inc *store.Incident, now time.Time, force bool) (stale []string, issued bool, err error) {
	cfg := p.Buttons
	if inc.Silence != nil && !now.Before(inc.Silence.Until) {
		inc.Silence = nil // ran out by itself
		force = true
	}

	var want []string // button kinds, in keyboard order
	switch {
	case inc.Silence != nil:
		want = append(want, store.ActionUnsilence)
	case cfg.Silences && len(inc.GroupLabels) > 0:
		for range cfg.Durations {
			want = append(want, store.ActionSilence)
		}
	}
	if inc.Ack != nil {
		want = append(want, store.ActionRelease)
	} else {
		want = append(want, store.ActionAck)
	}

	var have []string
	for _, b := range inc.Buttons {
		have = append(have, b.Kind)
	}
	fresh := slices.Equal(have, want) && now.Sub(inc.ButtonsIssued) < cfg.ActionTTL/2
	if fresh && !force {
		return nil, false, nil
	}

	var actions []*store.Action
	var refs []store.ButtonRef
	add := func(a *store.Action, label string) {
		a.ID, a.GroupKey, a.ExpiresAt = newActionID(), inc.GroupKey, now.Add(cfg.ActionTTL)
		actions = append(actions, a)
		refs = append(refs, store.ButtonRef{ActionID: a.ID, Kind: a.Kind, Label: label})
	}
	durations := slices.Values(cfg.Durations)
	next, stop := iter.Pull(durations)
	defer stop()
	for _, kind := range want {
		switch kind {
		case store.ActionUnsilence:
			add(&store.Action{Kind: kind, SilenceID: inc.Silence.ID}, "↩️ Unsilence")
		case store.ActionSilence:
			d, _ := next()
			add(&store.Action{Kind: kind, Duration: d}, "🔕 "+shortDuration(d))
		case store.ActionAck:
			add(&store.Action{Kind: kind}, "👀 I'm on it")
		case store.ActionRelease:
			add(&store.Action{Kind: kind}, "↩️ Release")
		}
	}
	if err := p.Store.PutActions(actions...); err != nil {
		return nil, false, err
	}
	stale, inc.Buttons, inc.ButtonsIssued = inc.ButtonIDs(), refs, now
	return stale, true, nil
}

// unsilenceStyle highlights the button that undoes something; silence
// buttons keep the client's default look.
const unsilenceStyle = telegram.StylePrimary

// maxCopyText is Telegram's limit for a copy button's text (checked against
// the live API: 256 characters are accepted, 257 are not).
const maxCopyText = 256

// keyboard builds the inline keyboard for a message. Firing: a disabled
// status button while silenced, the callback buttons, then links and the copy
// button (withLinks; Telegram refuses some URLs, so there is a variant without
// them). Resolved: one disabled button saying so, nothing that can be pressed.
func (p *Processor) keyboard(inc *store.Incident, m *alertmanager.Message, withLinks bool) *telegram.Keyboard {
	kb := &telegram.Keyboard{}
	if m.Status == alertmanager.StatusResolved {
		kb.Rows = append(kb.Rows, []telegram.Button{{Text: resolvedLabel(m), Disabled: true}})
		return kb
	}
	if s := inc.Silence; s != nil && inc.HasButtonKind(store.ActionUnsilence) {
		label := "🔕 Silenced until " + p.Render.ShortTime(s.Until)
		if by := clipName(s.ByName); by != "" {
			label += " by " + by
		}
		kb.Rows = append(kb.Rows, []telegram.Button{{Text: label, Disabled: true}})
	}
	var row []telegram.Button
	for _, b := range inc.Buttons {
		btn := telegram.Button{Text: b.Label, Data: b.ActionID}
		if b.Kind == store.ActionUnsilence {
			btn.Style = unsilenceStyle
		}
		row = append(row, btn)
	}
	if len(row) > 0 {
		kb.Rows = append(kb.Rows, row)
	}
	if withLinks {
		if links := p.links(m); len(links) > 0 {
			kb.Rows = append(kb.Rows, links)
		}
		if c := copyCommand(m); c != "" {
			kb.Rows = append(kb.Rows, []telegram.Button{{Text: "📋 Copy command", CopyText: c}})
		}
	}
	if len(kb.Rows) == 0 {
		return nil
	}
	return kb
}

// resolvedLabel is the disabled button on a resolved message: how long the
// incident lasted, from its first alert's start to its last alert's end.
func resolvedLabel(m *alertmanager.Message) string {
	var from, to time.Time
	for _, a := range m.Alerts {
		if from.IsZero() || a.StartsAt.Before(from) {
			from = a.StartsAt
		}
		if a.EndsAt.After(to) {
			to = a.EndsAt
		}
	}
	if d := render.Duration(from, to); d != "" {
		return "✅ Resolved after " + d
	}
	return "✅ Resolved"
}

// clipName shortens a Telegram display name for a button label.
func clipName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 32 {
		s = string(r[:31]) + "…"
	}
	return s
}

// annotation returns the first firing alert's annotation name, else the
// common annotation.
func annotation(m *alertmanager.Message, name string) string {
	for _, a := range m.Alerts {
		if a.Status == alertmanager.StatusFiring && a.Annotations[name] != "" {
			return a.Annotations[name]
		}
	}
	return m.CommonAnnotations[name]
}

// links returns the URL buttons for m. Only http(s) URLs are used.
func (p *Processor) links(m *alertmanager.Message) []telegram.Button {
	var row []telegram.Button
	if u := cmp.Or(render.HTTPURL(annotation(m, "dashboard_url")), p.Buttons.GrafanaURL); u != "" {
		row = append(row, telegram.Button{Text: "📈 Grafana", URL: u})
	}
	if u := render.HTTPURL(annotation(m, "runbook_url")); u != "" {
		row = append(row, telegram.Button{Text: "📖 Runbook", URL: u})
	}
	if p.Buttons.ShowSource && len(m.Alerts) > 0 {
		if u := render.HTTPURL(m.Alerts[0].GeneratorURL); u != "" {
			row = append(row, telegram.Button{Text: "🔗 Source", URL: u})
		}
	}
	return row
}

// copyCommand returns the copy_command annotation for a copy button, or ""
// if there is none or it is too long: a clipped command would be a different
// command. The text is the alert rule author's, not a trusted command.
func copyCommand(m *alertmanager.Message) string {
	c := strings.TrimSpace(annotation(m, "copy_command"))
	if utf8.RuneCountInString(c) > maxCopyText {
		return ""
	}
	return c
}

// shortDuration formats button labels: "30m", "1h", "1h30m", "24h".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

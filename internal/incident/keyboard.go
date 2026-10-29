package incident

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"

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
// silence buttons, or an unsilence button while a silence is active. New
// actions are stored right away; issued reports whether there are any, and
// stale lists the IDs they replace, to be deleted once the new keyboard is
// out.
//
// No silence buttons are offered for a group without group labels: a
// silence for it would have no matchers (SECURITY.md §3).
func (p *Processor) issueButtons(inc *store.Incident, now time.Time, force bool) (stale []string, issued bool, err error) {
	cfg := p.Buttons
	if inc.Silence != nil && !now.Before(inc.Silence.Until) {
		inc.Silence = nil // ran out by itself
		force = true
	}

	wantKind := store.ActionSilence
	if inc.Silence != nil {
		wantKind = store.ActionUnsilence
	}
	if !cfg.Silences || (len(inc.GroupLabels) == 0 && inc.Silence == nil) {
		stale, inc.Buttons = inc.ButtonIDs(), nil
		return stale, false, nil
	}
	fresh := len(inc.Buttons) > 0 && inc.Buttons[0].Kind == wantKind && now.Sub(inc.ButtonsIssued) < cfg.ActionTTL/2
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
	if inc.Silence != nil {
		add(&store.Action{Kind: store.ActionUnsilence, SilenceID: inc.Silence.ID}, "↩️ Unsilence")
	} else {
		for _, d := range cfg.Durations {
			add(&store.Action{Kind: store.ActionSilence, Duration: d}, "🔕 "+shortDuration(d))
		}
	}
	if err := p.Store.PutActions(actions...); err != nil {
		return nil, false, err
	}
	stale, inc.Buttons, inc.ButtonsIssued = inc.ButtonIDs(), refs, now
	return stale, true, nil
}

// keyboard builds the inline keyboard for a firing message: callback
// buttons in the first row, links in the second. Resolved messages have none.
func (p *Processor) keyboard(inc *store.Incident, m *alertmanager.Message, withLinks bool) *telegram.Keyboard {
	if m.Status == alertmanager.StatusResolved {
		return nil
	}
	kb := &telegram.Keyboard{}
	var row []telegram.Button
	for _, b := range inc.Buttons {
		row = append(row, telegram.Button{Text: b.Label, Data: b.ActionID})
	}
	if len(row) > 0 {
		kb.Rows = append(kb.Rows, row)
	}
	if withLinks {
		if links := p.links(m); len(links) > 0 {
			kb.Rows = append(kb.Rows, links)
		}
	}
	if len(kb.Rows) == 0 {
		return nil
	}
	return kb
}

// links returns the URL buttons for m. Only http(s) URLs are used.
func (p *Processor) links(m *alertmanager.Message) []telegram.Button {
	annotation := func(name string) string {
		for _, a := range m.Alerts {
			if a.Status == alertmanager.StatusFiring && a.Annotations[name] != "" {
				return a.Annotations[name]
			}
		}
		return m.CommonAnnotations[name]
	}
	var row []telegram.Button
	if u := cmp.Or(render.HTTPURL(annotation("dashboard_url")), p.Buttons.GrafanaURL); u != "" {
		row = append(row, telegram.Button{Text: "📈 Grafana", URL: u})
	}
	if u := render.HTTPURL(annotation("runbook_url")); u != "" {
		row = append(row, telegram.Button{Text: "📖 Runbook", URL: u})
	}
	if p.Buttons.ShowSource && len(m.Alerts) > 0 {
		if u := render.HTTPURL(m.Alerts[0].GeneratorURL); u != "" {
			row = append(row, telegram.Button{Text: "🔗 Source", URL: u})
		}
	}
	return row
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

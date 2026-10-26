// Package incident implements the message lifecycle (SPEC §4): which Telegram
// action a notification leads to, carrying it out with retries, and the
// per-chat queue that feeds it.
package incident

import (
	"maps"
	"slices"
	"strings"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
)

// Kind is what a notification does to its incident.
type Kind int

const (
	// Open: the group fires and has no open incident. Send a new message.
	Open Kind = iota + 1
	// Changed: the set of firing alerts changed. Edit the message; if alerts
	// were added, also reply so the change is noticed.
	Changed
	// Repeat: nothing changed (Alertmanager's repeat_interval). Reminder reply.
	Repeat
	// Resolve: all alerts resolved. Edit to the resolved layout and close.
	Resolve
	// ResolveUnknown: a resolve for a group with no open incident (lost state,
	// expired incident). Send the resolved layout as a new, silent message.
	ResolveUnknown
)

func (k Kind) String() string {
	switch k {
	case Open:
		return "open"
	case Changed:
		return "changed"
	case Repeat:
		return "repeat"
	case Resolve:
		return "resolve"
	case ResolveUnknown:
		return "resolve_unknown"
	}
	return "unknown"
}

// Plan is the decision for one notification.
type Plan struct {
	Kind Kind
	// Firing is the sorted set of alert keys firing after this notification.
	Firing []string
	// Added holds alerts firing now that were not firing before (Changed only).
	Added []alertmanager.Alert
}

// Decide maps a notification and the group's open incident (nil if none) to
// a plan. It has no side effects.
func Decide(inc *store.Incident, m *alertmanager.Message) Plan {
	firing := map[string]alertmanager.Alert{}
	for _, a := range m.Alerts {
		if a.Status == alertmanager.StatusFiring {
			firing[alertKey(a)] = a
		}
	}
	p := Plan{Firing: slices.Sorted(maps.Keys(firing))}

	if m.Status == alertmanager.StatusResolved || len(firing) == 0 {
		if inc == nil {
			p.Kind = ResolveUnknown
		} else {
			p.Kind = Resolve
		}
		return p
	}
	if inc == nil {
		p.Kind = Open
		return p
	}

	before := map[string]bool{}
	for _, k := range inc.Firing {
		before[k] = true
	}
	for _, k := range p.Firing {
		if !before[k] {
			p.Added = append(p.Added, firing[k])
		}
	}
	if len(p.Added) > 0 || !slices.Equal(inc.Firing, p.Firing) {
		p.Kind = Changed
	} else {
		p.Kind = Repeat
	}
	return p
}

// alertKey identifies an alert within its group: Alertmanager's fingerprint,
// or the sorted label set if a sender left the fingerprint out.
func alertKey(a alertmanager.Alert) string {
	if a.Fingerprint != "" {
		return a.Fingerprint
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
		b.WriteString(k + "=" + a.Labels[k] + "\x00")
	}
	return b.String()
}

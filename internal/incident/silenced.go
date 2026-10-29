package incident

import (
	"context"
	"encoding/json"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
)

// SilencedCheckInterval is how often CheckSilenced should run.
const SilencedCheckInterval = time.Minute

// resolveAfterEmpty is how long a silenced group must have no alerts in
// Alertmanager before it counts as resolved. Alertmanager keeps alerts only
// in memory: after it restarts, the list stays empty until Prometheus sends
// the alerts again (every minute by default).
const resolveAfterEmpty = 5 * time.Minute

// CheckSilenced covers what Alertmanager does not report for a group
// silenced from Telegram (SPEC §5): it sends no notification when silenced
// alerts resolve, and none right away when the silence runs out while the
// alerts keep firing. For each such incident it asks Alertmanager for the
// group's alerts. None for resolveAfterEmpty: a resolve notification is
// queued as if Alertmanager had sent it. Still firing after the silence ran
// out: the silence buttons come back.
func (b *Buttons) CheckSilenced(ctx context.Context) {
	if b.Alertmanager == nil {
		return
	}
	keys, err := b.Processor.Store.SilencedIncidents()
	if err != nil {
		b.Log.Error("checking silenced incidents", "err", err)
		return
	}
	silenced := map[string]bool{}
	for _, k := range keys {
		silenced[k] = true
		b.checkSilenced(ctx, k)
	}
	for k := range b.emptySince {
		if !silenced[k] {
			delete(b.emptySince, k)
		}
	}
}

func (b *Buttons) checkSilenced(ctx context.Context, groupKey string) {
	p := b.Processor
	defer p.lock(groupKey)()
	inc, err := p.Store.Incident(groupKey)
	if err != nil || inc == nil || inc.Silence == nil {
		return
	}
	log := b.Log.With("incident", groupID(groupKey))
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		log.Error("reading stored notification", "err", err)
		return
	}
	n, err := b.Alertmanager.ActiveAlerts(ctx, inc.GroupLabels)
	if err != nil {
		log.Warn("checking silenced alerts failed", "err", err)
		return
	}
	now := p.Now()
	if n > 0 {
		delete(b.emptySince, groupKey)
		if !now.Before(inc.Silence.Until) {
			b.refresh(ctx, log.With("action", "silence_ended"), inc, &m, now)
			log.Info("silence ran out, alerts still firing", "action", "silence_ended")
		}
		return
	}
	if b.emptySince == nil {
		b.emptySince = map[string]time.Time{}
	}
	since, seen := b.emptySince[groupKey]
	if !seen {
		b.emptySince[groupKey] = now
		return
	}
	if now.Sub(since) < resolveAfterEmpty {
		return
	}
	if err := b.Enqueue(resolvedCopy(&m, now)); err != nil {
		log.Error("queueing resolve failed", "err", err)
		return
	}
	// The queued resolve closes the incident. Until it is processed, the
	// incident must not count as silenced, or the next check queues another.
	inc.Silence = nil
	if err := p.Store.PutIncident(inc); err != nil {
		log.Error("storing incident failed", "err", err)
	}
	delete(b.emptySince, groupKey)
	log.Info("silenced alerts resolved without a notification", "action", "resolve")
}

// resolvedCopy turns the group's last notification into the resolve
// notification Alertmanager did not send.
func resolvedCopy(m *alertmanager.Message, now time.Time) *alertmanager.Message {
	r := *m
	r.Status = alertmanager.StatusResolved
	r.Alerts = make([]alertmanager.Alert, len(m.Alerts))
	for i, a := range m.Alerts {
		a.Status = alertmanager.StatusResolved
		if a.EndsAt.IsZero() || a.EndsAt.After(now) {
			a.EndsAt = now
		}
		r.Alerts[i] = a
	}
	return &r
}

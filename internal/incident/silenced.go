package incident

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
)

// SilencedCheckInterval is how often CheckSilenced should run.
const SilencedCheckInterval = time.Minute

// resolveAfterEmpty is how long a silenced group must have no alerts in
// Alertmanager before it counts as resolved. Alertmanager keeps alerts only
// in memory: after it restarts, the list stays empty until Prometheus sends
// the alerts again (every minute by default).
const resolveAfterEmpty = 5 * time.Minute

// CheckAlertmanager covers what Alertmanager does not report for a group
// (SPEC §5): it sends nothing for silenced alerts, neither when they are
// silenced nor when they resolve, and nothing right away when a silence runs
// out while the alerts keep firing. Once per run it lists Alertmanager's
// alerts and silences and, for each open incident with group labels:
//   - notes a silence made outside Telegram that covers all its alerts, so
//     the message shows it;
//   - for a silenced incident whose alerts are gone for resolveAfterEmpty,
//     queues the resolve notification Alertmanager did not send;
//   - brings the silence buttons back when a Telegram silence ran out while
//     the alerts still fire.
func (b *Buttons) CheckAlertmanager(ctx context.Context) {
	if b.Alertmanager == nil {
		return
	}
	incs, err := b.Processor.Store.Incidents(func(inc *store.Incident) bool { return len(inc.GroupLabels) > 0 })
	if err != nil {
		b.Log.Error("checking incidents against Alertmanager", "err", err)
		return
	}
	open := map[string]bool{}
	for _, inc := range incs {
		open[inc.GroupKey] = true
	}
	for k := range b.emptySince {
		if !open[k] {
			delete(b.emptySince, k)
		}
	}
	if len(incs) == 0 {
		return
	}
	alerts, err := b.Alertmanager.Alerts(ctx)
	if err != nil {
		b.Log.Warn("listing Alertmanager's alerts failed", "err", err)
		return
	}
	silences, err := b.Alertmanager.ActiveSilences(ctx)
	if err != nil {
		b.Log.Warn("listing Alertmanager's silences failed", "err", err)
		return
	}
	byID := map[string]alertmanager.SilenceInfo{}
	for _, s := range silences {
		byID[s.ID] = s
	}
	for _, inc := range incs {
		b.checkIncident(ctx, inc.GroupKey, alerts, byID)
	}
}

func (b *Buttons) checkIncident(ctx context.Context, groupKey string, alerts []alertmanager.AlertInfo, silences map[string]alertmanager.SilenceInfo) {
	p := b.Processor
	defer p.lock(groupKey)()
	inc, err := p.Store.Incident(groupKey)
	if err != nil || inc == nil || len(inc.GroupLabels) == 0 {
		return
	}
	log := b.Log.With("incident", groupID(groupKey))
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		log.Error("reading stored notification", "err", err)
		return
	}
	var group []alertmanager.AlertInfo
	for _, a := range alerts {
		if hasLabels(a.Labels, inc.GroupLabels) {
			group = append(group, a)
		}
	}
	now := p.Now()

	if len(group) > 0 {
		delete(b.emptySince, groupKey)
		ext := externalSilence(group, silences, inc.Silence)
		extChanged := !sameSilence(inc.ExternalSilence, ext)
		ranOut := inc.Silence != nil && !now.Before(inc.Silence.Until)
		if !extChanged && !ranOut {
			return
		}
		inc.ExternalSilence = ext
		b.refresh(ctx, log.With("action", "silence_changed"), inc, &m, now)
		switch {
		case ranOut:
			log.Info("silence ran out, alerts still firing", "action", "silence_ended")
		case ext != nil:
			log.Info("silenced outside Telegram", "action", "external_silence", "silence_id", ext.ID)
		default:
			log.Info("silence outside Telegram ended", "action", "external_silence")
		}
		return
	}

	// No alerts left. Alertmanager reports that itself, unless they were
	// silenced.
	if inc.Silence == nil && inc.ExternalSilence == nil {
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
	inc.Silence, inc.ExternalSilence = nil, nil
	if err := p.Store.PutIncident(inc); err != nil {
		log.Error("storing incident failed", "err", err)
	}
	delete(b.emptySince, groupKey)
	log.Info("silenced alerts resolved without a notification", "action", "resolve")
}

func hasLabels(labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// externalSilence returns a silence made outside Telegram if every alert of
// the group is silenced by one (own, the incident's Telegram silence, does
// not count). Until is when the first alert stops being silenced.
func externalSilence(group []alertmanager.AlertInfo, silences map[string]alertmanager.SilenceInfo, own *store.Silence) *store.Silence {
	var ext *store.Silence
	for _, a := range group {
		var best *alertmanager.SilenceInfo
		for _, id := range a.Status.SilencedBy {
			s, ok := silences[id]
			if !ok || (own != nil && id == own.ID) {
				continue
			}
			if best == nil || s.EndsAt.After(best.EndsAt) {
				best = &s
			}
		}
		if best == nil {
			return nil
		}
		if ext == nil || best.EndsAt.Before(ext.Until) {
			by := best.CreatedBy
			if strings.HasPrefix(by, "telegram:") {
				by = "Telegram" // never show a user ID
			}
			ext = &store.Silence{ID: best.ID, Until: best.EndsAt, ByName: by}
		}
	}
	return ext
}

func sameSilence(a, b *store.Silence) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.Until.Equal(b.Until) && a.ByName == b.ByName
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

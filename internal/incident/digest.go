package incident

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// DigestCheckInterval is how often CheckDigest should run.
const DigestCheckInterval = time.Minute

// HistoryTTL is how long resolved incidents are kept for the digest.
const HistoryTTL = 7 * 24 * time.Hour

// CheckDigest sends each DigestChats chat its daily digest (#12) once per
// day, as soon as it is past DigestAt in TIMEZONE: incidents of the last
// 24 h, what still fires, silences ending soon, the noisiest alert names of
// the last 7 days. Quiet, built from the relay's own state only.
func (p *Processor) CheckDigest(ctx context.Context) {
	if p.DigestAt == nil {
		return
	}
	now := p.Now().In(p.Location)
	if now.Hour()*60+now.Minute() < *p.DigestAt {
		return
	}
	day := now.Format(time.DateOnly)
	if err := p.Store.PruneResolved(now.Add(-HistoryTTL)); err != nil {
		p.Log.Warn("pruning history failed", "err", err)
	}
	for _, chat := range p.DigestChats {
		if last, err := p.Store.LastDigest(chat); err != nil || last == day {
			continue
		}
		if err := p.sendDigest(ctx, chat, now); err != nil {
			p.Log.Warn("sending the daily digest failed", "err", err)
			continue
		}
		if err := p.Store.SetLastDigest(chat, day); err != nil {
			p.Log.Warn("recording the daily digest failed", "err", err)
		}
		p.Log.Info("daily digest sent")
	}
}

func (p *Processor) sendDigest(ctx context.Context, chat int64, now time.Time) error {
	week, err := p.Store.ResolvedSince(now.Add(-HistoryTTL))
	if err != nil {
		return err
	}
	open, err := p.Store.Incidents(func(inc *store.Incident) bool { return inc.ChatID == chat })
	if err != nil {
		return err
	}

	var d render.DigestData
	noisy := map[string]int{}
	type resolved struct {
		item render.DigestItem
		took time.Duration
	}
	var day []resolved
	for _, r := range week {
		if r.ChatID != chat {
			continue
		}
		noisy[r.Alertname]++
		if r.ResolvedAt.After(now.Add(-24 * time.Hour)) {
			day = append(day, resolved{render.DigestItem{Alertname: r.Alertname, Instance: r.Instance, Duration: render.Duration(r.OpenedAt, r.ResolvedAt)}, r.ResolvedAt.Sub(r.OpenedAt)})
		}
	}
	slices.SortFunc(day, func(a, b resolved) int { return cmp.Compare(b.took, a.took) })
	d.ResolvedCount = len(day)
	for _, r := range day[:min(len(day), 10)] {
		d.Resolved = append(d.Resolved, r.item)
	}

	slices.SortFunc(open, func(a, b *store.Incident) int { return a.OpenedAt.Compare(b.OpenedAt) })
	for _, inc := range open {
		var m alertmanager.Message
		if json.Unmarshal(inc.Last, &m) != nil || m.Status != alertmanager.StatusFiring {
			continue
		}
		item := render.DigestItem{Alertname: render.Alertname(render.NewData(&m)), Instance: m.CommonLabels["instance"], Since: inc.OpenedAt}
		noisy[item.Alertname]++
		if len(d.Open) < 5 {
			d.Open = append(d.Open, item)
		}
		if s := inc.Silence; s != nil && s.Until.After(now) && s.Until.Before(now.Add(24*time.Hour)) {
			item.Since = s.Until
			d.SilencesEnding = append(d.SilencesEnding, item)
		}
	}

	names := slices.Collect(maps.Keys(noisy))
	slices.SortFunc(names, func(a, b string) int { return cmp.Or(cmp.Compare(noisy[b], noisy[a]), cmp.Compare(a, b)) })
	for _, n := range names {
		if noisy[n] < 2 || len(d.Noisy) == 5 {
			break
		}
		d.Noisy = append(d.Noisy, render.DigestItem{Alertname: n, Count: noisy[n]})
	}

	text, err := p.Render.Digest(d)
	if err != nil {
		p.Metrics.TemplateErrors.Inc()
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = p.Telegram.SendMessage(ctx, telegram.Message{ChatID: chat, Text: text, Silent: true})
	return err
}

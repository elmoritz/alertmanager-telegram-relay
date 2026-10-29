package incident

import (
	"context"
	"encoding/json"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/route"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
)

// EscalationCheckInterval is how often CheckEscalations should run.
const EscalationCheckInterval = time.Minute

// maxEscalations bounds the escalation replies per incident: loud enough,
// without ringing all night.
const maxEscalations = 3

// CheckEscalations sends a loud reply (#5) to each critical incident that is
// still firing after another EscalateAfter without anyone saying "I'm on it"
// or silencing it, up to maxEscalations times. Quiet severities and quiet
// hours do not apply: only critical incidents escalate, and critical always
// rings.
func (p *Processor) CheckEscalations(ctx context.Context) {
	if p.EscalateAfter <= 0 {
		return
	}
	now := p.Now()
	incs, err := p.Store.Incidents(func(inc *store.Incident) bool { return p.dueForEscalation(inc, now) })
	if err != nil {
		p.Log.Error("checking escalations", "err", err)
		return
	}
	for _, inc := range incs {
		p.escalate(ctx, inc.GroupKey)
	}
}

func (p *Processor) dueForEscalation(inc *store.Incident, now time.Time) bool {
	return inc.Ack == nil && inc.Silence == nil && inc.Escalations < maxEscalations &&
		now.Sub(inc.OpenedAt) >= time.Duration(inc.Escalations+1)*p.EscalateAfter
}

func (p *Processor) escalate(ctx context.Context, groupKey string) {
	defer p.lock(groupKey)()
	inc, err := p.Store.Incident(groupKey)
	now := p.Now()
	if err != nil || inc == nil || !p.dueForEscalation(inc, now) {
		return
	}
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		return
	}
	if m.Status != alertmanager.StatusFiring || render.Severity(render.NewData(&m)) != route.Critical {
		return
	}
	log := p.Log.With("incident", groupID(groupKey), "action", "escalate")
	j := &job{p: p, ctx: ctx, log: log, deadline: now.Add(30 * time.Second), chatID: inc.ChatID, threadID: inc.ThreadID}
	j.reply(inc.MessageID, p.Render.Escalation, p.data(inc, &m, nil), false)
	inc.Escalations++
	if err := p.Store.PutIncident(inc); err != nil {
		log.Error("storing incident failed", "err", err)
	}
	p.Metrics.Escalations.Inc()
	log.Info("unacknowledged critical incident escalated", "escalation", inc.Escalations)
}

package incident

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/route"
)

func critical(alerts ...string) *alertmanager.Message {
	m := gmsg(alerts...)
	for i := range m.Alerts {
		m.Alerts[i].Labels["severity"] = "critical"
	}
	return m
}

func (e *buttonEnv) escalate(after time.Duration) []call {
	e.now = e.now.Add(after)
	e.proc.CheckEscalations(context.Background())
	return e.tg.take()
}

func TestEscalation(t *testing.T) {
	e := newButtonEnv(t)
	e.proc.EscalateAfter = 15 * time.Minute
	w, _ := route.ParseWindow("00:00-23:59")
	e.proc.Quiet = route.Quiet{Hours: &w, Location: time.UTC} // escalations ignore quiet hours
	e.handle(critical("a"))

	if calls := e.escalate(14 * time.Minute); len(calls) > 0 {
		t.Fatalf("escalated early: %v", calls)
	}
	calls := e.escalate(time.Minute)
	expectCalls(t, calls, "sendMessage(reply #1)")
	if !strings.HasPrefix(calls[0].Text, "🚨 still unacknowledged after 15m") {
		t.Errorf("text = %q", calls[0].Text)
	}
	if calls := e.escalate(time.Minute); len(calls) > 0 {
		t.Errorf("escalated again after a minute: %v", calls)
	}
	expectCalls(t, e.escalate(14*time.Minute), "sendMessage(reply #1)")
	expectCalls(t, e.escalate(15*time.Minute), "sendMessage(reply #1)")
	if calls := e.escalate(time.Hour); len(calls) > 0 {
		t.Errorf("more than %d escalations: %v", maxEscalations, calls)
	}
}

func TestEscalationStops(t *testing.T) {
	for name, handle := range map[string]func(e *buttonEnv, kb []call){
		"acknowledged": func(e *buttonEnv, c []call) { e.press(button(t, c[0].Keyboard, "👀 I'm on it"), chat, 1, admin) },
		"silenced":     func(e *buttonEnv, c []call) { e.press(button(t, c[0].Keyboard, "🔕 1h"), chat, 1, admin) },
		"resolved":     func(e *buttonEnv, _ []call) { e.handle(critical("a-")) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newButtonEnv(t)
			e.proc.EscalateAfter = 15 * time.Minute
			handle(e, e.handle(critical("a")))
			if calls := e.escalate(time.Hour); len(calls) > 0 {
				t.Errorf("escalated: %v", calls)
			}
		})
	}
}

func TestOnlyCriticalEscalates(t *testing.T) {
	e := newButtonEnv(t)
	e.proc.EscalateAfter = 15 * time.Minute
	e.handle(gmsg("a")) // severity="warning"
	if calls := e.escalate(time.Hour); len(calls) > 0 {
		t.Errorf("warning escalated: %v", calls)
	}
	e.proc.EscalateAfter = 0
	m := critical("b")
	m.GroupKey = "critical"
	e.handle(m)
	if calls := e.escalate(time.Hour); len(calls) > 0 {
		t.Errorf("escalated with ESCALATE_AFTER unset: %v", calls)
	}
}

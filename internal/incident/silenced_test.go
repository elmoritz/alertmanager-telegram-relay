package incident

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
)

// silencedEnv is a button environment with one incident silenced from
// Telegram, and the notifications CheckSilenced queues.
func silencedEnv(t *testing.T) (*buttonEnv, *[]*alertmanager.Message) {
	e := newButtonEnv(t)
	var queued []*alertmanager.Message
	e.buttons.Enqueue = func(m *alertmanager.Message) error {
		queued = append(queued, m)
		return nil
	}
	e.press(button(t, e.handle(gmsg("a", "b"))[0].Keyboard, "🔕 1h"), chat, 1, admin)
	return e, &queued
}

func (e *buttonEnv) check(after time.Duration) []call {
	e.now = e.now.Add(after)
	e.buttons.CheckSilenced(context.Background())
	return e.tg.take()
}

func TestSilencedAlertsResolveWithoutNotification(t *testing.T) {
	e, queued := silencedEnv(t)
	e.am.alerts = 0

	for _, after := range []time.Duration{0, 4 * time.Minute} {
		if calls := e.check(after); len(calls) > 0 || len(*queued) > 0 {
			t.Fatalf("resolved too early: calls %v, queued %d", calls, len(*queued))
		}
	}
	if got := strings.Join(e.am.filters, " "); got != `alertname="DiskAlmostFull" cluster="lab"` {
		t.Errorf("filters = %s", got)
	}
	e.check(2 * time.Minute) // 6 minutes without alerts
	if len(*queued) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(*queued))
	}
	r := (*queued)[0]
	if r.Status != alertmanager.StatusResolved || r.GroupKey != gmsg().GroupKey || len(r.Alerts) != 2 {
		t.Fatalf("queued %+v", r)
	}
	for _, a := range r.Alerts {
		if a.Status != alertmanager.StatusResolved || a.EndsAt.After(e.now) || a.EndsAt.IsZero() {
			t.Errorf("alert %+v", a)
		}
	}

	// Until the resolve is processed, the next check must not queue another.
	e.check(time.Minute)
	if len(*queued) != 1 {
		t.Errorf("queued %d notifications, want 1", len(*queued))
	}

	calls := e.handle(r)
	expectCalls(t, calls, "editMessageText(#1)")
	if kb := calls[0].Keyboard; kb == nil || !kb.Rows[0][0].Disabled || !strings.HasPrefix(kb.Rows[0][0].Text, "✅ Resolved") {
		t.Errorf("keyboard = %+v", calls[0].Keyboard)
	}
	if inc, _ := e.store.Incident(r.GroupKey); inc != nil {
		t.Error("incident still open")
	}
}

// After an Alertmanager restart the alert list is empty until Prometheus
// sends the alerts again; that must not close the incident.
func TestSilencedCheckSurvivesAlertmanagerRestart(t *testing.T) {
	e, queued := silencedEnv(t)
	e.am.alerts = 0
	e.check(0)
	e.check(2 * time.Minute)
	e.am.alerts = 2 // Prometheus sent them again
	e.check(2 * time.Minute)
	e.am.alerts = 0
	e.check(time.Minute)
	e.check(4 * time.Minute) // 9 minutes in total, but only 4 without alerts
	if len(*queued) != 0 {
		t.Fatalf("queued %d notifications after a gap in the alert list", len(*queued))
	}
	e.check(2 * time.Minute)
	if len(*queued) != 1 {
		t.Errorf("queued %d notifications, want 1", len(*queued))
	}
}

func TestSilencedCheckWhenAlertmanagerIsDown(t *testing.T) {
	e, queued := silencedEnv(t)
	e.am.down = true
	for range 10 {
		if calls := e.check(time.Minute); len(calls) > 0 {
			t.Fatalf("calls %v", calls)
		}
	}
	if len(*queued) != 0 {
		t.Errorf("queued %d notifications while Alertmanager was down", len(*queued))
	}
}

func TestSilenceRunsOutWhileFiring(t *testing.T) {
	e, queued := silencedEnv(t)
	e.am.alerts = 2
	if calls := e.check(30 * time.Minute); len(calls) > 0 {
		t.Fatalf("silence still active, but calls %v", calls)
	}
	calls := e.check(31 * time.Minute)
	expectCalls(t, calls, "editMessageText(#1)")
	if got := strings.Join(row(calls[0].Keyboard, 0), ","); got != "🔕 1h,🔕 24h,👀 I'm on it" || strings.Contains(calls[0].Text, "silenced") {
		t.Errorf("keyboard %q, text %q", got, calls[0].Text)
	}
	if inc, _ := e.store.Incident(gmsg().GroupKey); inc == nil || inc.Silence != nil {
		t.Errorf("incident = %+v", inc)
	}
	if len(*queued) != 0 {
		t.Errorf("queued %d notifications", len(*queued))
	}
	if calls := e.check(time.Minute); len(calls) > 0 {
		t.Errorf("no longer silenced, but calls %v", calls)
	}
}

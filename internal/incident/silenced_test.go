package incident

import (
	"context"
	"encoding/json"
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
	e.buttons.CheckAlertmanager(context.Background())
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
	if got := strings.Join(row(calls[0].Keyboard, 0), ","); got != "🔕 1h,🔕 24h,⏱ Custom…" || strings.Contains(calls[0].Text, "silenced") {
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

func TestHasLabels(t *testing.T) {
	group := map[string]string{"alertname": "DiskAlmostFull", "cluster": "lab"}
	for labels, want := range map[string]bool{
		`{"alertname":"DiskAlmostFull","cluster":"lab","instance":"a"}`: true,
		`{"alertname":"DiskAlmostFull","cluster":"prod"}`:               false,
		`{"alertname":"DiskAlmostFull"}`:                                false,
	} {
		var m map[string]string
		_ = json.Unmarshal([]byte(labels), &m)
		if got := hasLabels(m, group); got != want {
			t.Errorf("hasLabels(%s) = %v", labels, got)
		}
	}
}

func TestExternalSilenceShown(t *testing.T) {
	e := newButtonEnv(t)
	e.am.foreign = true
	e.am.alerts = 1
	e.handle(gmsg("a"))

	if calls := e.check(time.Minute); len(calls) > 0 {
		t.Fatalf("not silenced, but calls %v", calls)
	}
	e.am.silencedBy = []string{foreignSilenceID}
	calls := e.check(time.Minute)
	expectCalls(t, calls, "editMessageText(#1)")
	c := calls[0]
	if !strings.Contains(c.Text, "silenced until") || !strings.Contains(c.Text, "by ops@example.com in Alertmanager") {
		t.Errorf("text = %q", c.Text)
	}
	if got := strings.Join(row(c.Keyboard, 0), ","); !strings.HasPrefix(got, "🔕 Silenced in Alertmanager until") {
		t.Errorf("row 0 = %q", got)
	}
	for _, r := range c.Keyboard.Rows {
		for _, b := range r {
			if strings.HasPrefix(b.Text, "🔕 1h") || b.Text == "↩️ Unsilence" {
				t.Errorf("silence buttons offered while silenced in Alertmanager: %q", b.Text)
			}
		}
	}
	if got := e.reactions(); got != "#1 😴" {
		t.Errorf("reactions = %q", got)
	}
	if calls := e.check(time.Minute); len(calls) > 0 {
		t.Errorf("unchanged, but calls %v", calls)
	}

	// Ended in Alertmanager: the silence buttons come back.
	e.am.silencedBy = nil
	calls = e.check(time.Minute)
	expectCalls(t, calls, "editMessageText(#1)")
	if got := strings.Join(row(calls[0].Keyboard, 0), ","); got != "🔕 1h,🔕 24h,⏱ Custom…" || strings.Contains(calls[0].Text, "silenced") {
		t.Errorf("after the silence ended: row %q, text %q", got, calls[0].Text)
	}
}

// Alertmanager sends no resolve for alerts silenced outside Telegram either.
func TestExternallySilencedAlertsResolve(t *testing.T) {
	e := newButtonEnv(t)
	var queued int
	e.buttons.Enqueue = func(*alertmanager.Message) error { queued++; return nil }
	e.am.foreign, e.am.alerts, e.am.silencedBy = true, 1, []string{foreignSilenceID}
	e.handle(gmsg("a"))
	e.check(time.Minute) // notes the external silence
	e.am.alerts = 0
	e.check(time.Minute)
	e.check(6 * time.Minute)
	if queued != 1 {
		t.Errorf("queued %d resolves, want 1", queued)
	}
}

// A silence without its own incident's Telegram silence ID, made from
// Telegram elsewhere (e.g. another incident), never shows a user ID.
func TestExternalSilenceHidesTelegramUser(t *testing.T) {
	s := externalSilence([]alertmanager.AlertInfo{{Status: struct {
		State      string   `json:"state"`
		SilencedBy []string `json:"silencedBy"`
	}{SilencedBy: []string{"x"}}}}, map[string]alertmanager.SilenceInfo{"x": {ID: "x", CreatedBy: "telegram:11111111"}}, nil)
	if s == nil || s.ByName != "Telegram" {
		t.Errorf("got %+v", s)
	}
}

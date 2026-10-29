package incident

import (
	"context"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/route"
)

const mediaChat = int64(-1001234567891)

// deliver routes m like the dispatcher does and handles it in that chat.
func (e *env) deliver(m *alertmanager.Message) []call {
	e.t.Helper()
	target, err := e.proc.Target(m)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.proc.Handle(context.Background(), target.ChatID, m); err != nil {
		e.t.Fatal(err)
	}
	return e.tg.take()
}

func mediaMsg(alerts ...string) *alertmanager.Message {
	m := msg(alerts...)
	m.CommonLabels = map[string]string{"alertname": "DiskAlmostFull", "team": "media"}
	return m
}

func TestRoutesToTopicAndStaysThere(t *testing.T) {
	e := newEnv(t)
	rules, err := route.Parse(`team="media" => -1001234567891/42`, []int64{chat, mediaChat})
	if err != nil {
		t.Fatal(err)
	}
	e.proc.Router.Rules = rules

	calls := e.deliver(mediaMsg("a"))
	expectCalls(t, calls, "sendMessage")
	if c := calls[0]; c.ChatID != mediaChat || c.ThreadID != 42 {
		t.Fatalf("sent to %d/%d", c.ChatID, c.ThreadID)
	}

	// The next notification no longer carries team="media" as a common
	// label (an alert from another team joined the group). The incident
	// stays in its chat and topic, and the reply goes there too.
	m := mediaMsg("a", "b")
	m.CommonLabels = map[string]string{"alertname": "DiskAlmostFull"}
	calls = e.deliver(m)
	expectCalls(t, calls, "editMessageText(#1)", "sendMessage(reply #1)")
	if c := calls[1]; c.ChatID != mediaChat || c.ThreadID != 42 {
		t.Errorf("reply sent to %d/%d", c.ChatID, c.ThreadID)
	}

	// Once resolved, a new incident of an unmatched group goes to the default.
	e.deliver(mediaMsg("a-", "b-"))
	m = msg("c")
	m.GroupKey = "other"
	if c := e.deliver(m)[0]; c.ChatID != chat || c.ThreadID != 0 {
		t.Errorf("default sent to %d/%d", c.ChatID, c.ThreadID)
	}
}

func TestQuietSeverities(t *testing.T) {
	e := newEnv(t)
	e.proc.Quiet = route.Quiet{Severities: []string{"warning"}, Location: time.UTC}

	// msg alerts have severity="warning".
	expectCalls(t, e.handle(msg("a")), "sendMessage silent")
	expectCalls(t, e.handle(msg("a", "b")), "editMessageText(#1)", "sendMessage(reply #1) silent")
	expectCalls(t, e.handle(msg("a", "b")), "sendMessage(reply #1) silent") // reminder

	m := msg("x")
	m.GroupKey = "critical"
	m.Alerts[0].Labels["severity"] = "critical"
	expectCalls(t, e.handle(m), "sendMessage")
}

func TestQuietHours(t *testing.T) {
	e := newEnv(t)
	w, _ := route.ParseWindow("22:00-07:00")
	e.proc.Quiet = route.Quiet{Hours: &w, Location: time.UTC}

	e.now = time.Date(2026, 3, 2, 23, 0, 0, 0, time.UTC)
	expectCalls(t, e.handle(msg("a")), "sendMessage silent")

	e.now = time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC)
	expectCalls(t, e.handle(msg("a", "b")), "editMessageText(#1)", "sendMessage(reply #1)")

	e.now = time.Date(2026, 3, 3, 23, 0, 0, 0, time.UTC)
	m := msg("x")
	m.GroupKey = "critical"
	m.Alerts[0].Labels["severity"] = "critical"
	expectCalls(t, e.handle(m), "sendMessage") // critical always rings
}

// A deleted topic (or a wrong ID in ROUTES) must not lose the notification:
// it goes to the chat without a topic, and the incident stays there.
func TestMissingTopicFallsBackToChat(t *testing.T) {
	e := newEnv(t)
	rules, _ := route.Parse(`team="media" => -1001234567891/42`, []int64{chat, mediaChat})
	e.proc.Router.Rules = rules
	e.tg.fail = func(c call, _ int) string {
		if c.ThreadID == 42 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`
		}
		return ""
	}
	calls := e.deliver(mediaMsg("a"))
	expectCalls(t, calls, "sendMessage", "sendMessage")
	if c := calls[1]; c.ChatID != mediaChat || c.ThreadID != 0 {
		t.Fatalf("fallback sent to %d/%d", c.ChatID, c.ThreadID)
	}
	inc, _ := e.store.Incident(mediaMsg().GroupKey)
	if inc == nil || inc.ThreadID != 0 || inc.ChatID != mediaChat {
		t.Fatalf("incident = %+v", inc)
	}
	calls = e.deliver(mediaMsg("a", "b"))
	expectCalls(t, calls, "editMessageText(#1)", "sendMessage(reply #1)")
	if calls[1].ThreadID != 0 {
		t.Errorf("reply into the missing topic again")
	}
}

package incident

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/komodo"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

type fakeRunner struct {
	mu       sync.Mutex
	started  []string
	startErr error
	success  bool
}

func (f *fakeRunner) Start(_ context.Context, a komodo.Action) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, a.ID)
	return "update-1", f.startErr
}

func (f *fakeRunner) Wait(context.Context, string) (bool, error) { return f.success, nil }

func remedyEnv(t *testing.T) (*buttonEnv, *fakeRunner) {
	e := newButtonEnv(t)
	actions, err := komodo.ParseActions([]byte(`{"actions":[
		{"id":"restart-lab","label":"Restart stack lab","operation":"RestartStack","stack":"lab","match":{"cluster":"lab"}},
		{"id":"restart-prod","label":"Restart stack prod","operation":"RestartStack","stack":"prod","match":{"cluster":"prod"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.proc.Buttons.Remedies = actions
	r := &fakeRunner{success: true}
	e.buttons.Komodo = r
	return e, r
}

// offer presses the incident's 🛠 button and returns the confirmation.
func (e *buttonEnv) offer(t *testing.T, kb *telegram.Keyboard) call {
	t.Helper()
	toast, calls := e.press(button(t, kb, "🛠 Restart stack lab"), chat, 1, admin)
	if toast != toastConfirm {
		t.Fatalf("toast %q", toast)
	}
	expectCalls(t, calls, "sendMessage silent for 11111111")
	return calls[0]
}

func TestRemedyConfirmAndRun(t *testing.T) {
	e, r := remedyEnv(t)
	kb := e.handle(gmsg("a"))[0].Keyboard // group labels: cluster=lab
	if got := strings.Join(row(kb, 2), ","); got != "🛠 Restart stack lab" {
		t.Fatalf("remedy row = %q (only the matching action)", got)
	}

	confirm := e.offer(t, kb)
	if !strings.Contains(confirm.Text, "Restart stack lab</b>?") || !strings.Contains(confirm.Text, "RestartStack on stack lab") {
		t.Errorf("confirmation = %q", confirm.Text)
	}
	if len(r.started) != 0 {
		t.Fatal("ran before the confirmation")
	}
	yes := button(t, confirm.Keyboard, "✅ Yes, Restart stack lab")

	// Nobody else can confirm: strangers are ignored, other admins get "expired".
	if toast, _ := e.press(yes, chat, 1, stranger); toast != "" {
		t.Errorf("stranger: %q", toast)
	}
	e.buttons.AdminUserIDs = append(e.buttons.AdminUserIDs, stranger)
	if toast, _ := e.press(yes, chat, 1, stranger); toast != toastExpired || len(r.started) != 0 {
		t.Errorf("other admin: %q, started %v", toast, r.started)
	}

	toast, calls := e.press(yes, chat, 1, admin)
	e.buttons.WaitRemedies()
	calls = append(calls, e.tg.take()...)
	if toast != toastStarted || strings.Join(r.started, ",") != "restart-lab" {
		t.Fatalf("toast %q, started %v", toast, r.started)
	}
	expectCalls(t, calls, "editEphemeralMessageText", "sendMessage(reply #1) silent", "editMessageText(#2)")
	if calls[1].Text != "🛠 Alice: Restart stack lab …" || !strings.HasPrefix(calls[2].Text, "✅ Alice: Restart stack lab, done after") {
		t.Errorf("reply %q, result %q", calls[1].Text, calls[2].Text)
	}

	// Single use.
	if toast, _ := e.press(yes, chat, 1, admin); toast != toastExpired {
		t.Errorf("second confirmation: %q", toast)
	}
	// Cooldown: a new confirmation within 10 minutes does not run.
	e.now = e.now.Add(5 * time.Minute)
	confirm = e.offer(t, kb)
	toast, _ = e.press(button(t, confirm.Keyboard, "✅ Yes, Restart stack lab"), chat, 1, admin)
	if !strings.HasPrefix(toast, "Ran 5m ago") || len(r.started) != 1 {
		t.Errorf("within cooldown: toast %q, started %v", toast, r.started)
	}
}

func TestRemedyCancelAndExpiry(t *testing.T) {
	e, r := remedyEnv(t)
	kb := e.handle(gmsg("a"))[0].Keyboard
	confirm := e.offer(t, kb)
	toast, calls := e.press(button(t, confirm.Keyboard, "Cancel"), chat, 1, admin)
	if toast != toastCancelled || len(r.started) != 0 {
		t.Errorf("cancel: %q, started %v", toast, r.started)
	}
	expectCalls(t, calls, "editEphemeralMessageText")
	// The "Yes" of a cancelled confirmation is gone too.
	if toast, _ := e.press(button(t, confirm.Keyboard, "✅ Yes, Restart stack lab"), chat, 1, admin); toast != toastExpired {
		t.Errorf("yes after cancel: %q", toast)
	}

	// A confirmation expires after a minute.
	confirm = e.offer(t, kb)
	e.now = e.now.Add(2 * time.Minute)
	if toast, _ := e.press(button(t, confirm.Keyboard, "✅ Yes, Restart stack lab"), chat, 1, admin); toast != toastExpired || len(r.started) != 0 {
		t.Errorf("expired: %q, started %v", toast, r.started)
	}

	// Not after the incident resolved.
	confirm = e.offer(t, kb)
	e.handle(gmsg("a-"))
	if toast, _ := e.press(button(t, confirm.Keyboard, "✅ Yes, Restart stack lab"), chat, 1, admin); toast != toastExpired || len(r.started) != 0 {
		t.Errorf("after resolve: %q, started %v", toast, r.started)
	}
}

func TestRemedyFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		runner *fakeRunner
		want   string
	}{
		"start error": {&fakeRunner{startErr: errors.New("komodo RestartStack: 403 Forbidden")}, "❌ Alice: Restart stack lab could not be started"},
		"failed":      {&fakeRunner{success: false}, "❌ Alice: Restart stack lab failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := remedyEnv(t)
			e.buttons.Komodo = tc.runner
			confirm := e.offer(t, e.handle(gmsg("a"))[0].Keyboard)
			e.press(button(t, confirm.Keyboard, "✅ Yes, Restart stack lab"), chat, 1, admin)
			e.buttons.WaitRemedies()
			calls := e.tg.take()
			if last := calls[len(calls)-1]; !strings.HasPrefix(last.Text, tc.want) {
				t.Errorf("result = %q", last.Text)
			}
		})
	}
}

func TestNoRemedyWithoutMatch(t *testing.T) {
	e, _ := remedyEnv(t)
	m := gmsg("a")
	m.GroupLabels = map[string]string{"alertname": "DiskAlmostFull", "cluster": "staging"}
	for _, r := range e.handle(m)[0].Keyboard.Rows {
		for _, b := range r {
			if strings.HasPrefix(b.Text, "🛠") {
				t.Errorf("remedy offered without a match: %q", b.Text)
			}
		}
	}
}

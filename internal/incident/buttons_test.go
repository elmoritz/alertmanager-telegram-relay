package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

const (
	admin    = int64(11111111)
	stranger = int64(22222222)
)

// fakeAlertmanager records silence API calls.
type fakeAlertmanager struct {
	mu       sync.Mutex
	created  []alertmanager.Silence
	expired  []string
	down     bool
	nextID   int
	notFound bool
}

func (f *fakeAlertmanager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		var s alertmanager.Silence
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &s)
		f.created = append(f.created, s)
		f.nextID++
		fmt.Fprintf(w, `{"silenceID":"00000000-0000-4000-8000-%012d"}`, f.nextID)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		f.expired = append(f.expired, strings.TrimPrefix(r.URL.Path, "/api/v2/silence/"))
		if f.notFound {
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type buttonEnv struct {
	*env
	am      *fakeAlertmanager
	buttons *Buttons
}

func newButtonEnv(t *testing.T) *buttonEnv {
	e := &buttonEnv{env: newEnv(t), am: &fakeAlertmanager{}}
	srv := httptest.NewServer(e.am)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)

	e.proc.Reminders = "off"
	e.proc.Buttons = ButtonConfig{Silences: true, Durations: []time.Duration{time.Hour, 24 * time.Hour}, ActionTTL: 7 * 24 * time.Hour}
	e.buttons = &Buttons{
		Processor:    e.proc,
		Telegram:     e.proc.Telegram.(*telegram.Client),
		Alertmanager: alertmanager.NewClient(u, "", "", ""),
		ChatIDs:      []int64{chat},
		AdminUserIDs: []int64{admin},
		Log:          e.proc.Log,
	}
	return e
}

// gmsg is msg with group labels, as Alertmanager sends them for group_by.
func gmsg(alerts ...string) *alertmanager.Message {
	m := msg(alerts...)
	m.GroupLabels = map[string]string{"alertname": "DiskAlmostFull", "cluster": "lab"}
	return m
}

// press simulates a button press and returns the toast and the other calls.
func (e *buttonEnv) press(data string, chatID int64, messageID int, user int64) (toast string, calls []call) {
	e.t.Helper()
	cq := &telegram.CallbackQuery{ID: "q", From: telegram.User{ID: user, FirstName: "Alice"}, Data: data}
	cq.Message = &struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{MessageID: messageID}
	cq.Message.Chat.ID = chatID
	e.buttons.HandleUpdate(context.Background(), telegram.Update{CallbackQuery: cq})
	for _, c := range e.tg.take() {
		if c.Method == "answerCallbackQuery" {
			toast = c.Text
		} else {
			calls = append(calls, c)
		}
	}
	return toast, calls
}

func row(kb *telegram.Keyboard, i int) []string {
	if kb == nil || len(kb.Rows) <= i {
		return nil
	}
	var out []string
	for _, b := range kb.Rows[i] {
		out = append(out, b.Text)
	}
	return out
}

func button(t *testing.T, kb *telegram.Keyboard, text string) string {
	t.Helper()
	if kb != nil {
		for _, r := range kb.Rows {
			for _, b := range r {
				if b.Text == text {
					return b.Data
				}
			}
		}
	}
	t.Fatalf("no %q button in %+v", text, kb)
	return ""
}

func TestKeyboardOnNewMessage(t *testing.T) {
	e := newButtonEnv(t)
	e.proc.Buttons.GrafanaURL = "https://grafana.example.com"
	m := gmsg("a")
	m.Alerts[0].Annotations["runbook_url"] = "https://example.com/runbooks/disk"
	m.Alerts[0].Annotations["dashboard_url"] = "javascript:alert(1)"
	calls := e.handle(m)

	kb := calls[0].Keyboard
	if got := strings.Join(row(kb, 0), ","); got != "🔕 1h,🔕 24h" {
		t.Errorf("silence row = %q", got)
	}
	if got := strings.Join(row(kb, 1), ","); got != "📈 Grafana,📖 Runbook" {
		t.Errorf("link row = %q", got)
	}
	if u := kb.Rows[1][0].URL; u != "https://grafana.example.com" {
		t.Errorf("javascript dashboard_url should fall back to GRAFANA_URL, got %q", u)
	}
	data := button(t, kb, "🔕 1h")
	if len(data) < 22 || strings.Contains(data, "DiskAlmostFull") || strings.Contains(data, "1h") {
		t.Errorf("callback data must be an opaque random ID, got %q", data)
	}
	a, _ := e.store.Action(data, e.now)
	if a == nil || a.Kind != store.ActionSilence || a.Duration != time.Hour || a.GroupKey != m.GroupKey {
		t.Errorf("action = %+v", a)
	}
}

func TestCopyCommandButton(t *testing.T) {
	e := newButtonEnv(t)
	m := gmsg("a")
	m.Alerts[0].Annotations["copy_command"] = "  journalctl -u node-exporter --since -1h  "
	kb := e.handle(m)[0].Keyboard
	last := kb.Rows[len(kb.Rows)-1]
	if len(last) != 1 || last[0].Text != "📋 Copy command" || last[0].CopyText != "journalctl -u node-exporter --since -1h" || last[0].Data != "" {
		t.Errorf("copy row = %+v", last)
	}

	// Too long: no button rather than a clipped, different command.
	m = gmsg("b")
	m.GroupKey = "other"
	m.Alerts[0].Annotations["copy_command"] = strings.Repeat("x", maxCopyText+1)
	for _, r := range e.handle(m)[0].Keyboard.Rows {
		for _, b := range r {
			if b.CopyText != "" {
				t.Errorf("copy button for an over-long command: %+v", b)
			}
		}
	}
}

func TestRejectedCopyButtonFallsBack(t *testing.T) {
	e := newButtonEnv(t)
	e.tg.fail = func(c call, _ int) string {
		if c.Keyboard == nil {
			return ""
		}
		for _, r := range c.Keyboard.Rows {
			for _, b := range r {
				if b.CopyText != "" {
					return `{"ok":false,"error_code":400,"description":"Bad Request: BUTTON_COPY_TEXT_INVALID"}`
				}
			}
		}
		return ""
	}
	m := gmsg("a")
	m.Alerts[0].Annotations["copy_command"] = "journalctl -u foo"
	calls := e.handle(m)
	expectCalls(t, calls, "sendMessage", "sendMessage")
	if got := strings.Join(row(calls[1].Keyboard, 0), ","); got != "🔕 1h,🔕 24h" || len(calls[1].Keyboard.Rows) != 1 {
		t.Errorf("fallback keyboard = %+v", calls[1].Keyboard)
	}
}

func TestNoSilenceButtonsWithoutGroupLabels(t *testing.T) {
	e := newButtonEnv(t)
	calls := e.handle(msg("a")) // no group labels
	if calls[0].Keyboard != nil {
		t.Errorf("keyboard = %+v, want none", calls[0].Keyboard)
	}
}

func TestNoSilenceButtonsWithoutAlertmanager(t *testing.T) {
	e := newButtonEnv(t)
	e.proc.Buttons.Silences = false
	if calls := e.handle(gmsg("a")); calls[0].Keyboard != nil {
		t.Errorf("keyboard = %+v, want none", calls[0].Keyboard)
	}
}

func TestUnauthorizedPressesAreIgnored(t *testing.T) {
	e := newButtonEnv(t)
	data := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")
	for name, p := range map[string]struct {
		chat int64
		user int64
	}{
		"stranger in allowed chat": {chat, stranger},
		"admin in other chat":      {-1009999999999, admin},
		"stranger elsewhere":       {-1009999999999, stranger},
	} {
		toast, calls := e.press(data, p.chat, 1, p.user)
		if toast != "" || len(calls) != 0 {
			t.Errorf("%s: toast %q, calls %v", name, toast, calls)
		}
	}
	if len(e.am.created) != 0 {
		t.Error("an unauthorised press created a silence")
	}
	if got := counterValue(e.proc.Metrics.UpdatesRejected); got != 3 {
		t.Errorf("rejected = %v", got)
	}
}

func TestForgedAndStalePresses(t *testing.T) {
	e := newButtonEnv(t)
	data := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")

	other := gmsg("x")
	other.GroupKey = `{}:{alertname="Other"}`
	e.handle(other) // message #2

	for name, p := range map[string]struct {
		data string
		msg  int
	}{
		"forged data":                  {"AAAAAAAAAAAAAAAAAAAAAA", 1},
		"label value as data":          {"DiskAlmostFull", 1},
		"real action, other message":   {data, 2},
		"real action, unknown message": {data, 99},
	} {
		if toast, calls := e.press(p.data, chat, p.msg, admin); toast != toastExpired || len(calls) != 0 {
			t.Errorf("%s: toast %q, calls %v", name, toast, calls)
		}
	}
	if len(e.am.created) != 0 {
		t.Error("a stale press created a silence")
	}
}

func TestSilenceAndUnsilence(t *testing.T) {
	e := newButtonEnv(t)
	silence := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")
	start := e.now

	toast, calls := e.press(silence, chat, 1, admin)
	if toast != "Silenced for 1h." {
		t.Errorf("toast = %q", toast)
	}
	if len(e.am.created) != 1 {
		t.Fatalf("created %d silences", len(e.am.created))
	}
	s := e.am.created[0]
	if fmt.Sprint(s.Matchers) != "[{alertname DiskAlmostFull false true} {cluster lab false true}]" {
		t.Errorf("matchers = %v", s.Matchers)
	}
	if !s.StartsAt.Equal(start) || !s.EndsAt.Equal(start.Add(time.Hour)) || s.CreatedBy != "telegram:11111111" || !strings.Contains(s.Comment, "alertmanager-telegram-relay") {
		t.Errorf("silence = %+v", s)
	}
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.Contains(calls[0].Text, "🔕 silenced until") || !strings.Contains(calls[0].Text, "by Alice") {
		t.Errorf("message = %q", calls[0].Text)
	}
	kb := calls[0].Keyboard
	if got := strings.Join(row(kb, 0), ","); !strings.HasPrefix(got, "🔕 Silenced until ") || !strings.HasSuffix(got, " by Alice") {
		t.Errorf("status row = %q", got)
	} else if st := kb.Rows[0][0]; !st.Disabled || st.Data != "" {
		t.Errorf("status button must be disabled and inert: %+v", st)
	}
	if got := strings.Join(row(kb, 1), ","); got != "↩️ Unsilence" || kb.Rows[1][0].Style != telegram.StylePrimary {
		t.Errorf("unsilence row = %q, style %q", got, kb.Rows[1][0].Style)
	}

	// The old silence buttons are gone: pressing one again does nothing.
	if toast, _ := e.press(silence, chat, 1, admin); toast != toastExpired || len(e.am.created) != 1 {
		t.Errorf("second silence press: %q, %d silences", toast, len(e.am.created))
	}

	unsilence := button(t, calls[0].Keyboard, "↩️ Unsilence")
	toast, calls = e.press(unsilence, chat, 1, admin)
	if toast != toastUnsilenced || len(e.am.expired) != 1 || e.am.expired[0] != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("toast %q, expired %v", toast, e.am.expired)
	}
	if got := strings.Join(row(calls[0].Keyboard, 0), ","); got != "🔕 1h,🔕 24h" || strings.Contains(calls[0].Text, "silenced") {
		t.Errorf("after unsilence: keyboard %q, text %q", got, calls[0].Text)
	}
	// Unsilence is single-use.
	if toast, _ := e.press(unsilence, chat, 1, admin); toast != toastExpired || len(e.am.expired) != 1 {
		t.Errorf("second unsilence press: %q, %v", toast, e.am.expired)
	}
}

func TestUnsilenceOfVanishedSilence(t *testing.T) {
	e := newButtonEnv(t)
	_, calls := e.press(button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 24h"), chat, 1, admin)
	e.am.notFound = true // expired in Alertmanager meanwhile
	toast, calls := e.press(button(t, calls[0].Keyboard, "↩️ Unsilence"), chat, 1, admin)
	if toast != toastUnsilenced || len(calls) != 1 {
		t.Errorf("toast %q, calls %v", toast, calls)
	}
}

func TestAlertmanagerDownChangesNothing(t *testing.T) {
	e := newButtonEnv(t)
	silence := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")
	e.am.down = true
	toast, calls := e.press(silence, chat, 1, admin)
	if toast != toastUnreachable || len(calls) != 0 {
		t.Errorf("toast %q, calls %v", toast, calls)
	}
	e.am.down = false
	if toast, _ := e.press(silence, chat, 1, admin); toast != "Silenced for 1h." {
		t.Errorf("button should still work after Alertmanager recovers: %q", toast)
	}
}

func TestResolveDisablesKeyboardAndDeletesActions(t *testing.T) {
	e := newButtonEnv(t)
	silence := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")
	calls := e.handle(gmsg("a-"))
	expectCalls(t, calls, "editMessageText(#1)")
	kb := calls[0].Keyboard
	if kb == nil || len(kb.Rows) != 1 || len(kb.Rows[0]) != 1 {
		t.Fatalf("resolve must leave one disabled button, got %+v", kb)
	}
	if b := kb.Rows[0][0]; !b.Disabled || b.Data != "" || b.URL != "" || !strings.HasPrefix(b.Text, "✅ Resolved") {
		t.Errorf("resolved button = %+v", b)
	}
	if a, _ := e.store.Action(silence, e.now); a != nil {
		t.Error("action survived the resolve")
	}
	if toast, _ := e.press(silence, chat, 1, admin); toast != toastExpired {
		t.Errorf("toast = %q", toast)
	}
}

func TestRejectedLinkButtonFallsBack(t *testing.T) {
	e := newButtonEnv(t)
	e.proc.Buttons.ShowSource = true
	e.tg.fail = func(c call, _ int) string {
		if c.Keyboard != nil && len(c.Keyboard.Rows) > 1 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: inline keyboard button URL 'http://prometheus:9090/graph' is invalid: Wrong HTTP URL"}`
		}
		return ""
	}
	m := gmsg("a")
	m.Alerts[0].GeneratorURL = "http://prometheus:9090/graph"
	calls := e.handle(m)
	expectCalls(t, calls, "sendMessage", "sendMessage")
	if got := strings.Join(row(calls[1].Keyboard, 0), ","); got != "🔕 1h,🔕 24h" || len(calls[1].Keyboard.Rows) != 1 {
		t.Errorf("fallback keyboard = %+v", calls[1].Keyboard)
	}
}

func TestButtonsRotateBeforeExpiry(t *testing.T) {
	e := newButtonEnv(t)
	old := button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h")

	e.now = e.now.Add(3 * 24 * time.Hour)
	if calls := e.handle(gmsg("a")); len(calls) != 0 {
		t.Errorf("buttons are fresh, nothing to edit: %v", calls)
	}
	e.now = e.now.Add(12 * time.Hour) // past half the TTL
	calls := e.handle(gmsg("a"))
	expectCalls(t, calls, "editMessageText(#1)")
	if fresh := button(t, calls[0].Keyboard, "🔕 1h"); fresh == old {
		t.Error("buttons not re-issued")
	}
	if toast, _ := e.press(old, chat, 1, admin); toast != toastExpired {
		t.Errorf("old button still works: %q", toast)
	}
}

func TestSilenceRunsOut(t *testing.T) {
	e := newButtonEnv(t)
	e.press(button(t, e.handle(gmsg("a"))[0].Keyboard, "🔕 1h"), chat, 1, admin)
	e.now = e.now.Add(2 * time.Hour)
	calls := e.handle(gmsg("a"))
	expectCalls(t, calls, "editMessageText(#1)")
	if got := strings.Join(row(calls[0].Keyboard, 0), ","); got != "🔕 1h,🔕 24h" || strings.Contains(calls[0].Text, "silenced") {
		t.Errorf("after the silence ran out: keyboard %q, text %q", got, calls[0].Text)
	}
}

func TestSilenceMatchersNeverEmpty(t *testing.T) {
	if ms := silenceMatchers(nil); len(ms) != 0 {
		t.Fatal(ms)
	}
	e := newButtonEnv(t)
	e.handle(gmsg("a"))
	// Corrupt the stored labels: the press must not create a matcher-less silence.
	inc, _ := e.store.Incident(gmsg("a").GroupKey)
	inc.GroupLabels = nil
	_ = e.store.PutIncident(inc)
	if toast, _ := e.press(inc.Buttons[0].ActionID, chat, 1, admin); toast != toastExpired || len(e.am.created) != 0 {
		t.Errorf("toast %q, created %v", toast, e.am.created)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "1h", 24 * time.Hour: "24h", 30 * time.Minute: "30m", 90 * time.Minute: "1h30m", 168 * time.Hour: "168h",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}

func TestConcurrentPressesCreateOneSilence(t *testing.T) {
	e := newButtonEnv(t)
	kb := e.handle(gmsg("a"))[0].Keyboard
	ids := []string{button(t, kb, "🔕 1h"), button(t, kb, "🔕 24h")}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			cq := &telegram.CallbackQuery{ID: "q", From: telegram.User{ID: admin}, Data: ids[i%2]}
			cq.Message = &struct {
				MessageID int `json:"message_id"`
				Chat      struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			}{MessageID: 1}
			cq.Message.Chat.ID = chat
			e.buttons.HandleUpdate(context.Background(), telegram.Update{CallbackQuery: cq})
		})
		wg.Go(func() { _ = e.proc.Handle(context.Background(), chat, gmsg("a")) })
	}
	wg.Wait()
	if len(e.am.created) != 1 {
		t.Errorf("created %d silences, want exactly 1", len(e.am.created))
	}
}

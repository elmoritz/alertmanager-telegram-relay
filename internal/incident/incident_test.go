package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/metrics"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const chat = int64(-1001234567890)

// call is one request the fake Bot API received.
type call struct {
	Method              string
	Text                string
	MessageID           int // editMessageText target
	ReplyTo             int
	DisableNotification bool
}

func (c call) String() string {
	s := c.Method
	switch {
	case c.MessageID != 0:
		s += fmt.Sprintf("(#%d)", c.MessageID)
	case c.ReplyTo != 0:
		s += fmt.Sprintf("(reply #%d)", c.ReplyTo)
	}
	if c.DisableNotification {
		s += " silent"
	}
	return s
}

// fakeTelegram is an httptest Bot API. fail, if set, can answer a call with
// an error response instead of success.
type fakeTelegram struct {
	mu     sync.Mutex
	calls  []call
	nextID int
	fail   func(c call, n int) string // n: index of this call
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p struct {
		Text                string `json:"text"`
		MessageID           int    `json:"message_id"`
		DisableNotification bool   `json:"disable_notification"`
		ReplyParameters     *struct {
			MessageID int `json:"message_id"`
		} `json:"reply_parameters"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &p)
	c := call{Method: r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], Text: p.Text, MessageID: p.MessageID, DisableNotification: p.DisableNotification}
	if p.ReplyParameters != nil {
		c.ReplyTo = p.ReplyParameters.MessageID
	}
	n := len(f.calls)
	f.calls = append(f.calls, c)
	if f.fail != nil {
		if resp := f.fail(c, n); resp != "" {
			var e struct {
				Code int `json:"error_code"`
			}
			_ = json.Unmarshal([]byte(resp), &e)
			w.WriteHeader(e.Code)
			io.WriteString(w, resp)
			return
		}
	}
	if c.Method == "sendMessage" {
		f.nextID++
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, f.nextID)
		return
	}
	io.WriteString(w, `{"ok":true,"result":true}`)
}

func (f *fakeTelegram) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

type env struct {
	t     *testing.T
	tg    *fakeTelegram
	store *store.Store
	proc  *Processor
	now   time.Time
	slept []time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tg: &fakeTelegram{}, now: time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(e.tg)
	t.Cleanup(srv.Close)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st

	r, err := render.New("", time.UTC, func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	e.proc = &Processor{
		Store:     st,
		Telegram:  telegram.New(srv.URL, "123:TEST-TOKEN-not-real"),
		Render:    r,
		Metrics:   metrics.New(func() float64 { return 0 }, func() float64 { return 0 }),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reminders: config.RemindersReply,
		RetryFor:  10 * time.Minute,
		Now:       func() time.Time { return e.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			e.slept = append(e.slept, d)
			e.now = e.now.Add(d)
			return nil
		},
	}
	return e
}

func alert(fp, status string) alertmanager.Alert {
	a := alertmanager.Alert{
		Status:      status,
		Labels:      map[string]string{"alertname": "DiskAlmostFull", "instance": "server-" + fp, "severity": "warning"},
		Annotations: map[string]string{"summary": "disk on server-" + fp + " almost full"},
		StartsAt:    time.Date(2026, 3, 2, 4, 0, 0, 0, time.UTC),
		Fingerprint: fp,
	}
	if status == alertmanager.StatusResolved {
		a.EndsAt = time.Date(2026, 3, 2, 6, 0, 0, 0, time.UTC)
	}
	return a
}

// msg builds a notification; each argument is "<fingerprint>" (firing) or
// "<fingerprint>-" (resolved).
func msg(alerts ...string) *alertmanager.Message {
	m := &alertmanager.Message{
		Version: "4", GroupKey: `{}:{alertname="DiskAlmostFull"}`, Status: alertmanager.StatusResolved,
		CommonLabels: map[string]string{"alertname": "DiskAlmostFull"},
	}
	for _, s := range alerts {
		if fp, ok := strings.CutSuffix(s, "-"); ok {
			m.Alerts = append(m.Alerts, alert(fp, alertmanager.StatusResolved))
		} else {
			m.Alerts = append(m.Alerts, alert(s, alertmanager.StatusFiring))
			m.Status = alertmanager.StatusFiring
		}
	}
	return m
}

func (e *env) handle(m *alertmanager.Message) []call {
	e.t.Helper()
	if err := e.proc.Handle(context.Background(), chat, m); err != nil {
		e.t.Fatal(err)
	}
	return e.tg.take()
}

func expectCalls(t *testing.T, got []call, want ...string) {
	t.Helper()
	var s []string
	for _, c := range got {
		s = append(s, c.String())
	}
	if strings.Join(s, ", ") != strings.Join(want, ", ") {
		t.Errorf("calls = [%s], want [%s]", strings.Join(s, ", "), strings.Join(want, ", "))
	}
}

func TestDecide(t *testing.T) {
	open := &store.Incident{Firing: []string{"a", "b"}}
	for _, tc := range []struct {
		name  string
		inc   *store.Incident
		m     *alertmanager.Message
		want  Kind
		added int
	}{
		{"new group", nil, msg("a"), Open, 0},
		{"same alerts", open, msg("b", "a"), Repeat, 0},
		{"alert added", open, msg("a", "b", "c"), Changed, 1},
		{"some resolved", open, msg("a", "b-"), Changed, 0},
		{"one swapped", open, msg("a", "c", "b-"), Changed, 1},
		{"all resolved", open, msg("a-", "b-"), Resolve, 0},
		{"resolve, nothing open", nil, msg("a-"), ResolveUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Decide(tc.inc, tc.m)
			if p.Kind != tc.want || len(p.Added) != tc.added {
				t.Errorf("got %v with %d added, want %v with %d", p.Kind, len(p.Added), tc.want, tc.added)
			}
		})
	}
}

func TestAlertKeyWithoutFingerprint(t *testing.T) {
	a, b := alert("", "firing"), alert("", "firing")
	b.Labels = map[string]string{"severity": "warning", "instance": "server-", "alertname": "DiskAlmostFull"}
	if alertKey(a) != alertKey(b) {
		t.Error("key must not depend on map order")
	}
	b.Labels["instance"] = "other"
	if alertKey(a) == alertKey(b) {
		t.Error("different labels, same key")
	}
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)

	calls := e.handle(msg("a"))
	expectCalls(t, calls, "sendMessage")
	if calls[0].DisableNotification {
		t.Error("a new incident must notify")
	}
	inc, _ := e.store.Incident(msg("a").GroupKey)
	if inc == nil || inc.MessageID != 1 {
		t.Fatalf("incident = %+v", inc)
	}

	e.now = e.now.Add(4 * time.Hour)
	calls = e.handle(msg("a"))
	expectCalls(t, calls, "sendMessage(reply #1)")
	if !strings.Contains(calls[0].Text, "still firing, 4h") {
		t.Errorf("reminder = %q", calls[0].Text)
	}

	calls = e.handle(msg("a", "b"))
	expectCalls(t, calls, "editMessageText(#1)", "sendMessage(reply #1)")
	if !strings.Contains(calls[1].Text, "disk on server-b") {
		t.Errorf("added reply = %q", calls[1].Text)
	}

	calls = e.handle(msg("a", "b-"))
	expectCalls(t, calls, "editMessageText(#1)")

	calls = e.handle(msg("a-", "b-"))
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.HasPrefix(calls[0].Text, "✅") {
		t.Errorf("resolved text = %q", calls[0].Text)
	}
	if inc, _ := e.store.Incident(msg("a").GroupKey); inc != nil {
		t.Error("incident not closed")
	}

	// Firing again after closing is a new incident with a new message
	// (#2 and #3 were the replies).
	expectCalls(t, e.handle(msg("a")), "sendMessage")
	if inc, _ := e.store.Incident(msg("a").GroupKey); inc == nil || inc.MessageID != 4 {
		t.Errorf("reopened incident = %+v", inc)
	}
}

func TestReminderModes(t *testing.T) {
	for mode, want := range map[string][]string{
		config.RemindersReply:  {"sendMessage(reply #1)"},
		config.RemindersSilent: {"sendMessage(reply #1) silent"},
		config.RemindersOff:    nil,
	} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			e.proc.Reminders = mode
			e.handle(msg("a"))
			expectCalls(t, e.handle(msg("a")), want...)
		})
	}
}

func TestRepeatWithChangedTextEdits(t *testing.T) {
	e := newEnv(t)
	e.proc.Reminders = config.RemindersOff
	e.handle(msg("a"))
	m := msg("a")
	m.Alerts[0].Annotations["summary"] = "disk on server-a now 99% full"
	expectCalls(t, e.handle(m), "editMessageText(#1)")
}

func TestResolveReplyAndUnknownResolve(t *testing.T) {
	e := newEnv(t)
	e.proc.ResolveReply = true
	e.handle(msg("a"))
	expectCalls(t, e.handle(msg("a-")), "editMessageText(#1)", "sendMessage(reply #1) silent")

	expectCalls(t, e.handle(msg("a-")), "sendMessage silent")
}

func TestEditFallsBackToNewMessage(t *testing.T) {
	e := newEnv(t)
	e.handle(msg("a"))
	e.tg.fail = func(c call, _ int) string {
		if c.Method == "editMessageText" {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
		}
		return ""
	}
	expectCalls(t, e.handle(msg("a", "b")), "editMessageText(#1)", "sendMessage", "sendMessage(reply #2)")
	if inc, _ := e.store.Incident(msg("a").GroupKey); inc.MessageID != 2 {
		t.Errorf("incident should point at the new message: %+v", inc)
	}
}

func TestRateLimitIsHonoured(t *testing.T) {
	e := newEnv(t)
	e.tg.fail = func(_ call, n int) string {
		if n < 2 {
			return `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 30","parameters":{"retry_after":30}}`
		}
		return ""
	}
	expectCalls(t, e.handle(msg("a")), "sendMessage", "sendMessage", "sendMessage")
	if len(e.slept) != 2 || e.slept[0] != 30*time.Second || e.slept[1] != 30*time.Second {
		t.Errorf("slept %v, want 2×30s", e.slept)
	}
	if inc, _ := e.store.Incident(msg("a").GroupKey); inc == nil {
		t.Error("incident not stored after successful retry")
	}
}

func TestGivesUpAfterRetryWindow(t *testing.T) {
	e := newEnv(t)
	e.tg.fail = func(call, int) string { return `{"ok":false,"error_code":502,"description":"Bad Gateway"}` }
	e.handle(msg("a"))

	var total time.Duration
	for _, d := range e.slept {
		total += d
	}
	if total > 10*time.Minute || total < 5*time.Minute {
		t.Errorf("retried for %v, want close to 10m", total)
	}
	if got := counterValue(e.proc.Metrics.DeliveriesDropped); got != 1 {
		t.Errorf("dropped = %v", got)
	}
	if inc, _ := e.store.Incident(msg("a").GroupKey); inc != nil {
		t.Error("no incident should be stored for an undelivered message")
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.tg.fail = func(call, int) string {
		return `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked from the supergroup chat"}`
	}
	expectCalls(t, e.handle(msg("a")), "sendMessage")
	if len(e.slept) != 0 {
		t.Errorf("slept %v", e.slept)
	}
}

func TestFormatErrorFallsBackToMinimal(t *testing.T) {
	e := newEnv(t)
	e.tg.fail = func(_ call, n int) string {
		if n == 0 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unsupported start tag"}`
		}
		return ""
	}
	calls := e.handle(msg("a"))
	expectCalls(t, calls, "sendMessage", "sendMessage")
	if !strings.Contains(calls[1].Text, "details too long") {
		t.Errorf("second attempt should be the minimal message: %q", calls[1].Text)
	}
}

func TestCancelledKeepsJobQueued(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.tg.fail = func(call, int) string { return `{"ok":false,"error_code":502,"description":"Bad Gateway"}` }
	e.proc.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := e.proc.Handle(ctx, chat, msg("a")); err == nil {
		t.Error("cancelled handling must report an error so the job stays queued")
	}
	if got := counterValue(e.proc.Metrics.DeliveriesDropped); got != 0 {
		t.Errorf("dropped = %v", got)
	}
}

func TestDispatcherDeliversInOrderAndReplaysAfterRestart(t *testing.T) {
	e := newEnv(t)
	d := &Dispatcher{Store: e.store, Processor: e.proc, Log: e.proc.Log, Route: func(*alertmanager.Message) int64 { return chat }}

	// Queued before Start, as if left over from a previous run.
	for _, m := range []*alertmanager.Message{msg("a"), msg("a", "b"), msg("a-", "b-")} {
		b, _ := json.Marshal(m)
		if err := e.store.Enqueue(chat, b); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(msg("a")); err != nil {
		t.Fatal(err)
	}
	drainCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	d.Drain(drainCtx)
	cancel()
	d.Wait()

	expectCalls(t, e.tg.take(), "sendMessage", "editMessageText(#1)", "sendMessage(reply #1)", "editMessageText(#1)", "sendMessage")
	if n, _ := e.store.QueueLen(); n != 0 {
		t.Errorf("queue not drained: %d", n)
	}
}

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		panic(err)
	}
	return m.GetCounter().GetValue()
}

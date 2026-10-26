package main

import (
	"bytes"
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
)

const (
	testToken  = "123:TEST-TOKEN-not-real"
	testSecret = "test-webhook-secret"
	testChat   = "-1001234567890"
)

// fakeBotAPI records method names and texts.
type fakeBotAPI struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var p struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	f.mu.Lock()
	f.calls = append(f.calls, method+": "+p.Text)
	f.mu.Unlock()
	switch method {
	case "getMe":
		io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"example_bot"}}`)
	case "sendMessage":
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	default:
		io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func (f *fakeBotAPI) waitFor(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for i, c := range f.calls {
			if strings.HasPrefix(c, prefix) {
				f.calls = f.calls[i+1:]
				f.mu.Unlock()
				return c
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q call to the Bot API", prefix)
	return ""
}

// syncBuffer collects log output from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func payload(status string) string {
	ends := "0001-01-01T00:00:00Z"
	if status == "resolved" {
		ends = "2026-03-02T06:00:00Z"
	}
	return fmt.Sprintf(`{"version":"4","groupKey":"{}:{alertname=\"HostDown\"}","status":%q,"receiver":"telegram",
"groupLabels":{"alertname":"HostDown"},"commonLabels":{"alertname":"HostDown","instance":"server-1","severity":"critical"},
"commonAnnotations":{"summary":"server-1 is down"},"externalURL":"http://alertmanager:9093",
"alerts":[{"status":%q,"labels":{"alertname":"HostDown","instance":"server-1","severity":"critical"},
"annotations":{"summary":"server-1 is down"},"startsAt":"2026-03-02T04:56:00Z","endsAt":%q,"fingerprint":"a1"}]}`, status, status, ends)
}

func TestEndToEnd(t *testing.T) {
	bot := &fakeBotAPI{}
	tgSrv := httptest.NewServer(bot)
	defer tgSrv.Close()

	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_IDS", testChat)
	t.Setenv("TELEGRAM_ADMIN_USER_IDS", "11111111")
	t.Setenv("WEBHOOK_SECRET", testSecret)
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("STATE_DIR", t.TempDir())

	logs := &syncBuffer{}
	level := new(slog.LevelVar)
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: level}))
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, log, level, tgSrv.URL, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run: %v", err)
	}
	base := "http://" + addr

	post := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/webhook", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testSecret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("webhook: %s", resp.Status)
		}
	}

	post(payload("firing"))
	if c := bot.waitFor(t, "sendMessage"); !strings.Contains(c, "🔥 <b>HostDown</b> · server-1") {
		t.Errorf("sent %q", c)
	}
	post(payload("resolved"))
	if c := bot.waitFor(t, "editMessageText"); !strings.Contains(c, "✅ <b>HostDown</b> · server-1 resolved") {
		t.Errorf("edited to %q", c)
	}

	for path, want := range map[string]string{
		"/healthz": "ok",
		"/metrics": `relay_notifications_received_total{status="resolved"} 1`,
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %s %q", path, resp.Status, b)
		}
		for _, leak := range []string{testToken, testSecret, testChat, "HostDown", "server-1"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s exposes %q", path, leak)
			}
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no shutdown")
	}

	out := logs.String()
	if !strings.Contains(out, "incident resolved") {
		t.Errorf("expected lifecycle logs, got:\n%s", out)
	}
	for _, leak := range []string{testToken, "TEST-TOKEN", testSecret, testChat, "HostDown", "server-1"} {
		if strings.Contains(out, leak) {
			t.Errorf("logs contain %q:\n%s", leak, out)
		}
	}
}

func TestRefusesToStartWithoutAllowlists(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_IDS", testChat)
	t.Setenv("TELEGRAM_ADMIN_USER_IDS", "")
	t.Setenv("WEBHOOK_SECRET", testSecret)
	t.Setenv("STATE_DIR", t.TempDir())
	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), new(slog.LevelVar), "http://127.0.0.1:1", nil)
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_ADMIN_USER_IDS") {
		t.Errorf("err = %v", err)
	}
}

func TestRefusesRejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer srv.Close()
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_IDS", testChat)
	t.Setenv("TELEGRAM_ADMIN_USER_IDS", "11111111")
	t.Setenv("WEBHOOK_SECRET", testSecret)
	t.Setenv("STATE_DIR", t.TempDir())
	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), new(slog.LevelVar), srv.URL, nil)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v", err)
	}
}

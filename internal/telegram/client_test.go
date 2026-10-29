package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Deliberately not shaped like a real token (SECURITY.md §7 scans for those),
// but containing characters that change under URL escaping.
const testToken = "123:TEST/token+not real"

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		for _, form := range []string{"%v", "%+v", "%#v", "%s"} {
			s := fmt.Sprintf(form, e)
			for _, tok := range []string{testToken, url.PathEscape(testToken), url.QueryEscape(testToken), "TEST/token", "TEST%2Ftoken"} {
				if strings.Contains(s, tok) {
					t.Fatalf("error leaks the token (%s): %s", form, s)
				}
			}
		}
	}
}

func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused from now on

	c := New(base, testToken)
	_, err := c.SendMessage(context.Background(), Message{ChatID: -1001234567890, Text: "x"})
	assertNoToken(t, err)
	if !IsRetryable(err) {
		t.Errorf("network error should be retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("cause should survive redaction: %q", err)
	}
}

func TestTimeoutDoesNotLeakToken(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)

	c := New(srv.URL, testToken)
	c.http.Timeout = 50 * time.Millisecond
	err := c.EditMessageText(context.Background(), 1, 2, "x", nil)
	assertNoToken(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.GetMe(ctx)
	assertNoToken(t, err)
	if !errors.Is(err, context.Canceled) || IsRetryable(err) {
		t.Errorf("canceled: err = %v, retryable = %v", err, IsRetryable(err))
	}
}

func TestAPIErrorDescriptionIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"ok":false,"error_code":401,"description":"Unauthorized for %s"}`, testToken)
	}))
	defer srv.Close()

	_, err := New(srv.URL, testToken).GetMe(context.Background())
	assertNoToken(t, err)
	if IsRetryable(err) {
		t.Error("401 should not be retryable")
	}
}

// Telegram quotes rejected button URLs, which come from alert annotations.
// Hand-written from the shape of a real answer.
func TestAPIErrorDescriptionDropsQuotedValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: inline keyboard button URL 'http://grafana:3000/d/abc?var-host=server-1' is invalid: Wrong HTTP URL"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, testToken).SendMessage(context.Background(), Message{ChatID: 1, Text: "x"})
	if strings.Contains(err.Error(), "grafana") || strings.Contains(err.Error(), "server-1") {
		t.Errorf("error contains the URL: %v", err)
	}
	if !IsButtonError(err) {
		t.Errorf("IsButtonError = false for %v", err)
	}
}

// fake is a minimal Bot API: it records requests and answers with the
// next queued response.
type fake struct {
	t         *testing.T
	requests  []map[string]any
	paths     []string
	responses []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.Header.Get("Content-Type"))
	}
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		f.t.Errorf("request body: %v", err)
	}
	f.requests = append(f.requests, m)
	f.paths = append(f.paths, r.URL.Path)
	resp := `{"ok":true,"result":true}`
	if len(f.responses) > 0 {
		resp, f.responses = f.responses[0], f.responses[1:]
	}
	var status struct {
		ErrorCode int `json:"error_code"`
	}
	_ = json.Unmarshal([]byte(resp), &status)
	if status.ErrorCode != 0 {
		w.WriteHeader(status.ErrorCode)
	}
	io.WriteString(w, resp)
}

func TestSendMessage(t *testing.T) {
	f := &fake{t: t, responses: []string{`{"ok":true,"result":{"message_id":42}}`}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	var calls []string
	c := New(srv.URL, testToken)
	c.OnCall = func(method, result string) { calls = append(calls, method+"="+result) }

	id, err := c.SendMessage(context.Background(), Message{ChatID: -1001234567890, Text: "<b>hi</b>", ReplyTo: 7, Silent: true})
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if f.paths[0] != "/bot"+testToken+"/sendMessage" {
		t.Errorf("path = %q", f.paths[0])
	}
	req := f.requests[0]
	if req["parse_mode"] != "HTML" || req["disable_notification"] != true || req["chat_id"] != float64(-1001234567890) {
		t.Errorf("request = %v", req)
	}
	rp, _ := req["reply_parameters"].(map[string]any)
	if rp["message_id"] != float64(7) || rp["allow_sending_without_reply"] != true {
		t.Errorf("reply_parameters = %v", req["reply_parameters"])
	}
	if lp, _ := req["link_preview_options"].(map[string]any); lp["is_disabled"] != true {
		t.Errorf("link previews not disabled: %v", req)
	}
	if strings.Join(calls, ",") != "sendMessage=ok" {
		t.Errorf("OnCall = %v", calls)
	}
}

func TestEditMessageText(t *testing.T) {
	f := &fake{t: t, responses: []string{
		`{"ok":true,"result":{"message_id":42}}`,
		`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`,
		`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`,
		`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unsupported start tag"}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)
	ctx := context.Background()

	if err := c.EditMessageText(ctx, 1, 42, "a", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a", nil); err != nil {
		t.Errorf("not modified should be success: %v", err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a", nil); !IsMessageGone(err) || IsRetryable(err) {
		t.Errorf("want message gone, got %v", err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a", nil); !IsFormatError(err) {
		t.Errorf("want format error, got %v", err)
	}
	if f.requests[0]["message_id"] != float64(42) || f.requests[0]["text"] != "a" {
		t.Errorf("request = %v", f.requests[0])
	}
}

func TestRateLimitAndServerErrors(t *testing.T) {
	f := &fake{t: t, responses: []string{
		`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`,
		`{"ok":false,"error_code":502,"description":"Bad Gateway"}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)

	_, err := c.SendMessage(context.Background(), Message{ChatID: 1, Text: "a"})
	if !IsRetryable(err) || RetryAfter(err) != 7*time.Second {
		t.Errorf("429: retryable=%v after=%v", IsRetryable(err), RetryAfter(err))
	}
	_, err = c.SendMessage(context.Background(), Message{ChatID: 1, Text: "a"})
	if !IsRetryable(err) || RetryAfter(err) != 0 {
		t.Errorf("502: retryable=%v", IsRetryable(err))
	}
}

func TestNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html>bad gateway "+r.URL.Path+"</html>")
	}))
	defer srv.Close()
	_, err := New(srv.URL, testToken).SendMessage(context.Background(), Message{ChatID: 1, Text: "a"})
	assertNoToken(t, err)
	if !IsRetryable(err) {
		t.Errorf("502 page should be retryable: %v", err)
	}
}

func TestThreadID(t *testing.T) {
	f := &fake{t: t, responses: []string{`{"ok":true,"result":{"message_id":1}}`, `{"ok":true,"result":{"message_id":2}}`}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)
	for _, thread := range []int{0, 42} {
		if _, err := c.SendMessage(context.Background(), Message{ChatID: 1, ThreadID: thread, Text: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, set := f.requests[0]["message_thread_id"]; set {
		t.Errorf("message_thread_id sent without a topic: %v", f.requests[0])
	}
	if f.requests[1]["message_thread_id"] != float64(42) {
		t.Errorf("message_thread_id missing: %v", f.requests[1])
	}
}

func TestProtectContent(t *testing.T) {
	f := &fake{t: t, responses: []string{`{"ok":true,"result":{"message_id":1}}`, `{"ok":true,"result":{"message_id":2}}`}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)
	if _, err := c.SendMessage(context.Background(), Message{ChatID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	c.ProtectContent = true
	if _, err := c.SendMessage(context.Background(), Message{ChatID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, set := f.requests[0]["protect_content"]; set {
		t.Errorf("protect_content sent by default: %v", f.requests[0])
	}
	if f.requests[1]["protect_content"] != true {
		t.Errorf("protect_content missing: %v", f.requests[1])
	}
}

// Wire formats checked against the live API (docs/LIVE-TESTING.md).
func TestButtonJSON(t *testing.T) {
	for _, tc := range []struct {
		b    Button
		want string
	}{
		{Button{Text: "🔕 1h", Data: "abc"}, `{"text":"🔕 1h","callback_data":"abc"}`},
		{Button{Text: "↩️", Data: "abc", Style: StyleSuccess}, `{"text":"↩️","style":"success","callback_data":"abc"}`},
		{Button{Text: "📖", URL: "https://example.com/rb"}, `{"text":"📖","url":"https://example.com/rb"}`},
		{Button{Text: "📋", CopyText: "journalctl -u foo"}, `{"text":"📋","copy_text":{"text":"journalctl -u foo"}}`},
		{Button{Text: "✅ Resolved", Disabled: true}, `{"text":"✅ Resolved","disabled":{}}`},
		// Telegram would drop "disabled" and keep the action: never send both.
		{Button{Text: "x", Data: "abc", URL: "https://example.com", CopyText: "c", Disabled: true}, `{"text":"x","disabled":{}}`},
	} {
		got, err := json.Marshal(tc.b)
		if err != nil || string(got) != tc.want {
			t.Errorf("Marshal(%+v) = %s, %v; want %s", tc.b, got, err, tc.want)
			continue
		}
		var back Button
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		if again, _ := json.Marshal(back); string(again) != tc.want {
			t.Errorf("round trip of %s gave %s", tc.want, again)
		}
	}
}

func TestButtonErrors(t *testing.T) {
	for _, desc := range []string{
		"Bad Request: BUTTON_COPY_TEXT_INVALID",
		"Bad Request: can't parse InlineKeyboardButton: Invalid button style specified",
		"Bad Request: BUTTON_DATA_INVALID",
	} {
		err := &Error{Method: "sendMessage", Code: 400, Description: desc}
		if !IsButtonError(err) || IsFormatError(err) {
			t.Errorf("%q: button error %v, format error %v", desc, IsButtonError(err), IsFormatError(err))
		}
	}
}

func TestKeyboards(t *testing.T) {
	f := &fake{t: t, responses: []string{
		`{"ok":true,"result":{"message_id":42}}`,
		`{"ok":true,"result":true}`,
		`{"ok":true,"result":true}`,
		`{"ok":false,"error_code":400,"description":"Bad Request: inline keyboard button URL 'http://grafana:3000' is invalid: Wrong HTTP URL"}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)
	ctx := context.Background()
	kb := &Keyboard{Rows: [][]Button{{{Text: "🔕 1h", Data: "abc"}}, {{Text: "📖 Runbook", URL: "https://example.com/rb"}}}}

	if _, err := c.SendMessage(ctx, Message{ChatID: 1, Text: "a", Keyboard: kb}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "b", kb); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "c", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "d", kb); !IsButtonError(err) || IsFormatError(err) {
		t.Errorf("want button error, got %v", err)
	}

	rows := func(i int) []any {
		rm, _ := f.requests[i]["reply_markup"].(map[string]any)
		r, ok := rm["inline_keyboard"].([]any)
		if !ok {
			t.Fatalf("request %d: no inline_keyboard: %v", i, f.requests[i])
		}
		return r
	}
	if len(rows(0)) != 2 || len(rows(1)) != 2 {
		t.Errorf("keyboard not sent: %v", f.requests[0])
	}
	if len(rows(2)) != 0 {
		t.Errorf("nil keyboard must remove the keyboard explicitly: %v", f.requests[2])
	}
	btn := rows(0)[0].([]any)[0].(map[string]any)
	if btn["callback_data"] != "abc" || btn["url"] != nil {
		t.Errorf("callback button = %v", btn)
	}
}

func TestPolling(t *testing.T) {
	f := &fake{t: t, responses: []string{
		`{"ok":true,"result":[{"update_id":7,"callback_query":{"id":"q1","from":{"id":11111111,"first_name":"Alice"},"message":{"message_id":42,"chat":{"id":-1001234567890},"date":1},"data":"abc"}},{"update_id":8,"message":{"text":"hi"}}]}`,
		`{"ok":true,"result":true}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL, testToken)

	ups, err := c.GetUpdates(context.Background(), 7, 25*time.Second)
	if err != nil || len(ups) != 2 {
		t.Fatalf("%v %v", ups, err)
	}
	cq := ups[0].CallbackQuery
	if cq == nil || cq.ID != "q1" || cq.From.ID != 11111111 || cq.From.FirstName != "Alice" || cq.Message.MessageID != 42 || cq.Message.Chat.ID != -1001234567890 || cq.Data != "abc" {
		t.Errorf("callback = %+v", cq)
	}
	if ups[1].CallbackQuery != nil {
		t.Error("message update parsed as callback")
	}
	req := f.requests[0]
	if req["offset"] != float64(7) || req["timeout"] != float64(25) || fmt.Sprint(req["allowed_updates"]) != "[callback_query]" {
		t.Errorf("getUpdates request = %v", req)
	}

	if err := c.AnswerCallbackQuery(context.Background(), "q1", strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	if text, _ := f.requests[1]["text"].(string); len(text) != 200 || f.requests[1]["callback_query_id"] != "q1" {
		t.Errorf("answer = %v", f.requests[1])
	}
	for _, p := range f.paths {
		if strings.Contains(strings.ToLower(p), "webhook") {
			t.Errorf("webhook method called: %s", p)
		}
	}
}

func TestThreadGone(t *testing.T) {
	err := &Error{Method: "sendMessage", Code: 400, Description: "Bad Request: message thread not found"}
	if !IsThreadGone(err) || IsFormatError(err) || IsButtonError(err) || IsMessageGone(err) || IsRetryable(err) {
		t.Errorf("classification of %v", err)
	}
}

// Shape of a real answer, IDs replaced.
func TestChatMigrated(t *testing.T) {
	f := &fake{t: t, responses: []string{`{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1001234567890}}`}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := New(srv.URL, testToken).SendMessage(context.Background(), Message{ChatID: -123456789, Text: "a"})
	if to := MigratedTo(err); to != -1001234567890 || IsRetryable(err) || IsFormatError(err) || IsButtonError(err) {
		t.Errorf("MigratedTo = %d for %v", to, err)
	}
	if MigratedTo(&Error{Code: 400, Description: "x"}) != 0 {
		t.Error("MigratedTo of another error")
	}
}

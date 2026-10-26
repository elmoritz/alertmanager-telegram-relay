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
	err := c.EditMessageText(context.Background(), 1, 2, "x")
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

	if err := c.EditMessageText(ctx, 1, 42, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a"); err != nil {
		t.Errorf("not modified should be success: %v", err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a"); !IsMessageGone(err) || IsRetryable(err) {
		t.Errorf("want message gone, got %v", err)
	}
	if err := c.EditMessageText(ctx, 1, 42, "a"); !IsFormatError(err) {
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

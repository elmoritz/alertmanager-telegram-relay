package webhook

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/metrics"
)

const secret = "test-webhook-secret"

const validBody = `{"version":"4","groupKey":"{}:{alertname=\"HostDown\"}","status":"firing","receiver":"telegram",
"groupLabels":{"alertname":"HostDown"},"commonLabels":{"alertname":"HostDown"},"commonAnnotations":{},
"externalURL":"http://alertmanager:9093","unknownField":{"nested":true},
"alerts":[{"status":"firing","labels":{"alertname":"HostDown"},"annotations":{},"startsAt":"2026-03-02T04:56:00Z","endsAt":"0001-01-01T00:00:00Z","fingerprint":"a1"}]}`

type queue struct {
	got []*alertmanager.Message
	err error
}

func (q *queue) Enqueue(m *alertmanager.Message) error {
	if q.err != nil {
		return q.err
	}
	q.got = append(q.got, m)
	return nil
}

func do(t *testing.T, q *queue, method, auth, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := New(secret, q, metrics.New(func() float64 { return 0 }, func() float64 { return 0 }), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest(method, "/webhook", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebhook(t *testing.T) {
	bearer := "Bearer " + secret
	for _, tc := range []struct {
		name, method, auth, ct, body string
		want                         int
	}{
		{"valid", "POST", bearer, "application/json", validBody, 200},
		{"valid, charset and lowercase scheme", "POST", "bearer " + secret, "application/json; charset=utf-8", validBody, 200},
		{"GET", "GET", bearer, "application/json", "", 405},
		{"no auth", "POST", "", "application/json", validBody, 401},
		{"wrong secret", "POST", "Bearer nope", "application/json", validBody, 401},
		{"secret prefix", "POST", "Bearer " + secret[:5], "application/json", validBody, 401},
		{"basic auth", "POST", "Basic " + secret, "application/json", validBody, 401},
		{"wrong content type", "POST", bearer, "text/plain", validBody, 415},
		{"no content type", "POST", bearer, "", validBody, 415},
		{"broken json", "POST", bearer, "application/json", `{"version":`, 400},
		{"wrong version", "POST", bearer, "application/json", strings.Replace(validBody, `"version":"4"`, `"version":"3"`, 1), 400},
		{"no alerts", "POST", bearer, "application/json", `{"version":"4","groupKey":"x","status":"firing","alerts":[]}`, 400},
		{"too large", "POST", bearer, "application/json", `{"version":"4","x":"` + strings.Repeat("a", MaxBody) + `"}`, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &queue{}
			w := do(t, q, tc.method, tc.auth, tc.ct, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, tc.want, w.Body)
			}
			if (tc.want == 200) != (len(q.got) == 1) {
				t.Errorf("queued %d notifications", len(q.got))
			}
			if strings.Contains(w.Body.String(), "HostDown") || strings.Contains(w.Body.String(), secret) {
				t.Errorf("response echoes request content: %q", w.Body)
			}
		})
	}
}

func TestWebhookStoreFailureAsksForRetry(t *testing.T) {
	w := do(t, &queue{err: errors.New("disk full")}, "POST", "Bearer "+secret, "application/json", validBody)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so Alertmanager retries", w.Code)
	}
}

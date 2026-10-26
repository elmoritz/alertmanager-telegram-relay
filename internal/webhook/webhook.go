// Package webhook receives Alertmanager notifications (SECURITY.md §4).
package webhook

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/metrics"
)

// MaxBody is the largest accepted request body.
const MaxBody = 1 << 20

// Enqueuer durably queues a notification for delivery.
type Enqueuer interface {
	Enqueue(*alertmanager.Message) error
}

// Handler serves POST /webhook.
type Handler struct {
	secret  [sha256.Size]byte
	queue   Enqueuer
	metrics *metrics.Metrics
	log     *slog.Logger
}

// New returns a handler that accepts requests carrying
// "Authorization: Bearer <secret>".
func New(secret string, q Enqueuer, m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{secret: sha256.Sum256([]byte(secret)), queue: q, metrics: m, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.reject(w, r, http.StatusMethodNotAllowed, "method")
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		h.reject(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		h.reject(w, r, http.StatusUnsupportedMediaType, "media_type")
		return
	}

	var m alertmanager.Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody)).Decode(&m); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			h.reject(w, r, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		h.reject(w, r, http.StatusBadRequest, "bad_json")
		return
	}
	if err := m.Validate(); err != nil {
		// The validation message names a field, never echoes content.
		h.log.Warn("webhook payload rejected", "err", err)
		h.reject(w, r, http.StatusBadRequest, "invalid")
		return
	}

	if err := h.queue.Enqueue(&m); err != nil {
		// 5xx makes Alertmanager retry.
		h.log.Error("queueing notification failed", "err", err)
		h.reject(w, r, http.StatusInternalServerError, "store")
		return
	}
	h.metrics.NotificationsReceived.WithLabelValues(m.Status).Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// authorized compares the bearer token in constant time. Both sides are
// hashed first so the comparison does not leak the secret's length either.
func (h *Handler) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(got[:], h.secret[:]) == 1
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, code int, reason string) {
	h.metrics.WebhookRejected.WithLabelValues(reason).Inc()
	switch code {
	case http.StatusInternalServerError: // already logged with its cause
	case http.StatusUnauthorized:
		h.log.Warn("webhook request without valid secret", "remote_addr", r.RemoteAddr)
	default:
		h.log.Debug("webhook request rejected", "reason", reason, "remote_addr", r.RemoteAddr)
	}
	http.Error(w, http.StatusText(code), code)
}

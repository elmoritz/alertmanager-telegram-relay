// Package metrics defines the relay's Prometheus metrics. Every label has a
// small, fixed set of values: no alert names, label values or IDs
// (SECURITY.md §4).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds the relay's collectors, registered on their own registry.
type Metrics struct {
	Registry *prometheus.Registry

	// NotificationsReceived counts accepted webhooks by status (firing, resolved).
	NotificationsReceived *prometheus.CounterVec
	// WebhookRejected counts refused webhook requests by reason.
	WebhookRejected *prometheus.CounterVec
	// TelegramRequests counts Bot API calls by method and result.
	TelegramRequests *prometheus.CounterVec
	// DeliveryRetries counts retried Telegram calls.
	DeliveryRetries prometheus.Counter
	// DeliveriesDropped counts notifications given up on after retrying.
	DeliveriesDropped prometheus.Counter
	// TemplateErrors counts renders that fell back to the minimal message.
	TemplateErrors prometheus.Counter
	// UpdatesRejected counts Telegram updates from chats or users not allowed.
	UpdatesRejected prometheus.Counter
	// ButtonPresses counts allowed button presses by action and result.
	ButtonPresses *prometheus.CounterVec
	// AlertmanagerRequests counts Alertmanager API calls by operation and result.
	AlertmanagerRequests *prometheus.CounterVec
}

// New registers the metrics. queueLen and openIncidents are read on scrape.
func New(queueLen, openIncidents func() float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		NotificationsReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_notifications_received_total",
			Help: "Alertmanager notifications accepted, by status.",
		}, []string{"status"}),
		WebhookRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_webhook_rejected_total",
			Help: "Webhook requests refused, by reason.",
		}, []string{"reason"}),
		TelegramRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_telegram_requests_total",
			Help: "Telegram Bot API calls, by method and result.",
		}, []string{"method", "result"}),
		DeliveryRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "relay_delivery_retries_total",
			Help: "Telegram calls retried after a temporary failure.",
		}),
		DeliveriesDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "relay_deliveries_dropped_total",
			Help: "Notifications dropped after retries were exhausted or on a permanent error.",
		}),
		TemplateErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "relay_template_errors_total",
			Help: "Renders that failed and fell back to the minimal message.",
		}),
		UpdatesRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "relay_telegram_updates_rejected_total",
			Help: "Telegram updates dropped because the chat or user is not allowed.",
		}),
		ButtonPresses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_button_presses_total",
			Help: "Button presses by allowed users, by action (silence, unsilence, unknown) and result (ok, error, stale).",
		}, []string{"action", "result"}),
		AlertmanagerRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_alertmanager_requests_total",
			Help: "Alertmanager API calls, by operation and result.",
		}, []string{"op", "result"}),
	}
	reg.MustRegister(
		m.NotificationsReceived, m.WebhookRejected, m.TelegramRequests,
		m.DeliveryRetries, m.DeliveriesDropped, m.TemplateErrors,
		m.UpdatesRejected, m.ButtonPresses, m.AlertmanagerRequests,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "relay_queue_length",
			Help: "Notifications waiting to be delivered to Telegram.",
		}, queueLen),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "relay_open_incidents",
			Help: "Incidents with an open Telegram message.",
		}, openIncidents),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

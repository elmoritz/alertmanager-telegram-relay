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
	}
	reg.MustRegister(
		m.NotificationsReceived, m.WebhookRejected, m.TelegramRequests,
		m.DeliveryRetries, m.DeliveriesDropped, m.TemplateErrors,
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

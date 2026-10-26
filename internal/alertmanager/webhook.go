// Package alertmanager holds the parts of Alertmanager's interfaces the relay
// uses: the webhook payload it receives (and, from v0.2, the silences API).
package alertmanager

import (
	"errors"
	"fmt"
	"time"
)

// Alert statuses as Alertmanager sends them.
const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// Message is Alertmanager's webhook payload (version "4"). Unknown fields are
// ignored by encoding/json; only the fields below are ever used.
type Message struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is a single alert inside a Message.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Validate rejects payloads the relay cannot act on. It does not judge alert
// content; that is rendered (escaped) as it is.
func (m *Message) Validate() error {
	if m.Version != "4" {
		return fmt.Errorf("unsupported payload version %q", m.Version)
	}
	if m.GroupKey == "" {
		return errors.New("missing groupKey")
	}
	if m.Status != StatusFiring && m.Status != StatusResolved {
		return fmt.Errorf("unknown status %q", m.Status)
	}
	if len(m.Alerts) == 0 {
		return errors.New("no alerts")
	}
	for i, a := range m.Alerts {
		if a.Status != StatusFiring && a.Status != StatusResolved {
			return fmt.Errorf("alert %d: unknown status %q", i, a.Status)
		}
	}
	return nil
}

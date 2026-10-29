//go:build live

package alertmanager_test

// Checks the silence client against a real Alertmanager. Use a local test
// instance: this creates and expires silences. See docs/LIVE-TESTING.md.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/livetest"
)

func TestLiveSilenceLifecycle(t *testing.T) {
	am := alertmanager.NewClient(livetest.AlertmanagerURL(t), "", "", "")
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id, err := am.CreateSilence(c, alertmanager.Silence{
		Matchers: []alertmanager.Matcher{
			{Name: "alertname", Value: "RelayLiveTest", IsEqual: true},
			{Name: "relay_live_test", Value: hex.EncodeToString(b), IsEqual: true},
		},
		StartsAt:  now,
		EndsAt:    now.Add(5 * time.Minute),
		CreatedBy: "telegram:0",
		Comment:   "alertmanager-telegram-relay live test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("silence %s", id)

	if err := am.ExpireSilence(c, id); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// The relay treats ErrSilenceNotFound as success when unsilencing. Any
	// other error here would make an unsilence after the silence ran out
	// report "Alertmanager is unreachable".
	if err := am.ExpireSilence(c, id); err != nil && !errors.Is(err, alertmanager.ErrSilenceNotFound) {
		t.Errorf("expiring an expired silence: %v", err)
	}
}

func TestLiveExpireUnknownSilence(t *testing.T) {
	am := alertmanager.NewClient(livetest.AlertmanagerURL(t), "", "", "")
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := am.ExpireSilence(c, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, alertmanager.ErrSilenceNotFound) {
		t.Errorf("expiring an unknown silence: %v, want ErrSilenceNotFound", err)
	}
}

// postAlert sends one alert to Alertmanager, as Prometheus would.
func postAlert(t *testing.T, base *url.URL, labels map[string]string, endsAt time.Time) {
	t.Helper()
	body, _ := json.Marshal([]map[string]any{{"labels": labels, "startsAt": time.Now().Add(-time.Minute), "endsAt": endsAt}})
	resp, err := http.Post(base.JoinPath("api/v2/alerts").String(), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("posting alert: %s", resp.Status)
	}
}

// Silenced alerts must still be counted, and resolved ones not: the relay
// closes a silenced incident when the count drops to zero.
func TestLiveActiveAlerts(t *testing.T) {
	base := livetest.AlertmanagerURL(t)
	am := alertmanager.NewClient(base, "", "", "")
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	// A value with characters the matcher syntax must escape.
	labels := map[string]string{"alertname": "RelayLiveTest", "relay_live_test": hex.EncodeToString(b) + ` "quoted" back\\slash`}
	count := func() int {
		t.Helper()
		n, err := am.ActiveAlerts(c, labels)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("before posting: %d alerts", n)
	}
	postAlert(t, base, labels, time.Now().Add(time.Hour))
	if n := count(); n != 1 {
		t.Fatalf("firing: %d alerts, want 1", n)
	}
	var matchers []alertmanager.Matcher
	for k, v := range labels {
		matchers = append(matchers, alertmanager.Matcher{Name: k, Value: v, IsEqual: true})
	}
	now := time.Now()
	id, err := am.CreateSilence(c, alertmanager.Silence{Matchers: matchers, StartsAt: now, EndsAt: now.Add(5 * time.Minute), CreatedBy: "telegram:0", Comment: "alertmanager-telegram-relay live test"})
	if err != nil {
		t.Fatal(err)
	}
	defer am.ExpireSilence(context.Background(), id)
	if n := count(); n != 1 {
		t.Errorf("silenced: %d alerts, want 1", n)
	}
	postAlert(t, base, labels, time.Now())
	if n := count(); n != 0 {
		t.Errorf("resolved: %d alerts, want 0", n)
	}
}

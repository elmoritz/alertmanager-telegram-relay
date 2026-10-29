//go:build live

package alertmanager_test

// Checks the silence client against a real Alertmanager. Use a local test
// instance: this creates and expires silences. See docs/LIVE-TESTING.md.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

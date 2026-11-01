//go:build live

package komodo_test

// Restarts a throwaway stack through a real Komodo. See docs/LIVE-TESTING.md.
// Only point RELAY_LIVE_KOMODO_STACK at a stack that may be restarted at any
// time.

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/komodo"
)

func TestLiveRestartStack(t *testing.T) {
	base, stack := os.Getenv("RELAY_LIVE_KOMODO_URL"), os.Getenv("RELAY_LIVE_KOMODO_STACK")
	keyFile, secretFile := os.Getenv("RELAY_LIVE_KOMODO_API_KEY_FILE"), os.Getenv("RELAY_LIVE_KOMODO_API_SECRET_FILE")
	if base == "" || stack == "" || keyFile == "" || secretFile == "" {
		t.Skip("RELAY_LIVE_KOMODO_URL, _STACK, _API_KEY_FILE or _API_SECRET_FILE not set")
	}
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		return strings.TrimSpace(string(b))
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := komodo.ParseActions([]byte(`{"actions":[{"id":"live","label":"Restart test stack","operation":"RestartStack","stack":"` + stack + `","match":{"alertname":"RelayLiveTest"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := komodo.New(u, read(keyFile), read(secretFile))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	id, err := c.Start(ctx, actions[0])
	if err != nil {
		t.Fatal(err)
	}
	ok, err := c.Wait(ctx, id)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	t.Logf("restarted in %s", time.Since(start).Round(100*time.Millisecond))

	deploy, err := komodo.ParseActions([]byte(`{"actions":[{"id":"live","label":"Redeploy test stack","operation":"DeployStack","stack":"` + stack + `","match":{"alertname":"RelayLiveTest"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if id, err = c.Start(ctx, deploy[0]); err != nil {
		t.Fatal(err)
	}
	if ok, err = c.Wait(ctx, id); err != nil || !ok {
		t.Fatalf("deploy: ok %v, err %v", ok, err)
	}
	t.Logf("redeployed in %s", time.Since(start).Round(100*time.Millisecond))

	// RestartContainer needs a standalone throwaway container the Komodo
	// user may restart: RELAY_LIVE_KOMODO_SERVER and _CONTAINER.
	if server, container := os.Getenv("RELAY_LIVE_KOMODO_SERVER"), os.Getenv("RELAY_LIVE_KOMODO_CONTAINER"); server != "" && container != "" {
		restart, err := komodo.ParseActions([]byte(`{"actions":[{"id":"live","label":"Restart test container","operation":"RestartContainer","server":"` + server + `","container":"` + container + `","match":{"alertname":"RelayLiveTest"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if id, err = c.Start(ctx, restart[0]); err != nil {
			t.Fatal(err)
		}
		if ok, err = c.Wait(ctx, id); err != nil || !ok {
			t.Fatalf("restart container: ok %v, err %v", ok, err)
		}
		t.Logf("restarted the container in %s", time.Since(start).Round(100*time.Millisecond))
	} else {
		t.Log("RELAY_LIVE_KOMODO_SERVER or _CONTAINER not set: RestartContainer not checked")
	}

	// A wrong secret fails without leaking it.
	_, err = komodo.New(u, read(keyFile), "S-wrong").Start(ctx, actions[0])
	if err == nil || strings.Contains(err.Error(), "S-wrong") {
		t.Errorf("wrong secret: %v", err)
	}
	t.Logf("wrong secret: %v", err)
}

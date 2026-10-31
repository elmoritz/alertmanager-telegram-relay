//go:build live

package grafana_test

// Renders a real panel through a Grafana with the image renderer. See
// docs/LIVE-TESTING.md.

import (
	"bytes"
	"context"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/grafana"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/livetest"
)

func TestLiveRenderPanel(t *testing.T) {
	base, token, panel := livetest.Grafana(t)
	c := grafana.New(base, nil, token, "Europe/Berlin")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	img, err := c.RenderPanel(ctx, panel)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	t.Logf("rendered %dx%d, %d KB in %s", cfg.Width, cfg.Height, len(img)/1024, time.Since(start).Round(100*time.Millisecond))

	// A wrong token must fail without leaking it.
	_, err = grafana.New(base, nil, token+"x", "Europe/Berlin").RenderPanel(ctx, panel)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("wrong token: %v", err)
	}
	t.Logf("wrong token: %v", err)
}

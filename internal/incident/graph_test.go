package incident

import (
	"context"
	"errors"
	"testing"
)

type fakeGraphs struct {
	panels []string
	err    error
}

func (f *fakeGraphs) RenderPanel(_ context.Context, panelURL string) ([]byte, error) {
	f.panels = append(f.panels, panelURL)
	return []byte("\x89PNG"), f.err
}

func TestGraphImageForNewIncident(t *testing.T) {
	e := newEnv(t)
	g := &fakeGraphs{}
	e.proc.Graphs = g
	m := msg("a")
	m.Alerts[0].Annotations["panel_url"] = "https://grafana.example.com/d/abc/x?viewPanel=2"
	calls := e.handle(m)
	e.proc.WaitGraphs()
	calls = append(calls, e.tg.take()...)
	expectCalls(t, calls, "sendMessage", "sendPhoto(reply #1) silent")
	if len(g.panels) != 1 || g.panels[0] != "https://grafana.example.com/d/abc/x?viewPanel=2" {
		t.Errorf("rendered %v", g.panels)
	}

	// Only for a new incident, not for later notifications.
	m = msg("a", "b")
	m.Alerts[0].Annotations["panel_url"] = "https://grafana.example.com/d/abc/x?viewPanel=2"
	e.handle(m)
	e.proc.WaitGraphs()
	if calls := e.tg.take(); len(g.panels) != 1 || len(calls) > 0 {
		t.Errorf("rendered again: %v, calls %v", g.panels, calls)
	}
}

func TestGraphImageIsBestEffort(t *testing.T) {
	e := newEnv(t)
	g := &fakeGraphs{err: errors.New("grafana render: 500 Internal Server Error")}
	e.proc.Graphs = g
	m := msg("a")
	m.Alerts[0].Annotations["panel_url"] = "https://grafana.example.com/d/abc/x?viewPanel=2"
	expectCalls(t, e.handle(m), "sendMessage")
	e.proc.WaitGraphs()
	if calls := e.tg.take(); len(calls) > 0 {
		t.Errorf("calls after a failed render: %v", calls)
	}

	for name, panel := range map[string]string{"no annotation": "", "not http": "javascript:alert(1)"} {
		g.panels = nil
		m := msg("x")
		m.GroupKey = name
		if panel != "" {
			m.Alerts[0].Annotations["panel_url"] = panel
		}
		e.handle(m)
		e.proc.WaitGraphs()
		if len(g.panels) > 0 {
			t.Errorf("%s: rendered %v", name, g.panels)
		}
	}
}

package grafana

import (
	"net/url"
	"strings"
	"testing"
)

// Whatever panel_url an alert carries, the render URL stays on the render
// base, under /render/d-solo/, with only known parameters: the token never
// goes anywhere else.
func FuzzRenderURL(f *testing.F) {
	for _, s := range []string{
		"https://grafana.example.com/grafana/d/abc/x?viewPanel=4&var-host=a&from=now-1h",
		"https://grafana.example.com@evil.example.org/grafana/d/abc?viewPanel=1",
		"https://grafana.example.com/grafana/d/../../api/admin?viewPanel=1",
		"https://grafana.example.com/grafana/d-solo/abc/x?panelId=1&orgId=2",
	} {
		f.Add(s)
	}
	base, _ := url.Parse("https://grafana.example.com/grafana")
	render, _ := url.Parse("http://grafana.internal.example.com:3000")
	c := New(base, render, testToken, "UTC")
	allowed := map[string]bool{"panelId": true, "width": true, "height": true, "from": true, "to": true, "tz": true, "orgId": true}
	f.Fuzz(func(t *testing.T, panelURL string) {
		u, err := c.RenderURL(panelURL)
		if err != nil {
			return
		}
		if u.Scheme != render.Scheme || u.Host != render.Host || !strings.HasPrefix(u.Path, "/render/d-solo/") || u.User != nil || u.Fragment != "" {
			t.Fatalf("RenderURL(%q) = %s", panelURL, u)
		}
		if strings.Contains(u.Path, "..") {
			t.Fatalf("traversal in %s", u)
		}
		for k := range u.Query() {
			if !allowed[k] && !strings.HasPrefix(k, "var-") {
				t.Fatalf("parameter %q in %s", k, u)
			}
		}
	})
}

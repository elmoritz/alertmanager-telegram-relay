package grafana

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testToken = "glsa_TEST-TOKEN-not-real"

func client(t *testing.T, base string) *Client {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return New(u, testToken, "Europe/Berlin")
}

func TestRenderURL(t *testing.T) {
	c := client(t, "https://grafana.example.com/grafana")
	for in, want := range map[string]string{
		"https://grafana.example.com/grafana/d/fleet-servers/servers?orgId=1&viewPanel=4":                     "https://grafana.example.com/grafana/render/d-solo/fleet-servers/servers?from=now-3h&height=500&orgId=1&panelId=4&to=now&tz=Europe%2FBerlin&width=1000",
		"https://grafana.example.com/grafana/d-solo/abc/x?panelId=12&from=now-24h&to=now&var-host=server-1":   "https://grafana.example.com/grafana/render/d-solo/abc/x?from=now-24h&height=500&panelId=12&to=now&tz=Europe%2FBerlin&var-host=server-1&width=1000",
		"https://grafana.example.com/grafana/d/abc?viewPanel=panel-7":                                         "https://grafana.example.com/grafana/render/d-solo/abc/panel?from=now-3h&height=500&panelId=7&to=now&tz=Europe%2FBerlin&width=1000",
		"https://GRAFANA.example.com/grafana/d/abc/x?viewPanel=1&from=x;rm&to=now-1h&orgId=1;2&kiosk&theme=x": "https://grafana.example.com/grafana/render/d-solo/abc/x?from=now-3h&height=500&panelId=1&to=now-1h&tz=Europe%2FBerlin&width=1000",
	} {
		got, err := c.RenderURL(in)
		if err != nil || got.String() != want {
			t.Errorf("RenderURL(%s)\n got  %v %v\n want %s", in, got, err, want)
		}
	}
	for _, in := range []string{
		"https://evil.example.org/grafana/d/abc/x?viewPanel=1",                   // other host
		"https://grafana.example.com@evil.example.org/grafana/d/abc?viewPanel=1", // userinfo trick
		"http://grafana.example.com/grafana/d/abc/x?viewPanel=1",                 // other scheme
		"https://grafana.example.com:8443/grafana/d/abc/x?viewPanel=1",           // other port
		"https://grafana.example.com/grafana/d/abc/x",                            // no panel
		"https://grafana.example.com/grafana/d/abc/x?viewPanel=1;2",              // bad panel
		"https://grafana.example.com/grafana/api/admin/users?viewPanel=1",        // not a dashboard
		"https://grafana.example.com/grafana/d/../../api/admin?viewPanel=1",      // traversal
		"javascript:alert(1)",
		"",
	} {
		if got, err := c.RenderURL(in); err == nil {
			t.Errorf("RenderURL(%q) = %v, want an error", in, got)
		}
	}
}

func TestRenderPanel(t *testing.T) {
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "\x89PNG fake")
	}))
	defer srv.Close()
	c := client(t, srv.URL)
	var results []string
	c.OnCall = func(r string) { results = append(results, r) }

	img, err := c.RenderPanel(context.Background(), srv.URL+"/d/abc/x?viewPanel=2")
	if err != nil || string(img) != "\x89PNG fake" {
		t.Fatalf("%q %v", img, err)
	}
	if auth != "Bearer "+testToken || path != "/render/d-solo/abc/x" || strings.Join(results, ",") != "ok" {
		t.Errorf("auth %q, path %q, results %v", auth, path, results)
	}
}

func TestRenderErrorsLeakNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("panelId") {
		case "1": // Grafana without the image renderer
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"No image renderer available/installed", "query":"secret_label"}`)
		case "2":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>login</html>")
		case "3":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, strings.Repeat("x", maxImageSize+1))
		}
	}))
	c := client(t, srv.URL)
	for _, panel := range []string{"1", "2", "3"} {
		_, err := c.RenderPanel(context.Background(), srv.URL+"/d/abc/x?var-host=server-1&viewPanel="+panel)
		if err == nil {
			t.Fatalf("panel %s: no error", panel)
		}
		for _, leak := range []string{testToken, "server-1", "secret_label", "/render"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("panel %s: error %q contains %q", panel, err, leak)
			}
		}
	}
	srv.Close()
	_, err := c.RenderPanel(context.Background(), srv.URL+"/d/abc/x?var-host=server-1&viewPanel=1")
	if err == nil || strings.Contains(err.Error(), "server-1") || strings.Contains(err.Error(), testToken) {
		t.Errorf("network error = %v", err)
	}
}

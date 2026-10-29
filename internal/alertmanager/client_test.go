package alertmanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeAM struct {
	t        *testing.T
	method   string
	path     string
	auth     string
	silence  Silence
	status   int
	response string
}

func (f *fakeAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.method, f.path, f.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
	if r.Method == http.MethodPost {
		if r.Header.Get("Content-Type") != "application/json" {
			f.t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &f.silence); err != nil {
			f.t.Error(err)
		}
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	io.WriteString(w, f.response)
}

func client(t *testing.T, f *fakeAM, prefix, user, pass, token string) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL + prefix)
	return NewClient(u, user, pass, token)
}

var start = time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC)

func silence() Silence {
	return Silence{
		Matchers:  []Matcher{{Name: "alertname", Value: "HostDown", IsEqual: true}, {Name: "instance", Value: "server-1", IsEqual: true}},
		StartsAt:  start,
		EndsAt:    start.Add(time.Hour),
		CreatedBy: "telegram:11111111",
		Comment:   "Silenced from Telegram via alertmanager-telegram-relay",
	}
}

func TestCreateSilence(t *testing.T) {
	f := &fakeAM{t: t, response: `{"silenceID":"7d8f2c1e-0000-4000-8000-000000000001"}`}
	c := client(t, f, "/alertmanager", "", "", "am-test-token")
	id, err := c.CreateSilence(context.Background(), silence())
	if err != nil || id != "7d8f2c1e-0000-4000-8000-000000000001" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if f.method != "POST" || f.path != "/alertmanager/api/v2/silences" || f.auth != "Bearer am-test-token" {
		t.Errorf("request %s %s auth=%q", f.method, f.path, f.auth)
	}
	if len(f.silence.Matchers) != 2 || f.silence.Matchers[0].IsRegex || !f.silence.Matchers[0].IsEqual || f.silence.CreatedBy != "telegram:11111111" {
		t.Errorf("silence = %+v", f.silence)
	}
}

func TestCreateSilenceRefusesUnsafe(t *testing.T) {
	f := &fakeAM{t: t, response: `{"silenceID":"x"}`}
	c := client(t, f, "", "", "", "")
	for name, mutate := range map[string]func(*Silence){
		"no matchers": func(s *Silence) { s.Matchers = nil },
		"regex":       func(s *Silence) { s.Matchers[0].IsRegex = true },
		"negative":    func(s *Silence) { s.Matchers[0].IsEqual = false },
		"empty name":  func(s *Silence) { s.Matchers[1].Name = "" },
		"backwards":   func(s *Silence) { s.EndsAt = s.StartsAt },
	} {
		s := silence()
		mutate(&s)
		if _, err := c.CreateSilence(context.Background(), s); !errors.Is(err, ErrUnsafeSilence) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if f.method != "" {
		t.Error("an unsafe silence reached Alertmanager")
	}
}

func TestCreateSilenceErrors(t *testing.T) {
	f := &fakeAM{t: t, status: 500, response: `matchers: instance="server-1" something`}
	c := client(t, f, "", "relay", "am-test-password", "")
	_, err := c.CreateSilence(context.Background(), silence())
	if err == nil || strings.Contains(err.Error(), "server-1") || strings.Contains(err.Error(), "am-test-password") {
		t.Errorf("err = %v", err)
	}
	if user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {f.auth}}}).BasicAuth(); !ok || user != "relay" || pass != "am-test-password" {
		t.Errorf("basic auth not sent: %q", f.auth)
	}

	f.status, f.response = 200, `{"silenceID":"../../etc"}`
	if _, err := c.CreateSilence(context.Background(), silence()); err == nil {
		t.Error("odd silence ID accepted")
	}
}

func TestExpireSilence(t *testing.T) {
	f := &fakeAM{t: t}
	c := client(t, f, "", "", "", "")
	if err := c.ExpireSilence(context.Background(), "7d8f2c1e-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if f.method != "DELETE" || f.path != "/api/v2/silence/7d8f2c1e-0000-4000-8000-000000000001" || f.auth != "" {
		t.Errorf("request %s %s auth=%q", f.method, f.path, f.auth)
	}
	f.status = 404
	if err := c.ExpireSilence(context.Background(), "abc"); !errors.Is(err, ErrSilenceNotFound) {
		t.Errorf("404: %v", err)
	}
	if err := c.ExpireSilence(context.Background(), "../x"); err == nil {
		t.Error("path traversal ID accepted")
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close()
	c := NewClient(u, "", "", "am-test-token")
	if _, err := c.CreateSilence(context.Background(), silence()); err == nil || strings.Contains(err.Error(), "am-test-token") {
		t.Errorf("err = %v", err)
	}
}

func TestActiveAlerts(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/am/api/v2/alerts" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		query = r.URL.Query()
		io.WriteString(w, `[{"labels":{"alertname":"DiskFull"},"status":{"state":"suppressed"}},{"labels":{"alertname":"DiskFull"},"status":{"state":"active"}}]`)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/am")

	n, err := NewClient(base, "", "", "").ActiveAlerts(context.Background(), map[string]string{"instance": `server-1 "a"\b`, "alertname": "DiskFull"})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := strings.Join(query["filter"], " "); got != `alertname="DiskFull" instance="server-1 \"a\"\\b"` {
		t.Errorf("filter = %s", got)
	}
	for _, p := range []string{"active", "silenced", "inhibited", "unprocessed"} {
		if query.Get(p) != "true" {
			t.Errorf("%s = %q", p, query.Get(p))
		}
	}
	if _, err := NewClient(base, "", "", "").ActiveAlerts(context.Background(), nil); err == nil {
		t.Error("no labels must be an error, not all alerts")
	}
}

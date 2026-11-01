package komodo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const validFile = `{"actions": [
 {"id": "restart-media", "label": "Restart stack media", "operation": "RestartStack", "stack": "media", "match": {"team": "media"}},
 {"id": "redeploy-media", "label": "Redeploy stack media", "operation": "DeployStack", "stack": "media", "services": ["jellyfin"], "match": {"team": "media", "alertname": "JellyfinDown"}, "cooldown": "30m"},
 {"id": "restart-proxy", "label": "Restart proxy", "operation": "RestartContainer", "server": "server-1", "container": "traefik", "match": {"alertname": "ProxyDown"}}
]}`

func TestParseActions(t *testing.T) {
	as, err := ParseActions([]byte(validFile))
	if err != nil || len(as) != 3 {
		t.Fatalf("%v %v", as, err)
	}
	if as[0].Cooldown != DefaultCooldown || as[1].Cooldown != 30*time.Minute || as[1].Services[0] != "jellyfin" || as[2].Container != "traefik" {
		t.Errorf("parsed %+v", as)
	}
	if !as[0].Offered(map[string]string{"team": "media", "alertname": "X"}) || as[0].Offered(map[string]string{"team": "infra"}) || as[1].Offered(map[string]string{"team": "media"}) {
		t.Error("Offered")
	}
}

func TestParseActionsErrors(t *testing.T) {
	act := func(fields string) string {
		return `{"actions":[{"id":"a","label":"Restart","match":{"team":"media"},` + fields + `}]}`
	}
	for name, s := range map[string]string{
		"not json":          `actions`,
		"unknown field":     `{"actions":[], "shell":"rm -rf /"}`,
		"no actions":        `{"actions":[]}`,
		"bad id":            `{"actions":[{"id":"Restart Media","label":"x","operation":"RestartStack","stack":"media","match":{"team":"media"}}]}`,
		"no match":          `{"actions":[{"id":"a","label":"x","operation":"RestartStack","stack":"media"}]}`,
		"empty match value": `{"actions":[{"id":"a","label":"x","operation":"RestartStack","stack":"media","match":{"team":""}}]}`,
		"other operation":   act(`"operation":"DestroyStack","stack":"media"`),
		"stack with spaces": act(`"operation":"RestartStack","stack":"media; rm"`),
		"stack and server":  act(`"operation":"RestartStack","stack":"media","server":"x"`),
		"container no srv":  act(`"operation":"RestartContainer","container":"traefik"`),
		"bad service":       act(`"operation":"RestartStack","stack":"media","services":["a b"]`),
		"short cooldown":    act(`"operation":"RestartStack","stack":"media","cooldown":"30s"`),
		"long label":        `{"actions":[{"id":"a","label":"` + strings.Repeat("x", 41) + `","operation":"RestartStack","stack":"media","match":{"team":"media"}}]}`,
		"duplicate id":      `{"actions":[` + strings.Repeat(`{"id":"a","label":"x","operation":"RestartStack","stack":"media","match":{"t":"v"}},`, 2)[:len(`{"id":"a","label":"x","operation":"RestartStack","stack":"media","match":{"t":"v"}},`)*2-1] + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseActions([]byte(s))
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "media") || strings.Contains(err.Error(), "rm -rf") || strings.Contains(err.Error(), "; rm") {
				t.Errorf("error echoes a value: %v", err)
			}
		})
	}
}

const (
	testKey    = "K-test-key-not-real"
	testSecret = "S-test-secret-not-real"
)

func TestStartAndWait(t *testing.T) {
	var paths, bodies []string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != testKey || r.Header.Get("X-Api-Secret") != testSecret || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %v", r.Method, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(b))
		switch r.URL.Path {
		case "/komodo/execute/RestartStack", "/komodo/execute/RestartContainer":
			io.WriteString(w, `{"_id":{"$oid":"6650a1b2c3d4e5f600000001"},"status":"InProgress","success":false,"logs":[{"stdout":"secret details"}]}`)
		case "/komodo/read/GetUpdate":
			polls++
			status := map[bool]string{true: "Complete", false: "InProgress"}[polls >= 2]
			io.WriteString(w, `{"_id":{"$oid":"6650a1b2c3d4e5f600000001"},"status":"`+status+`","success":true}`)
		}
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/komodo")
	c := New(base, testKey, testSecret)
	c.PollEvery = time.Millisecond
	as, _ := ParseActions([]byte(validFile))

	id, err := c.Start(context.Background(), as[0])
	if err != nil || id != "6650a1b2c3d4e5f600000001" {
		t.Fatalf("id %q err %v", id, err)
	}
	ok, err := c.Wait(context.Background(), id)
	if err != nil || !ok || polls != 2 {
		t.Fatalf("ok %v err %v polls %d", ok, err, polls)
	}
	if bodies[0] != `{"stack":"media","services":[]}` || bodies[1] != `{"id":"6650a1b2c3d4e5f600000001"}` {
		t.Errorf("bodies %q", bodies)
	}
	if _, err := c.Start(context.Background(), as[2]); err != nil {
		t.Fatal(err)
	}
	var container map[string]string
	_ = json.Unmarshal([]byte(bodies[len(bodies)-1]), &container)
	if container["server"] != "server-1" || container["container"] != "traefik" || paths[len(paths)-1] != "/komodo/execute/RestartContainer" {
		t.Errorf("container request %s %v", paths[len(paths)-1], container)
	}
}

func TestErrorsLeakNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"user lacks permission on server-1 at 192.0.2.1"}`)
	}))
	base, _ := url.Parse(srv.URL)
	c := New(base, testKey, testSecret)
	as, _ := ParseActions([]byte(validFile))
	_, err := c.Start(context.Background(), as[0])
	srv.Close()
	_, err2 := c.Start(context.Background(), as[0])
	for _, e := range []error{err, err2} {
		if e == nil {
			t.Fatal("no error")
		}
		for _, leak := range []string{testKey, testSecret, "server-1", "192.0.2.1", "permission"} {
			if strings.Contains(e.Error(), leak) {
				t.Errorf("error %q contains %q", e, leak)
			}
		}
	}
}

func TestWaitGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"_id":{"$oid":"x"},"status":"InProgress","success":false}`)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, testKey, testSecret)
	c.PollEvery = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Wait(ctx, "x"); err == nil {
		t.Error("Wait did not give up")
	}
}

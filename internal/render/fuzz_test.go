package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
)

// Any payload that passes validation renders to a message Telegram accepts:
// not empty, within the length limit, only the relay's own tags.
func FuzzMessage(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	r, err := New("", time.UTC, func() time.Time { return testNow })
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		var m alertmanager.Message
		if json.Unmarshal(payload, &m) != nil || m.Validate() != nil {
			return
		}
		d := NewData(&m)
		d.Added = d.Alerts.Firing()
		msg, _ := r.Message(d) // falls back to the minimal message on errors
		if n := Len(msg); n == 0 || n > MaxLen {
			t.Fatalf("length %d", n)
		}
		assertWellFormed(t, msg)
		for _, reply := range []func(Data) (string, error){r.Added, r.Reminder, r.Escalation, r.ResolveReply} {
			s, _ := reply(d)
			if n := Len(s); n == 0 || n > MaxLen {
				t.Fatalf("reply length %d", n)
			}
			assertWellFormed(t, s)
		}
	})
}

// The status board, built from arbitrary alert content, has the same
// guarantees, with the relay's own t.me links as the only extra tags.
func FuzzBoard(f *testing.F) {
	f.Add("HostDown", "server-1", "<b>x</b> & \"y\"")
	r, err := New("", time.UTC, func() time.Time { return testNow })
	if err != nil {
		f.Fatal(err)
	}
	link := regexp.MustCompile(`<a href="https://t\.me/c/[0-9/]+">|</a>`)
	f.Fuzz(func(t *testing.T, name, instance, summary string) {
		a := alertmanager.Alert{Status: alertmanager.StatusFiring, Labels: map[string]string{"alertname": name, "instance": instance},
			Annotations: map[string]string{"summary": summary}, StartsAt: testNow}
		d := NewData(&alertmanager.Message{Status: alertmanager.StatusFiring, Alerts: []alertmanager.Alert{a, a}, CommonLabels: a.Labels})
		s, err := r.Board([]BoardItem{{Data: d, URL: "https://t.me/c/1234567890/42"}, {Data: d}}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if n := Len(s); n == 0 || n > MaxLen {
			t.Fatalf("length %d", n)
		}
		assertWellFormed(t, link.ReplaceAllString(s, ""))
	})
}

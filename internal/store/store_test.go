package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIncidents(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 3, 2, 4, 56, 0, 0, time.UTC)

	if inc, err := s.Incident("{}:{alertname=\"HostDown\"}"); err != nil || inc != nil {
		t.Fatalf("empty store: %v %v", inc, err)
	}
	want := &Incident{GroupKey: "{}:{alertname=\"HostDown\"}", ChatID: -1001234567890, MessageID: 7, OpenedAt: now, LastSeen: now, Firing: []string{"a", "b"}}
	if err := s.PutIncident(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Incident(want.GroupKey)
	if err != nil || got == nil || got.MessageID != 7 || len(got.Firing) != 2 || !got.OpenedAt.Equal(now) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n, _ := s.CountIncidents(); n != 1 {
		t.Errorf("count = %d", n)
	}
	if err := s.DeleteIncident(want.GroupKey); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Incident(want.GroupKey); got != nil {
		t.Error("incident not deleted")
	}
	if err := s.DeleteIncident("missing"); err != nil {
		t.Errorf("deleting missing: %v", err)
	}
}

func TestHugeGroupKey(t *testing.T) {
	s := open(t)
	key := strings.Repeat("x", 100_000)
	if err := s.PutIncident(&Incident{GroupKey: key, MessageID: 3}); err != nil {
		t.Fatal(err)
	}
	if inc, err := s.Incident(key); err != nil || inc == nil || inc.MessageID != 3 || inc.GroupKey != key {
		t.Fatalf("got %v, %v", inc, err)
	}
	if err := s.DeleteIncident(key); err != nil {
		t.Fatal(err)
	}
	if inc, _ := s.Incident(key); inc != nil {
		t.Error("not deleted")
	}
}

func TestExpireIncidents(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 3, 2, 4, 56, 0, 0, time.UTC)
	for i, age := range []time.Duration{time.Hour, 40 * 24 * time.Hour, 31 * 24 * time.Hour, 0} {
		if err := s.PutIncident(&Incident{GroupKey: string(rune('a' + i)), LastSeen: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ExpireIncidents(now.Add(-30 * 24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("expired %d, %v", n, err)
	}
	for _, k := range []string{"a", "d"} {
		if inc, _ := s.Incident(k); inc == nil {
			t.Errorf("%s should have survived", k)
		}
	}
}

func TestQueueOrderPerChat(t *testing.T) {
	s := open(t)
	const a, b = int64(-1001234567890), int64(-1001234567891)
	for _, e := range []struct {
		chat int64
		p    string
	}{{a, "a1"}, {b, "b1"}, {a, "a2"}, {b, "b2"}, {a, "a3"}} {
		if err := s.Enqueue(e.chat, []byte(e.p)); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.QueueLen(); n != 5 {
		t.Errorf("len = %d", n)
	}
	if chats, _ := s.QueueChats(); len(chats) != 2 {
		t.Errorf("chats = %v", chats)
	}
	var got []string
	for {
		j, err := s.Next(a)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		got = append(got, string(j.Payload))
		if err := s.Done(j); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 || got[0] != "a1" || got[1] != "a2" || got[2] != "a3" {
		t.Errorf("chat a = %v", got)
	}
	if j, _ := s.Next(b); j == nil || string(j.Payload) != "b1" {
		t.Errorf("chat b first = %v", j)
	}
}

func TestQueueSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(1, []byte("pending")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j, _ := s.Next(1); j == nil || string(j.Payload) != "pending" {
		t.Fatalf("job lost across restart: %v", j)
	}
	if fi, err := os.Stat(filepath.Join(dir, FileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi.Mode(), err)
	}
	if err := s.Ping(); err != nil {
		t.Error(err)
	}
}

func TestActions(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC)
	a := &Action{ID: "a1", Kind: ActionSilence, GroupKey: "g", Duration: time.Hour, ExpiresAt: now.Add(7 * 24 * time.Hour)}
	b := &Action{ID: "b1", Kind: ActionUnsilence, GroupKey: "g", SilenceID: "s", ExpiresAt: now.Add(time.Hour)}
	if err := s.PutActions(a, b); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Action("a1", now); err != nil || got == nil || got.Duration != time.Hour || got.Kind != ActionSilence {
		t.Fatalf("got %+v %v", got, err)
	}
	if got, _ := s.Action("b1", now.Add(time.Hour)); got != nil {
		t.Error("expired action returned")
	}
	for _, id := range []string{"", "missing", strings.Repeat("x", 65)} {
		if got, err := s.Action(id, now); got != nil || err != nil {
			t.Errorf("Action(%q) = %v, %v", id, got, err)
		}
	}
	if n, err := s.ExpireActions(now.Add(2 * time.Hour)); err != nil || n != 1 {
		t.Errorf("expired %d, %v", n, err)
	}
	if err := s.DeleteActions("a1", "missing"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Action("a1", now); got != nil {
		t.Error("not deleted")
	}
}

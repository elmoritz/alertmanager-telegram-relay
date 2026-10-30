package incident

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDailyDigest(t *testing.T) {
	e := newButtonEnv(t)
	at := 8*60 + 30
	e.proc.DigestAt, e.proc.DigestChats, e.proc.Location = &at, []int64{chat}, time.UTC
	digest := func(now time.Time) []call {
		e.now = now
		e.proc.CheckDigest(context.Background())
		return e.tg.take()
	}

	// Two incidents of the same alert name resolve, one keeps firing silenced.
	e.now = time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC)
	e.handle(gmsg("a"))
	e.now = e.now.Add(2 * time.Hour)
	e.handle(gmsg("a-"))
	m := gmsg("b")
	m.GroupKey = "second"
	e.handle(m)
	e.now = e.now.Add(30 * time.Minute)
	m = gmsg("b-")
	m.GroupKey = "second"
	e.handle(m)
	m = gmsg("c")
	m.GroupKey = "open"
	m.CommonLabels = map[string]string{"alertname": "BackupLate", "instance": "nas-1"}
	e.press(button(t, e.handle(m)[0].Keyboard, "🔕 24h"), chat, 3, admin)
	e.tg.take()

	if calls := digest(time.Date(2026, 3, 2, 8, 29, 0, 0, time.UTC)); len(calls) > 0 {
		t.Fatalf("digest before DIGEST_AT: %v", calls)
	}
	calls := digest(time.Date(2026, 3, 2, 8, 30, 0, 0, time.UTC))
	expectCalls(t, calls, "sendMessage silent")
	text := calls[0].Text
	for _, want := range []string{
		"📋 <b>Daily digest</b>",
		"Last 24 h: 2 resolved",
		"• <b>DiskAlmostFull</b>, 2h\n• <b>DiskAlmostFull</b>, 30m", // longest first
		"Still firing:\n• <b>BackupLate</b> · nas-1, since",
		"Silences ending within 24 h:\n• <b>BackupLate</b> · nas-1,",
		"Noisiest (7 days): DiskAlmostFull ×2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	if calls := digest(time.Date(2026, 3, 2, 20, 0, 0, 0, time.UTC)); len(calls) > 0 {
		t.Errorf("second digest on the same day: %v", calls)
	}
	calls = digest(time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC))
	expectCalls(t, calls, "sendMessage silent")
	if !strings.Contains(calls[0].Text, "Last 24 h: nothing resolved.") {
		t.Errorf("next day:\n%s", calls[0].Text)
	}
}

func TestNoDigestWhenOff(t *testing.T) {
	e := newEnv(t)
	e.proc.DigestChats = []int64{chat}
	e.proc.CheckDigest(context.Background())
	if calls := e.tg.take(); len(calls) > 0 {
		t.Errorf("digest without DIGEST_AT: %v", calls)
	}
}

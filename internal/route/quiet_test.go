package route

import (
	"testing"
	"time"
)

func TestQuiet(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	night, err := ParseWindow("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	q := Quiet{Severities: []string{"info"}, Hours: &night, Location: loc}
	at := func(hh, mm int) time.Time { return time.Date(2026, 3, 2, hh, mm, 0, 0, loc).UTC() }
	for _, tc := range []struct {
		severity string
		t        time.Time
		want     bool
	}{
		{"critical", at(23, 0), false}, // critical always rings
		{"info", at(12, 0), true},
		{"warning", at(12, 0), false},
		{"warning", at(22, 0), true},
		{"warning", at(6, 59), true},
		{"warning", at(7, 0), false},
		{"", at(3, 0), true},
		{"", at(21, 59), false},
	} {
		if got := q.Silent(tc.severity, tc.t); got != tc.want {
			t.Errorf("Silent(%q, %s) = %v, want %v", tc.severity, tc.t.In(loc).Format("15:04"), got, tc.want)
		}
	}

	day, _ := ParseWindow("09:00-17:30")
	if !day.Contains(at(17, 29).In(loc)) || day.Contains(at(17, 30).In(loc)) || day.Contains(at(8, 59).In(loc)) {
		t.Error("daytime window")
	}
	if (Quiet{}).Silent("warning", at(3, 0)) {
		t.Error("no quiet settings must ring")
	}
}

func TestParseWindowErrors(t *testing.T) {
	for _, s := range []string{"", "22:00", "22:00-22:00", "25:00-07:00", "22-07", "22:00-07:00-08:00"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q): no error", s)
		}
	}
}

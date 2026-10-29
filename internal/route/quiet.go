package route

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// Critical is the severity that always rings (SPEC §7).
const Critical = "critical"

// Quiet decides which notifications are sent without sound:
// QUIET_SEVERITIES, and everything but critical during QUIET_HOURS.
type Quiet struct {
	Severities []string
	Hours      *Window // nil: no quiet hours
	Location   *time.Location
}

// Silent reports whether a notification of the given severity is sent
// without sound at now. Critical always rings.
func (q Quiet) Silent(severity string, now time.Time) bool {
	if severity == Critical {
		return false
	}
	if slices.Contains(q.Severities, severity) {
		return true
	}
	return q.Hours != nil && q.Hours.Contains(now.In(q.Location))
}

// Window is a daily time range in minutes after midnight. From > To spans
// midnight (22:00-07:00).
type Window struct {
	From, To int
}

// Contains reports whether t's time of day is in the window, From inclusive,
// To exclusive.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.From < w.To {
		return m >= w.From && m < w.To
	}
	return m >= w.From || m < w.To
}

// ParseWindow reads "22:00-07:00".
func ParseWindow(s string) (Window, error) {
	from, to, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, errors.New(`want "HH:MM-HH:MM"`)
	}
	var w Window
	var err error
	if w.From, err = clock(from); err != nil {
		return Window{}, err
	}
	if w.To, err = clock(to); err != nil {
		return Window{}, err
	}
	if w.From == w.To {
		return Window{}, errors.New("start and end are the same")
	}
	return w, nil
}

func clock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, errors.New(`want "HH:MM-HH:MM"`)
	}
	return t.Hour()*60 + t.Minute(), nil
}

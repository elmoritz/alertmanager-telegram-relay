package incident

import (
	"testing"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
)

func TestReminderReplace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		replace   bool
		reminders []string // calls for the second and third reminder
	}{
		{"reply, keep", config.RemindersReply, false, []string{"sendMessage(reply #1)", "sendMessage(reply #1)"}},
		{"silent, keep", config.RemindersSilent, false, []string{"sendMessage(reply #1) silent", "sendMessage(reply #1) silent"}},
		{"reply, replace", config.RemindersReply, true, []string{"sendMessage(reply #1)", "deleteMessage(#2), sendMessage(reply #1)"}},
		{"silent, replace", config.RemindersSilent, true, []string{"sendMessage(reply #1) silent", "deleteMessage(#2), sendMessage(reply #1) silent"}},
		{"off, replace", config.RemindersOff, true, []string{"", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.proc.Reminders, e.proc.RemindersReplace = tc.mode, tc.replace
			e.handle(msg("a")) // message #1
			for i, want := range tc.reminders {
				got := callNames(e.handle(msg("a")))
				if got != want {
					t.Errorf("reminder %d: calls [%s], want [%s]", i+1, got, want)
				}
			}
			if inc, _ := e.store.Incident(msg().GroupKey); tc.replace && tc.mode != config.RemindersOff && inc.ReminderMessageID != 3 {
				t.Errorf("stored reminder #%d, want #3", inc.ReminderMessageID)
			}
		})
	}
}

// When Telegram refuses the delete (older than 48 hours, or deleted by
// hand), the new reminder is sent anyway.
func TestReminderReplaceDeleteFails(t *testing.T) {
	e := newEnv(t)
	e.proc.RemindersReplace = true
	e.tg.fail = func(c call, _ int) string {
		if c.Method == "deleteMessage" {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message can't be deleted"}`
		}
		return ""
	}
	e.handle(msg("a"))
	e.handle(msg("a"))
	if got := callNames(e.handle(msg("a"))); got != "deleteMessage(#2), sendMessage(reply #1)" {
		t.Errorf("calls [%s]", got)
	}
}

func callNames(calls []call) string {
	s := ""
	for i, c := range calls {
		if i > 0 {
			s += ", "
		}
		s += c.String()
	}
	return s
}

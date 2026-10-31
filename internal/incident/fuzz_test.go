package incident

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
)

// Whatever an admin replies, a custom silence is 1 minute to 7 days with a
// one-line reason of at most 200 characters.
func FuzzParseCustomSilence(f *testing.F) {
	for _, s := range []string{"3h kernel update", "90m", "7d", "8d", "tomorrow-ish", "3h\nrm -rf /", "0h", "9999d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, reason, ok := parseCustomSilence(s)
		if !ok {
			return
		}
		if d < time.Minute || d > config.MaxSilence || utf8.RuneCountInString(reason) > maxReasonRunes || strings.ContainsAny(reason, "\n\r\t") {
			t.Fatalf("parseCustomSilence(%q) = %v, %q", s, d, reason)
		}
	})
}

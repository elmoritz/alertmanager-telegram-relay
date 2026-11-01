package route

import (
	"slices"
	"testing"
)

// Parse never panics, and every rule it accepts targets an allowed chat.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`team="media" => -1001234567890/42`,
		"severity=critical, instance=~\"db-.*\" => -1001234567891;job!=\"x\" => -1001234567890",
		`a="\"" => -1001234567890`,
		`# comment`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		rules, err := Parse(s, allowed)
		if err != nil {
			return
		}
		r := Router{Rules: rules, Default: Target{ChatID: allowed[0]}}
		for _, rule := range rules {
			if !slices.Contains(allowed, rule.Target.ChatID) || rule.Target.ThreadID < 0 || len(rule.Matchers) == 0 {
				t.Fatalf("accepted rule %+v from %q", rule, s)
			}
		}
		_ = r.Pick(map[string]string{"team": "media", "severity": "critical"})
	})
}

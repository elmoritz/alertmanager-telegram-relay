package route

import (
	"strings"
	"testing"
)

var allowed = []int64{-1001234567890, -1001234567891}

func TestParseAndPick(t *testing.T) {
	rules, err := Parse(`
# media alerts go to their own topic
team="media" => -1001234567890/42
severity="critical", instance=~"db-.*" => -1001234567891;  job!="node", env!~"dev|test" => -1001234567891/7
service="semi;colon \"quoted\", comma" => -1001234567890/9
`, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("%d rules", len(rules))
	}
	r := Router{Rules: rules, Default: Target{ChatID: -1001234567890}}
	for _, tc := range []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"team": "media", "severity": "critical", "instance": "db-1"}, "-1001234567890/42"}, // first match wins
		{map[string]string{"severity": "critical", "instance": "db-1"}, "-1001234567891"},
		{map[string]string{"severity": "critical", "instance": "xdb-1", "job": "node"}, "-1001234567890"}, // anchored regex
		{map[string]string{"job": "blackbox", "env": "prod"}, "-1001234567891/7"},
		{map[string]string{"job": "blackbox", "env": "dev"}, "-1001234567890"},
		{map[string]string{"job": "node"}, "-1001234567890"},
		{map[string]string{"env": "prod"}, "-1001234567891/7"}, // a missing label is "", which is != "node"
		{map[string]string{"service": `semi;colon "quoted", comma`, "job": "node"}, "-1001234567890/9"},
		{nil, "-1001234567891/7"},
	} {
		if got := r.Pick(tc.labels).String(); got != tc.want {
			t.Errorf("Pick(%v) = %s, want %s", tc.labels, got, tc.want)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	rules, err := Parse(" \n# nothing\n;\n", allowed)
	if err != nil || len(rules) != 0 {
		t.Errorf("rules %v, err %v", rules, err)
	}
}

func TestParseErrors(t *testing.T) {
	for name, s := range map[string]string{
		"no arrow":            `team="media" -1001234567890`,
		"no matchers":         `=> -1001234567890`,
		"chat not allowed":    `team="media" => -1009999999999`,
		"chat not a number":   `team="media" => media`,
		"topic not positive":  `team="media" => -1001234567890/0`,
		"topic not a number":  `team="media" => -1001234567890/x`,
		"bad label name":      `te-am="media" => -1001234567890`,
		"no operator":         `team => -1001234567890`,
		"unknown operator":    `team=="media" => -1001234567890`,
		"unterminated quote":  `team="media => -1001234567890`,
		"text after quote":    `team="media"x => -1001234567890`,
		"unknown escape":      `team="me\dia" => -1001234567890`,
		"bad regex":           `team=~"(" => -1001234567890`,
		"unquoted with space": `team=me dia => -1001234567890`,
		"value too long":      `team="` + strings.Repeat("x", maxValueLen+1) + `" => -1001234567890`,
		"too many rules":      strings.Repeat("team=\"a\" => -1001234567890\n", maxRules+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(s, allowed)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "media") || strings.Contains(err.Error(), "9999") {
				t.Errorf("error echoes the rule: %v", err)
			}
		})
	}
}

func TestBareValues(t *testing.T) {
	rules, err := Parse(`team=media, severity!=info => -1001234567890`, allowed)
	if err != nil {
		t.Fatal(err)
	}
	r := Router{Rules: rules, Default: Target{ChatID: -1001234567891}}
	if got := r.Pick(map[string]string{"team": "media", "severity": "critical"}); got.ChatID != -1001234567890 {
		t.Errorf("got %v", got)
	}
}

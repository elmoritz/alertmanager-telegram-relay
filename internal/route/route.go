// Package route picks the Telegram chat and forum topic for a notification
// (SPEC §7). Rules come from ROUTES: label matchers in Alertmanager syntax,
// then "=>" and the target, first match wins.
//
//	team="media" => -1001234567890/42
//	severity="critical", instance=~"db-.*" => -1001234567891
package route

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Target is where a notification goes: a chat and, in a forum, a topic.
type Target struct {
	ChatID   int64
	ThreadID int // message_thread_id; 0 for none
}

func (t Target) String() string {
	if t.ThreadID == 0 {
		return strconv.FormatInt(t.ChatID, 10)
	}
	return fmt.Sprintf("%d/%d", t.ChatID, t.ThreadID)
}

// Op is a matcher operator.
type Op string

const (
	Equal    Op = "="
	NotEqual Op = "!="
	Regex    Op = "=~"
	NotRegex Op = "!~"
)

// Limits keep a mistyped ROUTES from becoming expensive to evaluate.
const (
	maxRules    = 100
	maxRuleLen  = 1000
	maxMatchers = 20
	maxValueLen = 500
)

const (
	quoteChar  = '"'
	escapeChar = '\\'
	ruleArrow  = "=>"
)

// Matcher matches one label. A missing label matches as the empty string,
// as in Alertmanager. Regexes are anchored at both ends.
type Matcher struct {
	Name  string
	Op    Op
	Value string
	re    *regexp.Regexp
}

// Matches reports whether labels satisfy m.
func (m Matcher) Matches(labels map[string]string) bool {
	v := labels[m.Name]
	switch m.Op {
	case Equal:
		return v == m.Value
	case NotEqual:
		return v != m.Value
	case Regex:
		return m.re.MatchString(v)
	case NotRegex:
		return !m.re.MatchString(v)
	}
	return false
}

// Rule sends notifications whose labels match all Matchers to Target.
type Rule struct {
	Matchers []Matcher
	Target   Target
}

// Router holds the rules and the default target.
type Router struct {
	Rules   []Rule
	Default Target
}

// Pick returns the target of the first rule whose matchers all match labels,
// or the default.
func (r Router) Pick(labels map[string]string) Target {
	for _, rule := range r.Rules {
		if all(rule.Matchers, labels) {
			return rule.Target
		}
	}
	return r.Default
}

func all(ms []Matcher, labels map[string]string) bool {
	for _, m := range ms {
		if !m.Matches(labels) {
			return false
		}
	}
	return true
}

// Parse reads ROUTES: rules separated by newlines or ";" (outside quotes).
// Every target chat must be in allowed. Errors name the rule by number, never
// echo its text.
func Parse(s string, allowed []int64) ([]Rule, error) {
	var rules []Rule
	for i, text := range splitOutsideQuotes(s, "\n;") {
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if len(rules) == maxRules {
			return nil, fmt.Errorf("more than %d rules", maxRules)
		}
		r, err := parseRule(text, allowed)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func parseRule(s string, allowed []int64) (Rule, error) {
	if len(s) > maxRuleLen {
		return Rule{}, fmt.Errorf("longer than %d characters", maxRuleLen)
	}
	left, right, ok := cutArrow(s)
	if !ok {
		return Rule{}, errors.New(`missing "=>"`)
	}
	target, err := parseTarget(strings.TrimSpace(right), allowed)
	if err != nil {
		return Rule{}, err
	}
	var ms []Matcher
	for _, text := range splitOutsideQuotes(left, ",") {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		m, err := parseMatcher(text)
		if err != nil {
			return Rule{}, err
		}
		ms = append(ms, m)
	}
	if len(ms) == 0 {
		return Rule{}, errors.New("no matchers")
	}
	if len(ms) > maxMatchers {
		return Rule{}, fmt.Errorf("more than %d matchers", maxMatchers)
	}
	return Rule{Matchers: ms, Target: target}, nil
}

// cutArrow splits s at the first "=>" outside quotes.
func cutArrow(s string) (left, right string, ok bool) {
	inQuotes, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inQuotes && c == escapeChar:
			escaped = true
		case c == quoteChar:
			inQuotes = !inQuotes
		case !inQuotes && strings.HasPrefix(s[i:], ruleArrow):
			return s[:i], s[i+len(ruleArrow):], true
		}
	}
	return "", "", false
}

var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func parseMatcher(s string) (Matcher, error) {
	i := strings.IndexAny(s, "=!")
	if i <= 0 {
		return Matcher{}, errors.New("matcher without operator")
	}
	m := Matcher{Name: strings.TrimSpace(s[:i])}
	if !labelNameRE.MatchString(m.Name) {
		return Matcher{}, errors.New("invalid label name")
	}
	rest := s[i:]
	for _, op := range []Op{Regex, NotRegex, NotEqual, Equal} {
		if strings.HasPrefix(rest, string(op)) {
			m.Op, rest = op, rest[len(op):]
			break
		}
	}
	if m.Op == "" {
		return Matcher{}, errors.New("unknown operator")
	}
	v, err := parseValue(strings.TrimSpace(rest))
	if err != nil {
		return Matcher{}, err
	}
	if len(v) > maxValueLen {
		return Matcher{}, fmt.Errorf("value longer than %d characters", maxValueLen)
	}
	m.Value = v
	if m.Op == Regex || m.Op == NotRegex {
		if m.re, err = regexp.Compile("^(?:" + v + ")$"); err != nil {
			return Matcher{}, errors.New("invalid regular expression")
		}
	}
	return m, nil
}

// parseValue reads a double-quoted value with \" \\ \n escapes, or a bare
// word without quotes, spaces, commas or semicolons.
func parseValue(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		if s == "" || strings.ContainsAny(s, "\" \t,;\\") {
			return "", errors.New("value must be quoted")
		}
		return s, nil
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			switch c {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\':
				b.WriteByte(c)
			default:
				return "", errors.New("unknown escape in value")
			}
			escaped = false
		case c == escapeChar:
			escaped = true
		case c == quoteChar:
			if strings.TrimSpace(s[i+1:]) != "" {
				return "", errors.New("text after closing quote")
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated quote")
}

func parseTarget(s string, allowed []int64) (Target, error) {
	chat, topic, hasTopic := strings.Cut(s, "/")
	var t Target
	var err error
	if t.ChatID, err = strconv.ParseInt(strings.TrimSpace(chat), 10, 64); err != nil {
		return Target{}, errors.New("target chat is not a number")
	}
	if !slices.Contains(allowed, t.ChatID) {
		return Target{}, errors.New("target chat is not in TELEGRAM_CHAT_IDS")
	}
	if hasTopic {
		if t.ThreadID, err = strconv.Atoi(strings.TrimSpace(topic)); err != nil || t.ThreadID <= 0 {
			return Target{}, errors.New("topic must be a positive number")
		}
	}
	return t, nil
}

// splitOutsideQuotes splits s at any of seps that is not inside a quoted
// value.
func splitOutsideQuotes(s, seps string) []string {
	var out []string
	inQuotes, escaped, start := false, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inQuotes && c == escapeChar:
			escaped = true
		case c == quoteChar:
			inQuotes = !inQuotes
		case !inQuotes && strings.IndexByte(seps, c) >= 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

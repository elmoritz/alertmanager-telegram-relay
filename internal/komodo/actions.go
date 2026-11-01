// Package komodo runs predefined remediation actions through Komodo's API
// (SPEC §5, SECURITY.md §9). Actions come only from the operator's
// REMEDIATION_FILE: a fixed operation on a fixed target, offered on alerts
// whose group labels match. Nothing from an alert or a chat goes into a
// request.
package komodo

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Operations the relay can run: restart or redeploy a stack, restart a
// container. Nothing that deletes, prunes or changes configuration.
const (
	RestartStack     = "RestartStack"
	DeployStack      = "DeployStack"
	RestartContainer = "RestartContainer"
)

// Defaults and limits for actions.
const (
	DefaultCooldown = 10 * time.Minute
	MinCooldown     = time.Minute
	maxActions      = 50
	maxLabelRunes   = 40
)

// Action is one predefined remediation action.
type Action struct {
	ID        string
	Label     string // button text, e.g. "Restart stack media"
	Operation string
	Stack     string   // RestartStack, DeployStack
	Services  []string // optional filter within the stack
	Server    string   // RestartContainer
	Container string   // RestartContainer
	// Match: exact values of group labels; all must match. Never empty.
	Match    map[string]string
	Cooldown time.Duration
}

// Offered reports whether a is offered on an incident with these group
// labels.
func (a Action) Offered(groupLabels map[string]string) bool {
	for k, v := range a.Match {
		if groupLabels[k] != v {
			return false
		}
	}
	return len(a.Match) > 0
}

var (
	idRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	targetRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	labelRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type fileAction struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Operation string            `json:"operation"`
	Stack     string            `json:"stack"`
	Services  []string          `json:"services"`
	Server    string            `json:"server"`
	Container string            `json:"container"`
	Match     map[string]string `json:"match"`
	Cooldown  string            `json:"cooldown"`
}

// ParseActions reads REMEDIATION_FILE's content: {"actions": [...]}.
// Errors name the action by number and the field, never a value.
func ParseActions(data []byte) ([]Action, error) {
	var f struct {
		Actions []fileAction `json:"actions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("not valid JSON of the form {\"actions\": [...]} (unknown fields are refused)")
	}
	if len(f.Actions) == 0 {
		return nil, errors.New("no actions")
	}
	if len(f.Actions) > maxActions {
		return nil, fmt.Errorf("more than %d actions", maxActions)
	}
	seen := map[string]bool{}
	var out []Action
	for i, fa := range f.Actions {
		a, err := fa.action()
		if err != nil {
			return nil, fmt.Errorf("action %d: %w", i+1, err)
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("action %d: duplicate id", i+1)
		}
		seen[a.ID] = true
		out = append(out, a)
	}
	return out, nil
}

func (fa fileAction) action() (Action, error) {
	a := Action{ID: fa.ID, Label: strings.TrimSpace(fa.Label), Operation: fa.Operation, Stack: fa.Stack, Services: fa.Services,
		Server: fa.Server, Container: fa.Container, Match: fa.Match, Cooldown: DefaultCooldown}
	switch {
	case !idRE.MatchString(a.ID):
		return Action{}, errors.New("id must be 1-32 of a-z, 0-9 and -")
	case a.Label == "" || len([]rune(a.Label)) > maxLabelRunes || strings.ContainsAny(a.Label, "\n\r\t"):
		return Action{}, fmt.Errorf("label must be 1-%d characters on one line", maxLabelRunes)
	case len(a.Match) == 0:
		return Action{}, errors.New("match needs at least one label, or the action would be offered on every alert")
	}
	for k, v := range a.Match {
		if !labelRE.MatchString(k) || v == "" {
			return Action{}, errors.New("match needs label names and non-empty values")
		}
	}
	switch a.Operation {
	case RestartStack, DeployStack:
		if !targetRE.MatchString(a.Stack) || a.Server != "" || a.Container != "" {
			return Action{}, errors.New("a stack operation needs stack (and no server or container)")
		}
		for _, s := range a.Services {
			if !targetRE.MatchString(s) {
				return Action{}, errors.New("services must be service names")
			}
		}
	case RestartContainer:
		if !targetRE.MatchString(a.Server) || !targetRE.MatchString(a.Container) || a.Stack != "" || len(a.Services) > 0 {
			return Action{}, errors.New("RestartContainer needs server and container (and no stack or services)")
		}
	default:
		return Action{}, errors.New("operation must be RestartStack, DeployStack or RestartContainer")
	}
	if fa.Cooldown != "" {
		d, err := time.ParseDuration(fa.Cooldown)
		if err != nil || d < MinCooldown {
			return Action{}, fmt.Errorf("cooldown must be a duration of at least %s", MinCooldown)
		}
		a.Cooldown = d
	}
	return a, nil
}

// params is the request body for a's operation.
func (a Action) params() any {
	switch a.Operation {
	case RestartContainer:
		return struct {
			Server    string `json:"server"`
			Container string `json:"container"`
		}{a.Server, a.Container}
	default:
		services := a.Services
		if services == nil {
			services = []string{}
		}
		return struct {
			Stack    string   `json:"stack"`
			Services []string `json:"services"`
		}{a.Stack, services}
	}
}

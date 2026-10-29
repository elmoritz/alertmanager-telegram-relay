package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Client calls Alertmanager's v2 silences API. Credentials, if any, travel in
// the Authorization header, never in the URL, so they cannot end up in errors.
type Client struct {
	base     *url.URL
	username string
	password string
	token    string
	http     *http.Client

	// OnCall, if set, is called after every request with the operation
	// ("create_silence", "expire_silence", "list_alerts") and "ok" or "error".
	OnCall func(op, result string)
}

// NewClient returns a client for the Alertmanager at base (which may carry a
// route prefix), with basic auth, bearer auth or none.
func NewClient(base *url.URL, username, password, token string) *Client {
	return &Client{base: base, username: username, password: password, token: token, http: &http.Client{Timeout: 10 * time.Second}}
}

// Matcher is a silence matcher.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// Silence is a silence to create.
type Silence struct {
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

// ErrUnsafeSilence is returned for a silence the relay must never create.
var ErrUnsafeSilence = errors.New("refusing unsafe silence")

var silenceIDRE = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// CreateSilence creates s and returns its ID. Only exact-equality matchers
// are allowed, and at least one (SECURITY.md §3); this is checked here too,
// whatever the caller did.
func (c *Client) CreateSilence(ctx context.Context, s Silence) (string, error) {
	if len(s.Matchers) == 0 {
		return "", fmt.Errorf("%w: no matchers", ErrUnsafeSilence)
	}
	for _, m := range s.Matchers {
		if m.IsRegex || !m.IsEqual || m.Name == "" {
			return "", fmt.Errorf("%w: only exact, non-empty matchers", ErrUnsafeSilence)
		}
	}
	if !s.EndsAt.After(s.StartsAt) {
		return "", fmt.Errorf("%w: ends before it starts", ErrUnsafeSilence)
	}
	var res struct {
		SilenceID string `json:"silenceID"`
	}
	if err := c.do(ctx, "create_silence", http.MethodPost, c.base.JoinPath("api/v2/silences"), s, &res); err != nil {
		return "", err
	}
	if !silenceIDRE.MatchString(res.SilenceID) {
		return "", errors.New("alertmanager create_silence: unexpected silence ID in response")
	}
	return res.SilenceID, nil
}

// ErrSilenceNotFound is returned when expiring a silence Alertmanager does
// not know (any more).
var ErrSilenceNotFound = errors.New("silence not found")

// ExpireSilence ends the silence with the given ID now.
func (c *Client) ExpireSilence(ctx context.Context, id string) error {
	if !silenceIDRE.MatchString(id) {
		return errors.New("invalid silence ID")
	}
	return c.do(ctx, "expire_silence", http.MethodDelete, c.base.JoinPath("api/v2/silence", id), nil, nil)
}

// ActiveAlerts returns how many unresolved alerts Alertmanager has whose
// labels include all of labels, silenced and inhibited ones included.
func (c *Client) ActiveAlerts(ctx context.Context, labels map[string]string) (int, error) {
	if len(labels) == 0 {
		return 0, errors.New("alertmanager list_alerts: no labels to match")
	}
	u := c.base.JoinPath("api/v2/alerts")
	q := url.Values{"active": {"true"}, "silenced": {"true"}, "inhibited": {"true"}, "unprocessed": {"true"}}
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		q.Add("filter", name+"="+quoteMatcherValue(labels[name]))
	}
	u.RawQuery = q.Encode()
	var alerts []json.RawMessage
	if err := c.do(ctx, "list_alerts", http.MethodGet, u, nil, &alerts); err != nil {
		return 0, err
	}
	return len(alerts), nil
}

// quoteMatcherValue quotes a label value for an Alertmanager matcher, with
// the escapes its parser understands.
func quoteMatcherValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}

func (c *Client) do(ctx context.Context, op, method string, u *url.URL, body, result any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return fmt.Errorf("alertmanager %s: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.username != "":
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.observe(op, "error")
		return fmt.Errorf("alertmanager %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && method == http.MethodDelete {
		c.observe(op, "error")
		return ErrSilenceNotFound
	}
	if resp.StatusCode/100 != 2 {
		c.observe(op, "error")
		// The response body is not included: it may quote matchers (label values).
		return fmt.Errorf("alertmanager %s: %s", op, resp.Status)
	}
	c.observe(op, "ok")
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
			return fmt.Errorf("alertmanager %s: decoding response: %w", op, err)
		}
	}
	return nil
}

func (c *Client) observe(op, result string) {
	if c.OnCall != nil {
		c.OnCall(op, result)
	}
}

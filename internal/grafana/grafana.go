// Package grafana renders dashboard panels as PNG images through Grafana's
// /render endpoint (SPEC §8). It needs Grafana's image renderer.
//
// The service-account token is sent only to the render base (GRAFANA_RENDER_URL,
// default GRAFANA_URL): a panel URL comes from an alert annotation, so it must
// point at GRAFANA_URL, and the relay builds the render URL itself, taking
// over only the dashboard, the panel and a few known query parameters
// (SECURITY.md §6).
package grafana

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Image size and limits.
const (
	width        = 1000
	height       = 500
	maxImageSize = 5 << 20
	timeout      = 30 * time.Second
)

// ErrNotAPanel is returned for a panel URL the relay cannot render.
var ErrNotAPanel = errors.New("not a Grafana panel URL")

// Client renders panels from one Grafana.
type Client struct {
	base   *url.URL // where panel links point (GRAFANA_URL)
	render *url.URL // where the relay renders (GRAFANA_RENDER_URL)
	token  string
	tz     string
	http   *http.Client

	// OnCall, if set, is called after every render with "ok" or "error".
	OnCall func(result string)
}

// New returns a client for the Grafana whose panel links point at base
// (GRAFANA_URL) and which the relay reaches at render (GRAFANA_RENDER_URL;
// nil: base), with a service-account token; tz is the IANA zone for the
// panel's time axis.
func New(base, render *url.URL, token, tz string) *Client {
	if render == nil {
		render = base
	}
	return &Client{base: base, render: render, token: token, tz: tz, http: &http.Client{
		Timeout: timeout,
		// The token goes to the render URL only. Grafana answers a bad token
		// with a redirect to its login page; that is an error, not a page to
		// follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var (
	dashboardPathRE = regexp.MustCompile(`^/d(?:-solo)?/([A-Za-z0-9_-]{1,40})(?:/([A-Za-z0-9_-]{1,100}))?/?$`)
	panelIDRE       = regexp.MustCompile(`^[0-9]{1,9}$`)
	timeRE          = regexp.MustCompile(`^(now(-[0-9]{1,4}[smhdwMy])?|[0-9]{10,13})$`)
	varNameRE       = regexp.MustCompile(`^var-[A-Za-z0-9_]{1,64}$`)
)

// RenderURL builds the render URL for panelURL, a Grafana panel link
// (".../d/<uid>/<slug>?viewPanel=<id>" or ".../d-solo/<uid>/<slug>?panelId=<id>").
// panelURL must point at the same Grafana as GRAFANA_URL.
func (c *Client) RenderURL(panelURL string) (*url.URL, error) {
	u, err := url.Parse(panelURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrNotAPanel
	}
	if !strings.EqualFold(u.Scheme, c.base.Scheme) || !strings.EqualFold(u.Host, c.base.Host) {
		return nil, fmt.Errorf("%w: not on GRAFANA_URL", ErrNotAPanel)
	}
	path := strings.TrimPrefix(u.Path, strings.TrimSuffix(c.base.Path, "/"))
	m := dashboardPathRE.FindStringSubmatch(path)
	if m == nil {
		return nil, ErrNotAPanel
	}
	q := u.Query()
	panel := q.Get("viewPanel")
	if panel == "" {
		panel = q.Get("panelId")
	}
	panel = strings.TrimPrefix(panel, "panel-")
	if !panelIDRE.MatchString(panel) {
		return nil, fmt.Errorf("%w: no panel", ErrNotAPanel)
	}

	out := url.Values{}
	out.Set("panelId", panel)
	out.Set("width", strconv.Itoa(width))
	out.Set("height", strconv.Itoa(height))
	out.Set("from", "now-3h")
	out.Set("to", "now")
	if c.tz != "" {
		out.Set("tz", c.tz)
	}
	if v := q.Get("orgId"); panelIDRE.MatchString(v) {
		out.Set("orgId", v)
	}
	for _, k := range []string{"from", "to"} {
		if v := q.Get(k); timeRE.MatchString(v) {
			out.Set(k, v)
		}
	}
	for k, vs := range q {
		if varNameRE.MatchString(k) {
			for _, v := range vs {
				if len(v) <= 200 {
					out.Add(k, v)
				}
			}
		}
	}
	slug := m[2]
	if slug == "" {
		slug = "panel"
	}
	r := c.render.JoinPath("render", "d-solo", m[1], slug)
	r.RawQuery = out.Encode()
	return r, nil
}

// RenderPanel returns the panel at panelURL as a PNG image. Errors never
// contain the URL (it may carry label values) or the token.
func (c *Client) RenderPanel(ctx context.Context, panelURL string) ([]byte, error) {
	u, err := c.RenderURL(panelURL)
	if err != nil {
		return nil, err
	}
	img, err := c.fetch(ctx, u)
	result := "ok"
	if err != nil {
		result = "error"
	}
	if c.OnCall != nil {
		c.OnCall(result)
	}
	return img, err
}

func (c *Client) fetch(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("grafana render: building request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return nil, fmt.Errorf("grafana render: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Grafana answers 500 with a text body when no renderer is installed;
		// the body is not included, it may quote the panel's query.
		return nil, fmt.Errorf("grafana render: %s", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		return nil, errors.New("grafana render: answer is not a PNG image")
	}
	img, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return nil, fmt.Errorf("grafana render: reading image: %w", err)
	}
	if len(img) > maxImageSize {
		return nil, errors.New("grafana render: image too large")
	}
	return img, nil
}

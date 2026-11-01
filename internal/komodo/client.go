package komodo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client calls Komodo Core's API with an API key and secret. They travel in
// headers, never in URLs, and no error carries a response body: Komodo's
// logs can describe the server in detail.
type Client struct {
	base        *url.URL
	key, secret string
	http        *http.Client
	// PollEvery is how often Wait asks for the result.
	PollEvery time.Duration
}

// New returns a client for the Komodo Core at base (KOMODO_URL).
func New(base *url.URL, key, secret string) *Client {
	return &Client{base: base, key: key, secret: secret, PollEvery: 3 * time.Second, http: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// update is the part of Komodo's Update the relay uses.
type update struct {
	ID struct {
		OID string `json:"$oid"`
	} `json:"_id"`
	Status  string `json:"status"` // Queued, InProgress, Complete
	Success bool   `json:"success"`
}

// Start runs a's operation and returns the ID of Komodo's update for it.
func (c *Client) Start(ctx context.Context, a Action) (string, error) {
	var u update
	if err := c.post(ctx, "execute", a.Operation, a.params(), &u); err != nil {
		return "", err
	}
	if u.ID.OID == "" {
		return "", errors.New("komodo: no update ID in the answer")
	}
	return u.ID.OID, nil
}

// Wait polls the update until it is complete and reports whether it
// succeeded.
func (c *Client) Wait(ctx context.Context, updateID string) (bool, error) {
	for {
		var u update
		if err := c.post(ctx, "read", "GetUpdate", struct {
			ID string `json:"id"`
		}{updateID}, &u); err != nil {
			return false, err
		}
		if u.Status == "Complete" {
			return u.Success, nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("komodo: waiting for the result: %w", ctx.Err())
		case <-time.After(c.PollEvery):
		}
	}
}

func (c *Client) post(ctx context.Context, module, request string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.JoinPath(module, request).String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("komodo %s: building request", request)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("X-Api-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return fmt.Errorf("komodo %s: %w", request, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("komodo %s: %s", request, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		return fmt.Errorf("komodo %s: unreadable answer", request)
	}
	return nil
}

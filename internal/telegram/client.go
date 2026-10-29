// Package telegram is the relay's only Bot API client. It is the only code that
// knows the bot token, and it removes the token from every error it returns
// (SECURITY.md §1): the token is part of every request URL, and net/http puts
// that URL into its errors.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is the public Bot API endpoint.
const DefaultBaseURL = "https://api.telegram.org"

const maxResponseBytes = 1 << 20

// Client calls the Bot API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// OnCall, if set, is called after every request with the method name and
	// a bounded result: "ok", "error", "ratelimited" or "network".
	OnCall func(method, result string)

	// ProtectContent sends every message with protect_content, so it cannot
	// be forwarded or saved.
	ProtectContent bool
}

// New returns a client for the given API base URL (DefaultBaseURL, or a test
// server) and bot token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Error is returned for every failed call. Its text never contains the token.
type Error struct {
	Method string
	// Code is the Bot API error_code (usually the HTTP status), or 0 when the
	// request did not get an API answer (network error, timeout).
	Code        int
	Description string
	RetryAfter  time.Duration

	cause error // only context.Canceled / context.DeadlineExceeded, never the raw error
}

func (e *Error) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("telegram %s: %s", e.Method, e.Description)
	}
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Unwrap exposes context cancellation only. The original transport error is
// deliberately dropped, because its text contains the request URL.
func (e *Error) Unwrap() error { return e.cause }

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return &Error{Method: method, Description: "encoding request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return &Error{Method: method, Description: c.redact(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		c.observe(method, "network")
		e := &Error{Method: method, Description: c.transportError(req, err)}
		switch {
		case errors.Is(err, context.Canceled):
			e.cause = context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			e.cause = context.DeadlineExceeded
		}
		return e
	}
	defer resp.Body.Close()

	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&r); err != nil {
		c.observe(method, "error")
		// A non-JSON answer (proxy error page, 502): report the status only.
		return &Error{Method: method, Code: resp.StatusCode, Description: "unreadable response: " + http.StatusText(resp.StatusCode)}
	}
	if !r.OK {
		e := &Error{Method: method, Code: r.ErrorCode, Description: c.redact(stripQuoted(r.Description))}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		if r.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(r.Parameters.RetryAfter) * time.Second
		}
		if e.Code == http.StatusTooManyRequests {
			c.observe(method, "ratelimited")
		} else {
			c.observe(method, "error")
		}
		return e
	}
	c.observe(method, "ok")
	if result != nil {
		if err := json.Unmarshal(r.Result, result); err != nil {
			return &Error{Method: method, Code: resp.StatusCode, Description: "decoding result: " + err.Error()}
		}
	}
	return nil
}

func (c *Client) observe(method, result string) {
	if c.OnCall != nil {
		c.OnCall(method, result)
	}
}

// transportError describes a failed request without its URL: *url.Error
// embeds the full URL, token included, so only its operation and cause are
// kept, and anything token-shaped left in the cause is redacted as well.
func (c *Client) transportError(req *http.Request, err error) string {
	msg := err.Error()
	if ue, ok := errors.AsType[*url.Error](err); ok {
		msg = ue.Op + ": " + ue.Err.Error()
	}
	for _, u := range []string{req.URL.String(), req.URL.EscapedPath(), req.URL.Path} {
		msg = strings.ReplaceAll(msg, u, "<url>")
	}
	return c.redact(msg)
}

// quotedRE matches single-quoted values in Bot API error descriptions, which
// can be alert content: a rejected button is reported as "inline keyboard
// button URL '<url>' is invalid".
var quotedRE = regexp.MustCompile(`'[^']*'`)

// stripQuoted removes quoted values from an error description, so logging an
// error does not log alert content.
func stripQuoted(s string) string {
	return quotedRE.ReplaceAllString(s, "'…'")
}

// redact removes the token from s in every encoding a URL may carry it in.
func (c *Client) redact(s string) string {
	if c.token == "" {
		return s
	}
	for _, t := range []string{c.token, url.PathEscape(c.token), url.QueryEscape(c.token)} {
		s = strings.ReplaceAll(s, t, "<token>")
	}
	return s
}

// User is the subset of the Bot API User object the relay uses.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// GetMe returns the bot's own user. Useful as a startup check of the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// Message is a text message to send. Text is Telegram HTML.
type Message struct {
	ChatID int64
	Text   string
	// ReplyTo, if non-zero, makes the message a reply. If the original is
	// gone, the message is sent anyway, without the reply.
	ReplyTo int
	// Silent sends without a notification sound.
	Silent bool
	// Keyboard, if not nil, is attached as an inline keyboard.
	Keyboard *Keyboard
}

// Keyboard is an inline keyboard: rows of buttons.
type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

// Button styles (Bot API 9.4); no style is the client's default look.
const (
	StyleDanger  = "danger"
	StyleSuccess = "success"
	StylePrimary = "primary"
)

// Button is an inline keyboard button: a callback button (Data), a link
// (URL), a copy-to-clipboard button (CopyText) or, if Disabled, a label that
// does nothing.
type Button struct {
	Text     string
	Data     string
	URL      string
	CopyText string
	Style    string
	// Disabled shows the button greyed out and inert. It is sent without
	// Data, URL or CopyText: Telegram silently drops "disabled" from a button
	// that also has an action, which would make it pressable.
	Disabled bool
}

type copyText struct {
	Text string `json:"text"`
}

type buttonJSON struct {
	Text     string    `json:"text"`
	Style    string    `json:"style,omitempty"`
	Data     string    `json:"callback_data,omitempty"`
	URL      string    `json:"url,omitempty"`
	CopyText *copyText `json:"copy_text,omitempty"`
	Disabled *struct{} `json:"disabled,omitempty"`
}

// MarshalJSON encodes b as an InlineKeyboardButton.
func (b Button) MarshalJSON() ([]byte, error) {
	j := buttonJSON{Text: b.Text, Style: b.Style}
	switch {
	case b.Disabled:
		j.Disabled = &struct{}{}
	default:
		j.Data, j.URL = b.Data, b.URL
		if b.CopyText != "" {
			j.CopyText = &copyText{b.CopyText}
		}
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes an InlineKeyboardButton, for tests and fakes.
func (b *Button) UnmarshalJSON(data []byte) error {
	var j buttonJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	*b = Button{Text: j.Text, Style: j.Style, Data: j.Data, URL: j.URL, Disabled: j.Disabled != nil}
	if j.CopyText != nil {
		b.CopyText = j.CopyText.Text
	}
	return nil
}

// noKeyboard removes a message's inline keyboard when sent with an edit.
var noKeyboard = &Keyboard{Rows: [][]Button{}}

type replyParameters struct {
	MessageID                int  `json:"message_id"`
	AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
}

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

// SendMessage sends m and returns the new message's ID.
func (c *Client) SendMessage(ctx context.Context, m Message) (int, error) {
	params := struct {
		ChatID              int64              `json:"chat_id"`
		Text                string             `json:"text"`
		ParseMode           string             `json:"parse_mode"`
		LinkPreviewOptions  linkPreviewOptions `json:"link_preview_options"`
		DisableNotification bool               `json:"disable_notification,omitempty"`
		ProtectContent      bool               `json:"protect_content,omitempty"`
		ReplyParameters     *replyParameters   `json:"reply_parameters,omitempty"`
		ReplyMarkup         *Keyboard          `json:"reply_markup,omitempty"`
	}{
		ChatID:              m.ChatID,
		Text:                m.Text,
		ParseMode:           "HTML",
		LinkPreviewOptions:  linkPreviewOptions{IsDisabled: true},
		DisableNotification: m.Silent,
		ProtectContent:      c.ProtectContent,
		ReplyMarkup:         m.Keyboard,
	}
	if m.ReplyTo != 0 {
		params.ReplyParameters = &replyParameters{MessageID: m.ReplyTo, AllowSendingWithoutReply: true}
	}
	var res struct {
		MessageID int `json:"message_id"`
	}
	if err := c.call(ctx, "sendMessage", params, &res); err != nil {
		return 0, err
	}
	return res.MessageID, nil
}

// EditMessageText replaces the text and keyboard of a message the bot sent; a
// nil keyboard removes the existing one. Editing to the identical content is
// not an error.
func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text string, kb *Keyboard) error {
	if kb == nil {
		kb = noKeyboard
	}
	params := struct {
		ChatID             int64              `json:"chat_id"`
		MessageID          int                `json:"message_id"`
		Text               string             `json:"text"`
		ParseMode          string             `json:"parse_mode"`
		LinkPreviewOptions linkPreviewOptions `json:"link_preview_options"`
		ReplyMarkup        *Keyboard          `json:"reply_markup"`
	}{chatID, messageID, text, "HTML", linkPreviewOptions{IsDisabled: true}, kb}
	err := c.call(ctx, "editMessageText", params, nil)
	if hasDescription(err, "message is not modified") {
		return nil
	}
	return err
}

// Update is an incoming update. The relay asks only for callback queries.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// CallbackQuery is a press of a callback button. Data is whatever the client
// sent: clients can send any data, not only that of an existing button.
type CallbackQuery struct {
	ID      string `json:"id"`
	From    User   `json:"from"`
	Message *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

// GetUpdates long-polls for callback queries after offset. It never uses or
// sets a webhook: the relay needs no inbound connection (SECURITY.md §2).
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	params := struct {
		Offset         int64    `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, int(timeout.Seconds()), []string{"callback_query"}}
	var updates []Update
	err := c.call(ctx, "getUpdates", params, &updates)
	return updates, err
}

// AnswerCallbackQuery stops the client's progress indicator, showing text as
// a short notification if it is not empty.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	params := struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text,omitempty"`
	}{id, text}
	return c.call(ctx, "answerCallbackQuery", params, nil)
}

// IsRetryable reports whether a failed call may succeed when repeated:
// network errors, rate limiting and server errors. Context cancellation is not.
func IsRetryable(err error) bool {
	var e *Error
	if !errors.As(err, &e) || e.cause != nil {
		return false
	}
	return e.Code == 0 || e.Code == http.StatusTooManyRequests || e.Code >= 500
}

// RetryAfter returns the wait Telegram asked for with a 429, or 0.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// IsMessageGone reports whether an edit failed because the message can no
// longer be edited (deleted, too old, or otherwise not editable).
func IsMessageGone(err error) bool {
	return hasDescription(err, "message to edit not found") ||
		hasDescription(err, "message can't be edited") ||
		hasDescription(err, "MESSAGE_ID_INVALID")
}

// IsFormatError reports whether Telegram rejected the text itself: HTML it
// could not parse, or a message over the length limit.
func IsFormatError(err error) bool {
	return hasDescription(err, "can't parse entities") ||
		hasDescription(err, "is too long") ||
		hasDescription(err, "text must be non-empty")
}

// IsButtonError reports whether Telegram rejected a keyboard button, usually a
// URL it does not accept (e.g. an internal host name without a dot) or a copy
// text over its length limit.
func IsButtonError(err error) bool {
	return hasDescription(err, "button url") ||
		hasDescription(err, "BUTTON_URL_INVALID") ||
		hasDescription(err, "wrong http url") ||
		hasDescription(err, "BUTTON_DATA_INVALID") ||
		hasDescription(err, "BUTTON_COPY_TEXT_INVALID") ||
		hasDescription(err, "can't parse InlineKeyboardButton")
}

func hasDescription(err error, substr string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(e.Description), strings.ToLower(substr))
}

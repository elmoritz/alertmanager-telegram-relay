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
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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
	// MigrateToChatID is the new ID of a group that was upgraded to a
	// supergroup (e.g. when topics were turned on), or 0.
	MigrateToChatID int64

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
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return &Error{Method: method, Description: "encoding request: " + err.Error()}
	}
	return c.post(ctx, method, "application/json", body, result)
}

// post sends one Bot API request with an encoded body.
func (c *Client) post(ctx context.Context, method, contentType string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return &Error{Method: method, Description: c.redact(err.Error())}
	}
	req.Header.Set("Content-Type", contentType)

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
		e.MigrateToChatID = r.Parameters.MigrateToChatID
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
	// ThreadID, if non-zero, sends into that forum topic.
	ThreadID int
	Text     string
	// ReplyTo, if non-zero, makes the message a reply. If the original is
	// gone, the message is sent anyway, without the reply.
	ReplyTo int
	// Silent sends without a notification sound.
	Silent bool
	// Keyboard, if not nil, is attached as an inline keyboard.
	Keyboard *Keyboard
	// ForceReply, if not empty, opens the reply field for the users
	// mentioned in Text (or the author of the message replied to), with
	// ForceReply as its placeholder. Not combined with Keyboard.
	ForceReply string
	// EphemeralTo, if not zero, sends the message so that only this user
	// (and the bot) sees it. It must be a member of the chat.
	EphemeralTo int64
}

type ephemeralParameters struct {
	ReceiverUserID int64 `json:"receiver_user_id"`
}

type forceReply struct {
	ForceReply  bool   `json:"force_reply"`
	Selective   bool   `json:"selective"`
	Placeholder string `json:"input_field_placeholder,omitempty"`
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
		ChatID              int64                `json:"chat_id"`
		MessageThreadID     int                  `json:"message_thread_id,omitempty"`
		Text                string               `json:"text"`
		ParseMode           string               `json:"parse_mode"`
		LinkPreviewOptions  linkPreviewOptions   `json:"link_preview_options"`
		DisableNotification bool                 `json:"disable_notification,omitempty"`
		ProtectContent      bool                 `json:"protect_content,omitempty"`
		Ephemeral           *ephemeralParameters `json:"ephemeral_message_parameters,omitempty"`
		ReplyParameters     *replyParameters     `json:"reply_parameters,omitempty"`
		ReplyMarkup         any                  `json:"reply_markup,omitempty"`
	}{
		ChatID:              m.ChatID,
		MessageThreadID:     m.ThreadID,
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
	if m.Keyboard == nil {
		params.ReplyMarkup = nil // a nil *Keyboard in an interface is not omitted
	}
	if m.ForceReply != "" {
		params.ReplyMarkup = forceReply{ForceReply: true, Selective: true, Placeholder: m.ForceReply}
	}
	if m.EphemeralTo != 0 {
		params.Ephemeral = &ephemeralParameters{ReceiverUserID: m.EphemeralTo}
	}
	var res struct {
		MessageID          int `json:"message_id"`
		EphemeralMessageID int `json:"ephemeral_message_id"`
	}
	if err := c.call(ctx, "sendMessage", params, &res); err != nil {
		return 0, err
	}
	if m.EphemeralTo != 0 {
		return res.EphemeralMessageID, nil
	}
	return res.MessageID, nil
}

// EditEphemeralMessageText replaces the text and keyboard of an ephemeral
// message sent to receiver; a nil keyboard removes it.
func (c *Client) EditEphemeralMessageText(ctx context.Context, chatID int64, ephemeralID int, receiver int64, text string, kb *Keyboard) error {
	if kb == nil {
		kb = noKeyboard
	}
	params := struct {
		ChatID             int64              `json:"chat_id"`
		EphemeralMessageID int                `json:"ephemeral_message_id"`
		ReceiverUserID     int64              `json:"receiver_user_id"`
		Text               string             `json:"text"`
		ParseMode          string             `json:"parse_mode"`
		LinkPreviewOptions linkPreviewOptions `json:"link_preview_options"`
		ReplyMarkup        *Keyboard          `json:"reply_markup"`
	}{chatID, ephemeralID, receiver, text, "HTML", linkPreviewOptions{IsDisabled: true}, kb}
	err := c.call(ctx, "editEphemeralMessageText", params, nil)
	if hasDescription(err, "message is not modified") {
		return nil
	}
	return err
}

// Photo is an image to send as a reply (graph images, SPEC §8).
type Photo struct {
	ChatID   int64
	ThreadID int
	ReplyTo  int
	Silent   bool
	PNG      []byte
}

// SendPhoto uploads a PNG image and returns the new message's ID.
func (c *Client) SendPhoto(ctx context.Context, p Photo) (int, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	field := func(name, value string) { _ = w.WriteField(name, value) }
	field("chat_id", strconv.FormatInt(p.ChatID, 10))
	if p.ThreadID != 0 {
		field("message_thread_id", strconv.Itoa(p.ThreadID))
	}
	if p.ReplyTo != 0 {
		rp, _ := json.Marshal(replyParameters{MessageID: p.ReplyTo, AllowSendingWithoutReply: true})
		field("reply_parameters", string(rp))
	}
	if p.Silent {
		field("disable_notification", "true")
	}
	if c.ProtectContent {
		field("protect_content", "true")
	}
	part, err := w.CreateFormFile("photo", "graph.png")
	if err == nil {
		_, err = part.Write(p.PNG)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return 0, &Error{Method: "sendPhoto", Description: "encoding request: " + err.Error()}
	}
	var res struct {
		MessageID int `json:"message_id"`
	}
	if err := c.post(ctx, "sendPhoto", w.FormDataContentType(), buf.Bytes(), &res); err != nil {
		return 0, err
	}
	return res.MessageID, nil
}

// Command is a bot command for SetCommands.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	// Ephemeral makes the command and its answer visible only to its author
	// and the bot.
	Ephemeral bool `json:"is_ephemeral,omitempty"`
}

// SetCommands sets the commands one user sees in one chat (scope
// chat_member), so only the relay's admins are offered them.
func (c *Client) SetCommands(ctx context.Context, chatID, userID int64, cmds []Command) error {
	params := struct {
		Commands []Command `json:"commands"`
		Scope    struct {
			Type   string `json:"type"`
			ChatID int64  `json:"chat_id"`
			UserID int64  `json:"user_id"`
		} `json:"scope"`
	}{Commands: cmds}
	params.Scope.Type, params.Scope.ChatID, params.Scope.UserID = "chat_member", chatID, userID
	return c.call(ctx, "setMyCommands", params, nil)
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

// Update is an incoming update. The relay asks only for callback queries,
// reactions and messages (for replies to its own messages).
type Update struct {
	UpdateID        int64            `json:"update_id"`
	CallbackQuery   *CallbackQuery   `json:"callback_query"`
	MessageReaction *MessageReaction `json:"message_reaction"`
	Message         *IncomingMessage `json:"message"`
}

// IncomingMessage is the subset of a received message the relay uses. As a
// chat administrator, a bot receives every message in a group; the relay
// acts only on replies to its own messages and drops the rest unlogged.
type IncomingMessage struct {
	MessageID int `json:"message_id"`
	// EphemeralMessageID is set instead of MessageID for a message only the
	// bot and its author see (an ephemeral command).
	EphemeralMessageID int `json:"ephemeral_message_id"`
	ThreadID           int `json:"message_thread_id"`
	Chat               struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From           *User  `json:"from"` // nil for messages on behalf of a chat
	Text           string `json:"text"`
	ReplyToMessage *struct {
		MessageID int `json:"message_id"`
	} `json:"reply_to_message"`
}

// MessageReaction is a change of one user's reactions on a message. Bots
// receive it only as chat administrators. User is nil for anonymous
// reactions (on behalf of a chat).
type MessageReaction struct {
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	MessageID   int            `json:"message_id"`
	User        *User          `json:"user"`
	OldReaction []ReactionType `json:"old_reaction"`
	NewReaction []ReactionType `json:"new_reaction"`
}

// ReactionType is a reaction; only "emoji" reactions carry Emoji.
type ReactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji,omitempty"`
}

// Added returns the emoji reactions in r that were not there before.
func (r *MessageReaction) Added() []string {
	var added []string
	for _, n := range r.NewReaction {
		if n.Type != "emoji" {
			continue
		}
		if !slices.ContainsFunc(r.OldReaction, func(o ReactionType) bool { return o.Type == "emoji" && o.Emoji == n.Emoji }) {
			added = append(added, n.Emoji)
		}
	}
	return added
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
	}{offset, int(timeout.Seconds()), []string{"callback_query", "message_reaction", "message"}}
	var updates []Update
	err := c.call(ctx, "getUpdates", params, &updates)
	return updates, err
}

// PinChatMessage pins a message without notifying the chat. The bot needs
// the right to pin messages.
func (c *Client) PinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	params := struct {
		ChatID              int64 `json:"chat_id"`
		MessageID           int   `json:"message_id"`
		DisableNotification bool  `json:"disable_notification"`
	}{chatID, messageID, true}
	return c.call(ctx, "pinChatMessage", params, nil)
}

// SetMessageReaction replaces the bot's reaction on a message with emoji,
// or removes it if emoji is "". Bots can set one reaction from Telegram's
// fixed list (not ✅ or 🔕, checked against the live API).
func (c *Client) SetMessageReaction(ctx context.Context, chatID int64, messageID int, emoji string) error {
	reaction := []ReactionType{}
	if emoji != "" {
		reaction = append(reaction, ReactionType{Type: "emoji", Emoji: emoji})
	}
	params := struct {
		ChatID    int64          `json:"chat_id"`
		MessageID int            `json:"message_id"`
		Reaction  []ReactionType `json:"reaction"`
	}{chatID, messageID, reaction}
	return c.call(ctx, "setMessageReaction", params, nil)
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

// MigratedTo returns the new chat ID if a call failed because the group was
// upgraded to a supergroup, else 0.
func MigratedTo(err error) int64 {
	var e *Error
	if errors.As(err, &e) {
		return e.MigrateToChatID
	}
	return 0
}

// IsThreadGone reports whether a send failed because its forum topic does
// not exist (deleted, or a wrong ID in ROUTES).
func IsThreadGone(err error) bool {
	return hasDescription(err, "message thread not found")
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

package telegram

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// pollTimeout is the long-poll wait; it stays below the client's 30 s HTTP
// timeout.
const pollTimeout = 25 * time.Second

// Poll long-polls for updates until ctx is done, passing each one to handle
// in order. ok is called after every successful poll (for /healthz).
//
// Telegram allows one getUpdates consumer per bot: a second relay with the
// same token, or a webhook set on the bot, makes every poll fail with 409.
func (c *Client) Poll(ctx context.Context, log *slog.Logger, handle func(context.Context, Update), ok func()) {
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		updates, err := c.GetUpdates(ctx, offset, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var e *Error
			if errors.As(err, &e) && e.Code == http.StatusConflict {
				log.Error("telegram refuses polling: another relay uses this bot token, or a webhook is set on the bot", "err", err)
			} else {
				log.Warn("polling telegram failed", "err", err)
			}
			wait := max(RetryAfter(err), backoff)
			backoff = min(backoff*2, 30*time.Second)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		backoff = time.Second
		ok()
		for _, u := range updates {
			// Confirm the update on the next poll even if handling it fails:
			// a press that went wrong is answered and not replayed.
			offset = max(offset, u.UpdateID+1)
			handle(ctx, u)
		}
	}
}

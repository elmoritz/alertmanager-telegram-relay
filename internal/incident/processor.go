package incident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/metrics"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Telegram is the part of the Bot API client the processor uses.
type Telegram interface {
	SendMessage(ctx context.Context, m telegram.Message) (int, error)
	EditMessageText(ctx context.Context, chatID int64, messageID int, text string) error
}

// Processor carries out the plan for one notification.
type Processor struct {
	Store    *store.Store
	Telegram Telegram
	Render   *render.Renderer
	Metrics  *metrics.Metrics
	Log      *slog.Logger

	Reminders    string // config.Reminders*
	ResolveReply bool

	// RetryFor bounds how long temporary Telegram failures are retried for
	// one notification (SPEC §9).
	RetryFor time.Duration
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
}

// Handle processes one notification for chatID. It returns an error only if
// the notification should stay queued and be tried again: the context was
// cancelled, or the store failed. Telegram failures that persist past
// RetryFor are logged and counted, and the notification is dropped.
func (p *Processor) Handle(ctx context.Context, chatID int64, m *alertmanager.Message) error {
	inc, err := p.Store.Incident(m.GroupKey)
	if err != nil {
		return err
	}
	plan := Decide(inc, m)
	now := p.Now()
	log := p.Log.With("incident", groupID(m.GroupKey), "action", plan.Kind.String())
	if p.Log.Enabled(ctx, slog.LevelDebug) {
		log = log.With("chat_id", chatID)
	}
	j := &job{p: p, ctx: ctx, log: log, deadline: now.Add(p.RetryFor), chatID: chatID}

	d := render.NewData(m)
	d.Added = plan.Added
	d.OpenedAt = now
	if inc != nil {
		d.OpenedAt = inc.OpenedAt
	}
	text, err := p.Render.Message(d)
	if err != nil {
		p.Metrics.TemplateErrors.Inc()
		log.Error("template failed, sending minimal message", "err", err)
	}
	minimal := p.Render.Minimal(d)

	switch plan.Kind {
	case Open:
		id, err := j.send(text, minimal, 0, false)
		if err != nil {
			return j.dropped(err)
		}
		inc = &store.Incident{GroupKey: m.GroupKey, ChatID: chatID, MessageID: id, OpenedAt: now, LastSeen: now, Firing: plan.Firing, TextHash: hash(text)}
		if err := p.Store.PutIncident(inc); err != nil {
			// The message is out; without the record the next notification
			// opens a second one. Not worth re-sending this one for.
			log.Error("storing incident failed", "err", err)
		}
		log.Info("incident opened", "alerts", len(plan.Firing))
		return nil

	case Changed, Repeat:
		if err := j.update(inc, text, minimal, false); err != nil {
			return j.dropped(err)
		}
		inc.LastSeen, inc.Firing = now, plan.Firing
		if err := p.Store.PutIncident(inc); err != nil {
			return err
		}
		if plan.Kind == Changed {
			log.Info("incident changed", "alerts", len(plan.Firing), "added", len(plan.Added))
			if len(plan.Added) > 0 {
				j.reply(inc.MessageID, p.Render.Added, d, false)
			}
			return nil
		}
		switch p.Reminders {
		case config.RemindersReply:
			j.reply(inc.MessageID, p.Render.Reminder, d, false)
		case config.RemindersSilent:
			j.reply(inc.MessageID, p.Render.Reminder, d, true)
		}
		log.Info("incident still firing")
		return nil

	case Resolve:
		if err := j.update(inc, text, minimal, true); err != nil {
			return j.dropped(err)
		}
		if err := p.Store.DeleteIncident(m.GroupKey); err != nil {
			return err
		}
		if p.ResolveReply {
			j.reply(inc.MessageID, p.Render.ResolveReply, d, true)
		}
		log.Info("incident resolved", "duration", now.Sub(inc.OpenedAt).Round(time.Second).String())
		return nil

	case ResolveUnknown:
		if _, err := j.send(text, minimal, 0, true); err != nil {
			return j.dropped(err)
		}
		log.Info("resolve for unknown incident sent as new message")
		return nil
	}
	return fmt.Errorf("unhandled plan %v", plan.Kind)
}

// job is the state of one Handle call.
type job struct {
	p        *Processor
	ctx      context.Context
	log      *slog.Logger
	deadline time.Time
	chatID   int64
}

// send sends text (or minimal, if Telegram rejects text's formatting or
// length) and returns the new message ID.
func (j *job) send(text, minimal string, replyTo int, silent bool) (int, error) {
	var id int
	err := j.withFallback(text, minimal, func(t string) error {
		return j.retry(func(ctx context.Context) error {
			var err error
			id, err = j.p.Telegram.SendMessage(ctx, telegram.Message{ChatID: j.chatID, Text: t, ReplyTo: replyTo, Silent: silent})
			return err
		})
	})
	return id, err
}

// update edits the incident's message to text, unless it already shows it.
// If the message cannot be edited any more, it sends a new one and points
// the incident at it.
func (j *job) update(inc *store.Incident, text, minimal string, silent bool) error {
	h := hash(text)
	if h == inc.TextHash {
		return nil
	}
	err := j.withFallback(text, minimal, func(t string) error {
		return j.retry(func(ctx context.Context) error {
			return j.p.Telegram.EditMessageText(ctx, inc.ChatID, inc.MessageID, t)
		})
	})
	if telegram.IsMessageGone(err) {
		j.log.Warn("message can no longer be edited, sending a new one")
		var id int
		if id, err = j.send(text, minimal, 0, silent); err == nil {
			inc.MessageID = id
		}
	}
	if err == nil {
		inc.TextHash = h
	}
	return err
}

// reply sends a best-effort reply: a failure is logged, never retried past
// the job's deadline and never blocks the incident.
func (j *job) reply(to int, render func(render.Data) (string, error), d render.Data, silent bool) {
	text, err := render(d)
	if err != nil {
		j.p.Metrics.TemplateErrors.Inc()
		j.log.Error("reply template failed, using fallback", "err", err)
	}
	if _, err := j.send(text, text, to, silent); err != nil {
		j.log.Warn("reply not delivered", "err", err)
	}
}

func (j *job) withFallback(text, minimal string, do func(string) error) error {
	err := do(text)
	if telegram.IsFormatError(err) && minimal != text {
		j.log.Warn("telegram rejected the message text, sending minimal message", "err", err)
		j.p.Metrics.TemplateErrors.Inc()
		err = do(minimal)
	}
	return err
}

// retry repeats op while it fails temporarily, honouring Telegram's
// retry_after, with exponential backoff, until the job's deadline.
func (j *job) retry(op func(ctx context.Context) error) error {
	backoff := time.Second
	for {
		err := op(j.ctx)
		if err == nil || !telegram.IsRetryable(err) {
			return err
		}
		wait := max(telegram.RetryAfter(err), backoff)
		if j.p.Now().Add(wait).After(j.deadline) {
			return fmt.Errorf("giving up after retries: %w", err)
		}
		j.p.Metrics.DeliveryRetries.Inc()
		j.log.Warn("telegram call failed, retrying", "err", err, "in", wait.String())
		if err := j.p.Sleep(j.ctx, wait); err != nil {
			return err
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// dropped decides what a failed main delivery means for the queue entry.
func (j *job) dropped(err error) error {
	if j.ctx.Err() != nil {
		return j.ctx.Err() // shutting down: keep it queued for the next start
	}
	j.p.Metrics.DeliveriesDropped.Inc()
	j.log.Error("notification dropped", "err", err)
	return nil
}

// tgTimeText matches the fallback text inside date-time entities.
var tgTimeText = regexp.MustCompile(`(<tg-time[^>]*>)[^<]*(</tg-time>)`)

// hash identifies a message text for "has it changed?". The fallback text of
// date-time entities is left out: it only drifts with the clock ("2h ago",
// "3h ago"), and clients that support the entity render it live anyway.
func hash(s string) string {
	sum := sha256.Sum256([]byte(tgTimeText.ReplaceAllString(s, "$1$2")))
	return hex.EncodeToString(sum[:16])
}

// groupID is a short, stable stand-in for a group key in logs. Group keys
// contain label values, which stay out of logs.
func groupID(groupKey string) string {
	sum := sha256.Sum256([]byte(groupKey))
	return hex.EncodeToString(sum[:6])
}

// SleepContext waits for d or until ctx is done.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

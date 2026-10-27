package incident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"regexp"
	"sync"
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
	EditMessageText(ctx context.Context, chatID int64, messageID int, text string, kb *telegram.Keyboard) error
}

// Processor carries out the plan for one notification. It also renders and
// updates incident messages for button presses (Buttons), and serialises
// both per group, so a message is never edited from two sides at once.
type Processor struct {
	Store    *store.Store
	Telegram Telegram
	Render   *render.Renderer
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	Buttons  ButtonConfig

	Reminders    string // config.Reminders*
	ResolveReply bool

	// RetryFor bounds how long temporary Telegram failures are retried for
	// one notification (SPEC §9).
	RetryFor time.Duration
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error

	locks [64]sync.Mutex
}

// lock serialises work on one group. Locks are striped: unrelated groups
// rarely share one, and memory stays bounded.
func (p *Processor) lock(groupKey string) func() {
	h := fnv.New32a()
	h.Write([]byte(groupKey))
	mu := &p.locks[h.Sum32()%uint32(len(p.locks))]
	mu.Lock()
	return mu.Unlock
}

// Handle processes one notification for chatID. It returns an error only if
// the notification should stay queued and be tried again: the context was
// cancelled, or the store failed. Telegram failures that persist past
// RetryFor are logged and counted, and the notification is dropped.
func (p *Processor) Handle(ctx context.Context, chatID int64, m *alertmanager.Message) error {
	defer p.lock(m.GroupKey)()

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
	last, err := json.Marshal(m)
	if err != nil {
		return err
	}

	switch plan.Kind {
	case Open:
		inc = &store.Incident{GroupKey: m.GroupKey, ChatID: chatID, OpenedAt: now, LastSeen: now, Firing: plan.Firing,
			GroupLabels: m.GroupLabels, Last: last}
		if _, _, err := p.issueButtons(inc, now, true); err != nil {
			return err
		}
		v := p.view(inc, m, nil)
		id, err := j.send(v, 0, false)
		if err != nil {
			j.forget(inc.ButtonIDs())
			return j.dropped(err)
		}
		inc.MessageID, inc.TextHash = id, v.hash()
		if err := p.Store.PutIncident(inc); err != nil {
			// The message is out; without the record the next notification
			// opens a second one. Not worth re-sending this one for.
			log.Error("storing incident failed", "err", err)
		}
		log.Info("incident opened", "alerts", len(plan.Firing))
		return nil

	case Changed, Repeat:
		inc.Last, inc.GroupLabels = last, m.GroupLabels
		stale, issued, err := p.issueButtons(inc, now, false)
		if err != nil {
			return err
		}
		if err := j.update(inc, p.view(inc, m, plan.Added), false); err != nil {
			if issued {
				j.forget(inc.ButtonIDs()) // never shown; the stored keyboard stays as it was
			}
			return j.dropped(err)
		}
		inc.LastSeen, inc.Firing = now, plan.Firing
		if err := p.Store.PutIncident(inc); err != nil {
			return err
		}
		j.forget(stale)
		d := p.data(inc, m, plan.Added)
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
		if err := j.update(inc, p.view(inc, m, nil), true); err != nil {
			return j.dropped(err)
		}
		if err := p.Store.DeleteIncident(m.GroupKey); err != nil {
			return err
		}
		j.forget(inc.ButtonIDs())
		if p.ResolveReply {
			j.reply(inc.MessageID, p.Render.ResolveReply, p.data(inc, m, nil), true)
		}
		log.Info("incident resolved", "duration", now.Sub(inc.OpenedAt).Round(time.Second).String())
		return nil

	case ResolveUnknown:
		if _, err := j.send(p.view(&store.Incident{GroupKey: m.GroupKey, OpenedAt: now}, m, nil), 0, true); err != nil {
			return j.dropped(err)
		}
		log.Info("resolve for unknown incident sent as new message")
		return nil
	}
	return fmt.Errorf("unhandled plan %v", plan.Kind)
}

// data is the template data for inc's message showing notification m.
func (p *Processor) data(inc *store.Incident, m *alertmanager.Message, added []alertmanager.Alert) render.Data {
	d := render.NewData(m)
	d.Added = added
	d.OpenedAt = inc.OpenedAt
	if s := inc.Silence; s != nil && m.Status != alertmanager.StatusResolved {
		d.Silence = &render.Silence{Until: s.Until, By: s.ByName}
	}
	return d
}

// view is everything that can be put into the incident's message, from the
// full version down to the fallbacks.
type view struct {
	text, minimal   string
	keyboard, plain *telegram.Keyboard // plain: without link buttons
}

func (p *Processor) view(inc *store.Incident, m *alertmanager.Message, added []alertmanager.Alert) view {
	d := p.data(inc, m, added)
	text, err := p.Render.Message(d)
	if err != nil {
		p.Metrics.TemplateErrors.Inc()
		p.Log.Error("template failed, sending minimal message", "incident", groupID(inc.GroupKey), "err", err)
	}
	return view{text: text, minimal: p.Render.Minimal(d), keyboard: p.keyboard(inc, m, true), plain: p.keyboard(inc, m, false)}
}

// variant is one (text, keyboard) pair to try.
type variant struct {
	text string
	kb   *telegram.Keyboard
}

// variants lists what to try, in order: everything; without link buttons
// (Telegram refuses some URLs, e.g. internal host names); the minimal text.
func (v view) variants() []variant {
	out := []variant{{v.text, v.keyboard}}
	if !sameKeyboard(v.keyboard, v.plain) {
		out = append(out, variant{v.text, v.plain})
	}
	if v.minimal != v.text {
		out = append(out, variant{v.minimal, v.plain})
	}
	return out
}

func (v view) hash() string { return hash(v.text, v.keyboard) }

func sameKeyboard(a, b *telegram.Keyboard) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// job is the state of one Handle call (or one button press).
type job struct {
	p        *Processor
	ctx      context.Context
	log      *slog.Logger
	deadline time.Time
	chatID   int64
}

// send sends the first variant of v that Telegram accepts and returns the
// new message's ID.
func (j *job) send(v view, replyTo int, silent bool) (int, error) {
	var id int
	err := j.withFallback(v, func(vr variant) error {
		return j.retry(func(ctx context.Context) error {
			var err error
			id, err = j.p.Telegram.SendMessage(ctx, telegram.Message{ChatID: j.chatID, Text: vr.text, ReplyTo: replyTo, Silent: silent, Keyboard: vr.kb})
			return err
		})
	})
	return id, err
}

// update edits the incident's message to v, unless it already shows it. If
// the message cannot be edited any more, it sends a new one and points the
// incident at it.
func (j *job) update(inc *store.Incident, v view, silent bool) error {
	h := v.hash()
	if h == inc.TextHash {
		return nil
	}
	err := j.withFallback(v, func(vr variant) error {
		return j.retry(func(ctx context.Context) error {
			return j.p.Telegram.EditMessageText(ctx, inc.ChatID, inc.MessageID, vr.text, vr.kb)
		})
	})
	if telegram.IsMessageGone(err) {
		j.log.Warn("message can no longer be edited, sending a new one")
		var id int
		if id, err = j.send(v, 0, silent); err == nil {
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
	if _, err := j.send(view{text: text, minimal: text}, to, silent); err != nil {
		j.log.Warn("reply not delivered", "err", err)
	}
}

// withFallback tries the variants of v in order while Telegram rejects the
// text or a button.
func (j *job) withFallback(v view, do func(variant) error) error {
	var err error
	for i, vr := range v.variants() {
		if i > 0 {
			j.log.Warn("telegram rejected the message, trying a simpler version", "err", err)
			j.p.Metrics.TemplateErrors.Inc()
		}
		err = do(vr)
		if !telegram.IsFormatError(err) && !telegram.IsButtonError(err) {
			return err
		}
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

// forget deletes actions that are no longer on any keyboard. Failure only
// leaves them to expire.
func (j *job) forget(actionIDs []string) {
	if len(actionIDs) == 0 {
		return
	}
	if err := j.p.Store.DeleteActions(actionIDs...); err != nil {
		j.log.Warn("deleting old button actions failed", "err", err)
	}
}

// tgTimeText matches the fallback text inside date-time entities.
var tgTimeText = regexp.MustCompile(`(<tg-time[^>]*>)[^<]*(</tg-time>)`)

// hash identifies a message's content for "has it changed?". The fallback
// text of date-time entities is left out: it only drifts with the clock
// ("2h ago", "3h ago"), and clients that support the entity render it live.
func hash(text string, kb *telegram.Keyboard) string {
	h := sha256.New()
	h.Write([]byte(tgTimeText.ReplaceAllString(text, "$1$2")))
	if kb != nil {
		h.Write([]byte{0})
		_ = json.NewEncoder(h).Encode(kb)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
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

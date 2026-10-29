package incident

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Silencer is the part of the Alertmanager client the buttons use.
type Silencer interface {
	CreateSilence(ctx context.Context, s alertmanager.Silence) (string, error)
	ExpireSilence(ctx context.Context, id string) error
	ActiveAlerts(ctx context.Context, labels map[string]string) (int, error)
}

// Answerer answers callback queries.
type Answerer interface {
	AnswerCallbackQuery(ctx context.Context, id, text string) error
}

// Buttons handles presses of the silence and unsilence buttons (SPEC §5,
// SECURITY.md §2 and §3).
type Buttons struct {
	Processor    *Processor
	Telegram     Answerer
	Alertmanager Silencer // nil when ALERTMANAGER_URL is not set
	ChatIDs      []int64
	AdminUserIDs []int64
	Log          *slog.Logger
	// Enqueue queues a notification like one from Alertmanager (the
	// dispatcher's Enqueue); CheckSilenced uses it.
	Enqueue func(*alertmanager.Message) error

	emptySince map[string]time.Time // CheckSilenced: group key → first check without alerts
}

// Toast texts. Only allowed users in allowed chats ever see them.
const (
	toastExpired     = "This button has expired."
	toastUnreachable = "Alertmanager is unreachable. Nothing changed."
	toastUnsilenced  = "Silence removed."
)

// silenceComment is attached to every silence the relay creates.
const silenceComment = "Silenced from Telegram via alertmanager-telegram-relay"

// HandleUpdate processes one update from getUpdates.
func (b *Buttons) HandleUpdate(ctx context.Context, u telegram.Update) {
	cq := u.CallbackQuery
	if cq == nil {
		return // nothing else is requested; drop silently
	}
	if !b.authorized(cq) {
		// No text, no reason: an empty answer only stops the client's spinner.
		b.Processor.Metrics.UpdatesRejected.Inc()
		if b.Log.Enabled(ctx, slog.LevelDebug) {
			chat := int64(0)
			if cq.Message != nil {
				chat = cq.Message.Chat.ID
			}
			b.Log.Debug("rejected callback", "user_id", cq.From.ID, "chat_id", chat)
		}
		b.answer(ctx, cq, "")
		return
	}
	b.press(ctx, cq)
}

// authorized checks both the chat the message lives in and the user who
// pressed: an allowed group does not make all its members admins.
func (b *Buttons) authorized(cq *telegram.CallbackQuery) bool {
	return cq.Message != nil &&
		slices.Contains(b.ChatIDs, cq.Message.Chat.ID) &&
		slices.Contains(b.AdminUserIDs, cq.From.ID)
}

// press carries out an authorised press. Every path answers the query
// exactly once; when the press changes something, the answer comes before
// the message is updated, so the user's spinner stops right away.
func (b *Buttons) press(ctx context.Context, cq *telegram.CallbackQuery) {
	p := b.Processor
	now := p.Now()
	action, err := p.Store.Action(cq.Data, now)
	if err != nil {
		b.Log.Error("reading action", "err", err)
		b.answer(ctx, cq, toastUnreachable)
		return
	}
	if action == nil {
		p.Metrics.ButtonPresses.WithLabelValues("unknown", "stale").Inc()
		b.answer(ctx, cq, toastExpired)
		return
	}

	defer p.lock(action.GroupKey)()
	inc, err := p.Store.Incident(action.GroupKey)
	if err != nil {
		b.Log.Error("reading incident", "err", err)
		b.answer(ctx, cq, toastUnreachable)
		return
	}
	// The relay only acts on its own, current messages: the action must be
	// on the keyboard of the incident's message the press came from.
	if inc == nil || inc.ChatID != cq.Message.Chat.ID || inc.MessageID != cq.Message.MessageID || !inc.HasButton(action.ID) {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		b.answer(ctx, cq, toastExpired)
		return
	}
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		b.Log.Error("reading stored notification", "err", err)
		b.answer(ctx, cq, toastUnreachable)
		return
	}

	log := b.Log.With("incident", groupID(inc.GroupKey), "action", action.Kind, "user_id", cq.From.ID)
	var toast string
	var changed bool
	switch action.Kind {
	case store.ActionSilence:
		toast, changed = b.silence(ctx, log, inc, action, cq.From, now)
	case store.ActionUnsilence:
		toast, changed = b.unsilence(ctx, log, inc, action)
	default:
		toast = toastExpired
	}
	b.answer(ctx, cq, toast)
	if changed {
		b.refresh(ctx, log, inc, &m, now)
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "ok").Inc()
	}
}

// silence creates the silence. It reports the toast and whether inc changed.
func (b *Buttons) silence(ctx context.Context, log *slog.Logger, inc *store.Incident, action *store.Action, user telegram.User, now time.Time) (string, bool) {
	p := b.Processor
	matchers := silenceMatchers(inc.GroupLabels)
	if b.Alertmanager == nil || len(matchers) == 0 || action.Duration <= 0 {
		// Not reachable through issueButtons; refuse rather than guess.
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		return toastExpired, false
	}
	id, err := b.callAlertmanager(ctx, func(ctx context.Context) (string, error) {
		return b.Alertmanager.CreateSilence(ctx, alertmanager.Silence{
			Matchers:  matchers,
			StartsAt:  now,
			EndsAt:    now.Add(action.Duration),
			CreatedBy: telegramUser(user.ID),
			Comment:   silenceComment,
		})
	})
	if err != nil {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "error").Inc()
		log.Warn("creating silence failed", "err", err)
		return toastUnreachable, false
	}
	log.Info("silence created", "silence_id", id, "duration", action.Duration.String())
	inc.Silence = &store.Silence{ID: id, Until: now.Add(action.Duration), ByID: user.ID, ByName: user.FirstName}
	return "Silenced for " + shortDuration(action.Duration) + ".", true
}

// unsilence expires the silence. It reports the toast and whether inc changed.
func (b *Buttons) unsilence(ctx context.Context, log *slog.Logger, inc *store.Incident, action *store.Action) (string, bool) {
	p := b.Processor
	if b.Alertmanager == nil || inc.Silence == nil || inc.Silence.ID != action.SilenceID {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		return toastExpired, false
	}
	_, err := b.callAlertmanager(ctx, func(ctx context.Context) (string, error) {
		return "", b.Alertmanager.ExpireSilence(ctx, action.SilenceID)
	})
	if err != nil && !errors.Is(err, alertmanager.ErrSilenceNotFound) {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "error").Inc()
		log.Warn("expiring silence failed", "silence_id", action.SilenceID, "err", err)
		return toastUnreachable, false
	}
	log.Info("silence expired", "silence_id", action.SilenceID)
	inc.Silence = nil
	return toastUnsilenced, true
}

// refresh re-issues the buttons for inc's new state, updates the message and
// stores the incident. The Alertmanager change has happened at this point;
// a failed message update is logged, the stored state stays truthful.
func (b *Buttons) refresh(ctx context.Context, log *slog.Logger, inc *store.Incident, m *alertmanager.Message, now time.Time) {
	p := b.Processor
	stale, _, err := p.issueButtons(inc, now, true)
	if err != nil {
		log.Error("issuing buttons failed", "err", err)
	}
	j := &job{p: p, ctx: ctx, log: log, deadline: now.Add(30 * time.Second), chatID: inc.ChatID}
	if err := j.update(inc, p.view(inc, m, nil), true); err != nil {
		log.Warn("updating message after button press failed", "err", err)
	}
	if err := p.Store.PutIncident(inc); err != nil {
		log.Error("storing incident failed", "err", err)
		return
	}
	j.forget(stale) // unsilence is single-use; old silence buttons stop working
}

func (b *Buttons) callAlertmanager(ctx context.Context, call func(context.Context) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return call(ctx)
}

func (b *Buttons) answer(ctx context.Context, cq *telegram.CallbackQuery, text string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := b.Telegram.AnswerCallbackQuery(ctx, cq.ID, text); err != nil {
		b.Log.Debug("answering callback failed", "err", err)
	}
}

// silenceMatchers builds exact-equality matchers from all group labels, in
// a stable order. Never from anything a button carries.
func silenceMatchers(groupLabels map[string]string) []alertmanager.Matcher {
	var ms []alertmanager.Matcher
	for _, k := range slices.Sorted(maps.Keys(groupLabels)) {
		ms = append(ms, alertmanager.Matcher{Name: k, Value: groupLabels[k], IsRegex: false, IsEqual: true})
	}
	return ms
}

// telegramUser is the silence's createdBy (SECURITY.md §3).
func telegramUser(id int64) string {
	return "telegram:" + strconv.FormatInt(id, 10)
}

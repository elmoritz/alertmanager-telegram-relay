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
	toastAcked       = "It's yours."
	toastReleased    = "Released."
)

// silenceComment is attached to every silence the relay creates.
const silenceComment = "Silenced from Telegram via alertmanager-telegram-relay"

// HandleUpdate processes one update from getUpdates.
func (b *Buttons) HandleUpdate(ctx context.Context, u telegram.Update) {
	if u.MessageReaction != nil {
		b.handleReaction(ctx, u.MessageReaction)
		return
	}
	if u.Message != nil {
		b.handleMessage(ctx, u.Message)
		return
	}
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
	case store.ActionCustom:
		toast = b.promptCustom(ctx, log, inc, cq.From)
	case store.ActionAck:
		toast, changed = toastAcked, b.ack(log, inc, cq.From, now)
	case store.ActionRelease:
		// Any admin may release: they are equally trusted, and someone may
		// have fallen asleep on it.
		inc.Ack, toast, changed = nil, toastReleased, true
		log.Info("incident released")
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
	if err := b.createSilence(ctx, log, inc, action.Duration, "", user, now); err != nil {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "error").Inc()
		return toastUnreachable, false
	}
	return "Silenced for " + shortDuration(action.Duration) + ".", true
}

// createSilence silences inc's group for d, with matchers from its stored
// group labels only (SECURITY.md §3), and records it on inc. A silence that
// was active before is expired afterwards, so the new one replaces it.
// reason, if any, goes before the relay's own comment.
func (b *Buttons) createSilence(ctx context.Context, log *slog.Logger, inc *store.Incident, d time.Duration, reason string, user telegram.User, now time.Time) error {
	comment := silenceComment
	if reason != "" {
		comment = reason + " (" + silenceComment + ")"
	}
	id, err := b.callAlertmanager(ctx, func(ctx context.Context) (string, error) {
		return b.Alertmanager.CreateSilence(ctx, alertmanager.Silence{
			Matchers:  silenceMatchers(inc.GroupLabels),
			StartsAt:  now,
			EndsAt:    now.Add(d),
			CreatedBy: telegramUser(user.ID),
			Comment:   comment,
		})
	})
	if err != nil {
		log.Warn("creating silence failed", "err", err)
		return err
	}
	log.Info("silence created", "silence_id", id, "duration", d.String())
	if old := inc.Silence; old != nil {
		if _, err := b.callAlertmanager(ctx, func(ctx context.Context) (string, error) {
			return "", b.Alertmanager.ExpireSilence(ctx, old.ID)
		}); err != nil && !errors.Is(err, alertmanager.ErrSilenceNotFound) {
			log.Warn("expiring the replaced silence failed", "silence_id", old.ID, "err", err)
		}
	}
	inc.Silence = &store.Silence{ID: id, Until: now.Add(d), ByID: user.ID, ByName: user.FirstName}
	return nil
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
// ackReactions are the reactions that count as "I'm on it".
var ackReactions = []string{"👀", "👍"}

// handleReaction treats an admin's 👀 or 👍 on an incident's current
// message, or on one of its escalation replies, like a press of "I'm on
// it". The same checks as for buttons
// apply: allowed chat and allowed user (anonymous reactions have no user
// and are ignored), and only the relay's own, current messages.
func (b *Buttons) handleReaction(ctx context.Context, r *telegram.MessageReaction) {
	p := b.Processor
	if r.User == nil || !slices.Contains(b.ChatIDs, r.Chat.ID) || !slices.Contains(b.AdminUserIDs, r.User.ID) {
		p.Metrics.UpdatesRejected.Inc()
		return
	}
	if !slices.ContainsFunc(r.Added(), func(e string) bool { return slices.Contains(ackReactions, e) }) {
		return
	}
	onIncident := func(inc *store.Incident) bool {
		return inc.ChatID == r.Chat.ID && (inc.MessageID == r.MessageID || slices.Contains(inc.EscalationMessageIDs, r.MessageID))
	}
	incs, err := p.Store.Incidents(onIncident)
	if err != nil || len(incs) == 0 {
		return
	}
	defer p.lock(incs[0].GroupKey)()
	inc, err := p.Store.Incident(incs[0].GroupKey)
	if err != nil || inc == nil || !onIncident(inc) {
		return
	}
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		return
	}
	now := p.Now()
	log := b.Log.With("incident", groupID(inc.GroupKey), "action", store.ActionAck, "via", "reaction", "user_id", r.User.ID)
	if b.ack(log, inc, *r.User, now) {
		b.refresh(ctx, log, inc, &m, now)
	}
}

// ack marks inc as handled by user. It changes only the relay's state, never
// Alertmanager. It reports whether inc changed.
func (b *Buttons) ack(log *slog.Logger, inc *store.Incident, user telegram.User, now time.Time) bool {
	if inc.Ack != nil {
		return false
	}
	inc.Ack = &store.Ack{ByID: user.ID, ByName: user.FirstName, At: now}
	log.Info("incident acknowledged")
	return true
}

func (b *Buttons) refresh(ctx context.Context, log *slog.Logger, inc *store.Incident, m *alertmanager.Message, now time.Time) {
	p := b.Processor
	stale, _, err := p.issueButtons(inc, now, true)
	if err != nil {
		log.Error("issuing buttons failed", "err", err)
	}
	j := &job{p: p, ctx: ctx, log: log, deadline: now.Add(30 * time.Second), chatID: inc.ChatID, threadID: inc.ThreadID}
	if err := j.update(inc, p.view(inc, m, nil), true); err != nil {
		log.Warn("updating message after button press failed", "err", err)
	}
	j.react(inc, false)
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

package incident

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/komodo"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Runner runs remediation actions (internal/komodo).
type Runner interface {
	Start(ctx context.Context, a komodo.Action) (string, error)
	Wait(ctx context.Context, updateID string) (bool, error)
}

const (
	// confirmTTL is how long the confirmation's buttons work.
	confirmTTL = time.Minute
	// remedyTimeout bounds waiting for Komodo's result.
	remedyTimeout = 15 * time.Minute

	toastConfirm   = "Confirm below."
	toastStarted   = "Started."
	toastCancelled = "Cancelled."
)

// remedy returns the configured action with id, if it is still configured.
func (b *Buttons) remedy(id string) (komodo.Action, bool) {
	for _, r := range b.Processor.Buttons.Remedies {
		if r.ID == id {
			return r, true
		}
	}
	return komodo.Action{}, false
}

// offerRemedy answers a 🛠 button with a confirmation only the presser sees
// (SECURITY.md §9): nothing runs yet.
func (b *Buttons) offerRemedy(ctx context.Context, log *slog.Logger, inc *store.Incident, action *store.Action, user telegram.User) string {
	p := b.Processor
	r, ok := b.remedy(action.RemedyID)
	if !ok || b.Komodo == nil || !r.Offered(inc.GroupLabels) {
		return toastExpired
	}
	now := p.Now()
	confirm := &store.Action{ID: newActionID(), Kind: store.ActionRemedyConfirm, GroupKey: inc.GroupKey, RemedyID: r.ID, ChatID: inc.ChatID, UserID: user.ID, ExpiresAt: now.Add(confirmTTL)}
	cancel := &store.Action{ID: newActionID(), Kind: store.ActionRemedyCancel, GroupKey: inc.GroupKey, RemedyID: r.ID, ChatID: inc.ChatID, UserID: user.ID, ExpiresAt: now.Add(confirmTTL)}
	if err := p.Store.PutActions(confirm, cancel); err != nil {
		log.Error("storing confirmation failed", "err", err)
		return toastUnreachable
	}
	text := fmt.Sprintf("🛠 <b>%s</b>?\nKomodo will run %s on %s. Confirm within a minute.", html.EscapeString(r.Label), html.EscapeString(r.Operation), html.EscapeString(remedyTarget(r)))
	kb := &telegram.Keyboard{Rows: [][]telegram.Button{{
		{Text: "✅ Yes, " + r.Label, Data: confirm.ID, Style: telegram.StyleDanger},
		{Text: "Cancel", Data: cancel.ID},
	}}}
	ctx, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	id, err := p.Telegram.SendMessage(ctx, telegram.Message{ChatID: inc.ChatID, ThreadID: inc.ThreadID, Text: text, Keyboard: kb, Silent: true, EphemeralTo: user.ID})
	if err != nil {
		log.Warn("sending the confirmation failed", "err", err)
		return toastUnreachable
	}
	b.rememberAnswer([]*store.Action{confirm, cancel}, id)
	return toastConfirm
}

func remedyTarget(r komodo.Action) string {
	if r.Operation == komodo.RestartContainer {
		return "container " + r.Container + " on " + r.Server
	}
	return "stack " + r.Stack
}

// decideRemedy carries out the confirmation's "Yes" or "Cancel": only for
// the user it was sent to, in that chat, once, and only while the incident
// is open and the action still offered on it.
func (b *Buttons) decideRemedy(ctx context.Context, cq *telegram.CallbackQuery, action *store.Action) {
	p := b.Processor
	if cq.Message == nil || cq.Message.Chat.ID != action.ChatID || cq.From.ID != action.UserID {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		b.answer(ctx, cq, toastExpired)
		return
	}
	// Single use: both buttons of the confirmation stop working.
	pair := []string{action.ID}
	if a, err := p.Store.ActionsOnAnswer(action.EphemeralMessageID, action.ChatID); err == nil {
		pair = append(pair, a...)
	}
	if err := p.Store.DeleteActions(pair...); err != nil {
		b.Log.Warn("deleting a used confirmation failed", "err", err)
	}
	editAnswer := func(text string) {
		if action.EphemeralMessageID == 0 {
			return
		}
		ctx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		_ = p.Telegram.EditEphemeralMessageText(ctx, action.ChatID, action.EphemeralMessageID, action.UserID, text, nil)
	}
	if action.Kind == store.ActionRemedyCancel {
		b.answer(ctx, cq, toastCancelled)
		editAnswer("🛠 Cancelled.")
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "ok").Inc()
		return
	}

	r, ok := b.remedy(action.RemedyID)
	inc, err := p.Store.Incident(action.GroupKey)
	if !ok || b.Komodo == nil || err != nil || inc == nil || inc.ChatID != action.ChatID || !r.Offered(inc.GroupLabels) {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		b.answer(ctx, cq, toastExpired)
		editAnswer("🛠 No longer possible: the incident is closed or the action changed.")
		return
	}
	now := p.Now()
	log := b.Log.With("incident", groupID(inc.GroupKey), "action", "remedy", "remedy", r.ID, "user_id", cq.From.ID)
	run, last, err := p.Store.RemedyRun(r.ID, now, r.Cooldown)
	if err != nil {
		log.Error("checking the cooldown failed", "err", err)
		b.answer(ctx, cq, toastUnreachable)
		return
	}
	if !run {
		p.Metrics.Remediations.WithLabelValues("cooldown").Inc()
		wait := last.Add(r.Cooldown)
		b.answer(ctx, cq, "Ran "+render.Duration(last, now)+" ago; possible again at "+p.Render.ShortTime(wait)+".")
		editAnswer("🛠 Not run: cooldown until " + html.EscapeString(p.Render.ShortTime(wait)) + ".")
		return
	}
	b.answer(ctx, cq, toastStarted)
	editAnswer("🛠 Started: " + html.EscapeString(r.Label) + ".")
	p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "ok").Inc()
	b.runRemedy(ctx, log, inc, r, cq.From)
}

// runRemedy starts the action, posts it under the incident for everyone,
// and edits that reply with Komodo's result, in the background.
func (b *Buttons) runRemedy(ctx context.Context, log *slog.Logger, inc *store.Incident, r komodo.Action, user telegram.User) {
	p := b.Processor
	who := html.EscapeString(clipName(user.FirstName))
	label := html.EscapeString(r.Label)
	sendCtx, done := context.WithTimeout(ctx, 10*time.Second)
	replyID, err := p.Telegram.SendMessage(sendCtx, telegram.Message{ChatID: inc.ChatID, ThreadID: inc.ThreadID, ReplyTo: inc.MessageID, Silent: true,
		Text: fmt.Sprintf("🛠 %s: %s …", who, label)})
	done()
	if err != nil {
		log.Warn("posting the remediation failed", "err", err)
	}
	report := func(text string) {
		if replyID == 0 {
			return
		}
		ctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if err := p.Telegram.EditMessageText(ctx, inc.ChatID, replyID, fmt.Sprintf(text, who, label), nil); err != nil {
			log.Warn("updating the remediation reply failed", "err", err)
		}
	}

	start := p.Now()
	b.remedies.Add(1)
	go func() {
		defer b.remedies.Done()
		ctx, done := context.WithTimeout(context.WithoutCancel(ctx), remedyTimeout)
		defer done()
		id, err := b.Komodo.Start(ctx, r)
		if err != nil {
			p.Metrics.Remediations.WithLabelValues("error").Inc()
			log.Warn("remediation not started", "err", err)
			report("❌ %s: %s could not be started (Komodo unreachable or refused).")
			return
		}
		log.Info("remediation started", "komodo_update", id)
		ok, err := b.Komodo.Wait(ctx, id)
		took := html.EscapeString(render.Duration(start, p.Now())) // "<1m" would break the HTML
		switch {
		case err != nil:
			p.Metrics.Remediations.WithLabelValues("error").Inc()
			log.Warn("remediation result unknown", "komodo_update", id, "err", err)
			report("⚠️ %s: %s, result unknown; check Komodo.")
		case ok:
			p.Metrics.Remediations.WithLabelValues("ok").Inc()
			log.Info("remediation done", "komodo_update", id, "result", "ok")
			report("✅ %s: %s, done after " + took + ".")
		default:
			p.Metrics.Remediations.WithLabelValues("failed").Inc()
			log.Info("remediation done", "komodo_update", id, "result", "failed")
			report("❌ %s: %s failed; see Komodo.")
		}
	}()
}

// WaitRemedies waits for remediation actions still running.
func (b *Buttons) WaitRemedies() { b.remedies.Wait() }

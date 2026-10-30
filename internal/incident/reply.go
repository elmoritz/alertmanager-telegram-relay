package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Reactions on an admin's reply asking for a custom silence: done, not
// understood, could not be done (Alertmanager unreachable).
const (
	reactionDone      = "👌"
	reactionPuzzled   = "🤔"
	reactionCannotDo  = "🤷"
	maxReasonRunes    = 200
	toastCustomPrompt = "Reply with a duration and a reason."
)

var customSilenceRE = regexp.MustCompile(`^\s*([0-9]{1,4})\s*([mhd])(?:\s+(.*))?$`)

// parseCustomSilence reads "3h kernel update": a number with m, h or d
// (1 minute to 7 days), then an optional reason of at most 200 characters.
// Anything else is not a custom silence.
func parseCustomSilence(s string) (time.Duration, string, bool) {
	m := customSilenceRE.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d < time.Minute || d > config.MaxSilence {
		return 0, "", false
	}
	reason := strings.Join(strings.Fields(m[3]), " ")
	if len([]rune(reason)) > maxReasonRunes {
		return 0, "", false
	}
	return d, reason, true
}

// promptCustom answers "⏱ Custom…" with a reply under the incident that
// opens the presser's reply field. Replying to it, or to the incident,
// with "3h kernel update" silences the group (handleMessage).
func (b *Buttons) promptCustom(ctx context.Context, log *slog.Logger, inc *store.Incident, user telegram.User) string {
	p := b.Processor
	if b.Alertmanager == nil || len(inc.GroupLabels) == 0 {
		return toastExpired
	}
	// The mention makes the force reply selective: only the presser's reply
	// field opens. The name is user content and escaped.
	text := fmt.Sprintf(`⏱ <a href="tg://user?id=%d">%s</a>, reply with a duration and a reason, e.g. <code>3h kernel update</code> (at most 7d).`,
		user.ID, html.EscapeString(clipName(user.FirstName)))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, err := p.Telegram.SendMessage(ctx, telegram.Message{ChatID: inc.ChatID, ThreadID: inc.ThreadID, Text: text, ReplyTo: inc.MessageID, Silent: true, ForceReply: "3h kernel update"})
	if err != nil {
		log.Warn("sending the custom silence prompt failed", "err", err)
		return toastUnreachable
	}
	inc.PromptMessageIDs = append(inc.PromptMessageIDs, id)
	if err := p.Store.PutIncident(inc); err != nil {
		log.Error("storing incident failed", "err", err)
	}
	return toastCustomPrompt
}

// handleMessage acts on an admin's reply to an incident's message (or to
// its escalation replies or custom silence prompts) that asks for a custom
// silence (#7). Every other message is dropped without a trace: as a chat
// administrator the bot receives all of them (SECURITY.md §2).
func (b *Buttons) handleMessage(ctx context.Context, msg *telegram.IncomingMessage) {
	p := b.Processor
	if msg.ReplyToMessage == nil || !slices.Contains(b.ChatIDs, msg.Chat.ID) {
		return
	}
	to := msg.ReplyToMessage.MessageID
	onIncident := func(inc *store.Incident) bool {
		return inc.ChatID == msg.Chat.ID && (inc.MessageID == to || slices.Contains(inc.EscalationMessageIDs, to) || slices.Contains(inc.PromptMessageIDs, to))
	}
	incs, err := p.Store.Incidents(onIncident)
	if err != nil || len(incs) == 0 {
		return // not a reply to one of the relay's messages
	}
	if msg.From == nil || !slices.Contains(b.AdminUserIDs, msg.From.ID) {
		p.Metrics.UpdatesRejected.Inc()
		return
	}
	react := func(emoji string) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := p.Telegram.SetMessageReaction(ctx, msg.Chat.ID, msg.MessageID, emoji); err != nil {
			b.Log.Debug("reacting to a reply failed", "err", err)
		}
	}
	d, reason, ok := parseCustomSilence(msg.Text)
	if !ok || b.Alertmanager == nil {
		react(reactionPuzzled)
		return
	}

	defer p.lock(incs[0].GroupKey)()
	inc, err := p.Store.Incident(incs[0].GroupKey)
	if err != nil || inc == nil || !onIncident(inc) || len(inc.GroupLabels) == 0 {
		react(reactionCannotDo)
		return
	}
	var m alertmanager.Message
	if err := json.Unmarshal(inc.Last, &m); err != nil {
		react(reactionCannotDo)
		return
	}
	now := p.Now()
	log := b.Log.With("incident", groupID(inc.GroupKey), "action", "custom_silence", "user_id", msg.From.ID)
	if err := b.createSilence(ctx, log, inc, d, reason, *msg.From, now); err != nil {
		p.Metrics.ButtonPresses.WithLabelValues(store.ActionCustom, "error").Inc()
		react(reactionCannotDo)
		return
	}
	react(reactionDone)
	b.refresh(ctx, log, inc, &m, now)
	p.Metrics.ButtonPresses.WithLabelValues(store.ActionCustom, "ok").Inc()
}

package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// Commands the relay answers (SPEC §6).
const (
	cmdAlerts   = "alerts"
	cmdSilences = "silences"
)

// Commands is what the relay registers for each admin in each chat: as
// ephemeral commands, the command and its answer are seen only by the admin
// who sent it and the bot (#10).
var Commands = []telegram.Command{
	{Command: cmdAlerts, Description: "Firing alerts in this chat (only you see the answer)", Ephemeral: true},
	{Command: cmdSilences, Description: "Active silences (only you see the answer)", Ephemeral: true},
}

// endSilenceTTL is how long the ↩️ buttons of a /silences answer work.
const endSilenceTTL = 15 * time.Minute

// maxListedSilences bounds the /silences answer and its buttons.
const maxListedSilences = 20

// command reports which of the relay's commands text is: "/alerts" or
// "/alerts@thisbot", but not "/alerts@otherbot".
func (b *Buttons) command(text string) (string, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok {
		return "", false
	}
	if cmd, bot, found := strings.Cut(name, "@"); found {
		if b.BotUsername == "" || !strings.EqualFold(bot, b.BotUsername) {
			return "", false
		}
		name = cmd
	}
	name = strings.ToLower(name)
	if name != cmdAlerts && name != cmdSilences {
		return "", false
	}
	return name, true
}

// handleCommand answers an admin's /alerts or /silences with a message only
// they see. Commands from anyone else are ignored without an answer
// (SECURITY.md §2).
func (b *Buttons) handleCommand(ctx context.Context, msg *telegram.IncomingMessage, cmd string) {
	p := b.Processor
	if msg.From == nil || !slices.Contains(b.ChatIDs, msg.Chat.ID) || !slices.Contains(b.AdminUserIDs, msg.From.ID) {
		p.Metrics.UpdatesRejected.Inc()
		return
	}
	log := b.Log.With("action", "command_"+cmd, "user_id", msg.From.ID)
	var text string
	var kb *telegram.Keyboard
	var err error
	switch cmd {
	case cmdAlerts:
		text, err = b.alertsAnswer(msg.Chat.ID)
	case cmdSilences:
		text, kb, err = b.silencesAnswer(ctx, msg.Chat.ID, msg.From.ID)
	}
	if err != nil {
		log.Warn("answering a command failed", "err", err)
		text, kb = toastUnreachable, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := telegram.Message{ChatID: msg.Chat.ID, ThreadID: msg.ThreadID, Text: text, Keyboard: kb, Silent: true, EphemeralTo: msg.From.ID}
	if _, err := p.Telegram.SendMessage(ctx, out); err != nil {
		log.Warn("sending a command answer failed", "err", err)
		return
	}
	log.Info("command answered")
}

// alertsAnswer lists the chat's open incidents like the status board. Only
// this chat's: chats can have different audiences.
func (b *Buttons) alertsAnswer(chat int64) (string, error) {
	items, err := b.Processor.boardItems(chat)
	if err != nil {
		return "", err
	}
	return b.Processor.Render.Board(items, time.Time{})
}

// silencesAnswer lists Alertmanager's active silences. Only silences made
// from Telegram get an ↩️ button, so nobody ends someone's maintenance
// silence by accident; each button works only for the user who asked, in
// this chat, for endSilenceTTL.
func (b *Buttons) silencesAnswer(ctx context.Context, chat, user int64) (string, *telegram.Keyboard, error) {
	p := b.Processor
	if b.Alertmanager == nil {
		return "Silences need ALERTMANAGER_URL.", nil, nil
	}
	silences, err := b.callAlertmanagerList(ctx)
	if err != nil {
		return "", nil, err
	}
	names := p.silenceNames()
	now := p.Now()
	var items []render.SilenceItem
	var actions []*store.Action
	kb := &telegram.Keyboard{}
	for i, s := range silences {
		if i == maxListedSilences {
			break
		}
		item := render.SilenceItem{Matchers: matcherText(s.Matchers), Until: s.EndsAt, By: s.CreatedBy}
		if strings.HasPrefix(s.CreatedBy, "telegram:") {
			item.By = "Telegram"
			if name := names[s.ID]; name != "" {
				item.By = name + " via Telegram"
			}
			a := &store.Action{ID: newActionID(), Kind: store.ActionEndSilence, SilenceID: s.ID, ChatID: chat, UserID: user, ExpiresAt: now.Add(endSilenceTTL)}
			actions = append(actions, a)
			item.N = len(actions)
			kb.Rows = append(kb.Rows, []telegram.Button{{Text: fmt.Sprintf("↩️ End silence %d", item.N), Data: a.ID}})
		}
		items = append(items, item)
	}
	if err := p.Store.PutActions(actions...); err != nil {
		return "", nil, err
	}
	text, err := p.Render.Silences(items)
	if len(kb.Rows) == 0 {
		kb = nil
	}
	return text, kb, err
}

func (b *Buttons) callAlertmanagerList(ctx context.Context) ([]alertmanager.SilenceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.Alertmanager.ActiveSilences(ctx)
}

// silenceNames maps the IDs of silences made through incident buttons to
// who made them.
func (p *Processor) silenceNames() map[string]string {
	names := map[string]string{}
	incs, _ := p.Store.Incidents(func(inc *store.Incident) bool { return inc.Silence != nil })
	for _, inc := range incs {
		names[inc.Silence.ID] = inc.Silence.ByName
	}
	return names
}

func matcherText(ms []alertmanager.Matcher) string {
	var parts []string
	for _, m := range ms {
		op := map[[2]bool]string{{false, true}: "=", {false, false}: "!=", {true, true}: "=~", {true, false}: "!~"}[[2]bool{m.IsRegex, m.IsEqual}]
		parts = append(parts, fmt.Sprintf("%s%s%q", m.Name, op, m.Value))
	}
	return strings.Join(parts, ", ")
}

// endSilence carries out an ↩️ button of a /silences answer: only for the
// user it was sent to, in that chat, once. If the silence belongs to an open
// incident, its message is updated as after Unsilence.
func (b *Buttons) endSilence(ctx context.Context, cq *telegram.CallbackQuery, action *store.Action) {
	p := b.Processor
	if cq.Message == nil || cq.Message.Chat.ID != action.ChatID || cq.From.ID != action.UserID || b.Alertmanager == nil {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "stale").Inc()
		b.answer(ctx, cq, toastExpired)
		return
	}
	log := b.Log.With("action", action.Kind, "user_id", cq.From.ID)
	_, err := b.callAlertmanager(ctx, func(ctx context.Context) (string, error) {
		return "", b.Alertmanager.ExpireSilence(ctx, action.SilenceID)
	})
	if err != nil && !errors.Is(err, alertmanager.ErrSilenceNotFound) {
		p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "error").Inc()
		log.Warn("expiring silence failed", "silence_id", action.SilenceID, "err", err)
		b.answer(ctx, cq, toastUnreachable)
		return
	}
	if err := p.Store.DeleteActions(action.ID); err != nil {
		log.Warn("deleting a used action failed", "err", err)
	}
	log.Info("silence expired", "silence_id", action.SilenceID)
	b.answer(ctx, cq, toastUnsilenced)
	p.Metrics.ButtonPresses.WithLabelValues(action.Kind, "ok").Inc()

	incs, _ := p.Store.Incidents(func(inc *store.Incident) bool { return inc.Silence != nil && inc.Silence.ID == action.SilenceID })
	for _, found := range incs {
		func() {
			defer p.lock(found.GroupKey)()
			inc, err := p.Store.Incident(found.GroupKey)
			if err != nil || inc == nil || inc.Silence == nil || inc.Silence.ID != action.SilenceID {
				return
			}
			var m alertmanager.Message
			if json.Unmarshal(inc.Last, &m) != nil {
				return
			}
			inc.Silence = nil
			b.refresh(ctx, log.With("incident", groupID(inc.GroupKey)), inc, &m, p.Now())
		}()
	}
}

package incident

import (
	"context"
	"strings"
	"testing"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

func (e *env) reactions() string {
	e.tg.mu.Lock()
	defer e.tg.mu.Unlock()
	r := strings.Join(e.tg.reactions, ", ")
	e.tg.reactions = nil
	return r
}

func TestAckAndRelease(t *testing.T) {
	e := newButtonEnv(t)
	ack := button(t, e.handle(gmsg("a"))[0].Keyboard, "👀 I'm on it")

	toast, calls := e.press(ack, chat, 1, admin)
	if toast != toastAcked {
		t.Errorf("toast = %q", toast)
	}
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.Contains(calls[0].Text, "👀 Alice is on it since") {
		t.Errorf("text = %q", calls[0].Text)
	}
	if got := strings.Join(row(calls[0].Keyboard, 1), ","); got != "↩️ Release" {
		t.Errorf("row = %q", got)
	}
	if got := e.reactions(); got != "#1 👀" {
		t.Errorf("reactions = %q", got)
	}
	if a, _ := e.store.Action(ack, e.now); a != nil {
		t.Error("the used ack action is still valid")
	}
	if e.am.created != nil || e.am.expired != nil {
		t.Error("acknowledging must not touch Alertmanager")
	}

	// Any admin may release.
	toast, calls = e.press(button(t, calls[0].Keyboard, "↩️ Release"), chat, 1, admin)
	if toast != toastReleased || strings.Contains(calls[0].Text, "is on it") {
		t.Errorf("toast %q, text %q", toast, calls[0].Text)
	}
	if got := e.reactions(); got != "#1 " {
		t.Errorf("reactions = %q", got)
	}
}

func TestReactionShowsState(t *testing.T) {
	e := newButtonEnv(t)
	kb := e.handle(gmsg("a"))[0].Keyboard
	_, calls := e.press(button(t, kb, "👀 I'm on it"), chat, 1, admin)
	e.press(button(t, calls[0].Keyboard, "🔕 1h"), chat, 1, admin) // silenced beats acknowledged
	e.handle(gmsg("a-"))                                          // resolved beats both
	if got := e.reactions(); got != "#1 👀, #1 😴, #1 👌" {
		t.Errorf("reactions = %q", got)
	}
}

func TestStrangerCannotAck(t *testing.T) {
	e := newButtonEnv(t)
	ack := button(t, e.handle(gmsg("a"))[0].Keyboard, "👀 I'm on it")
	if toast, calls := e.press(ack, chat, 1, stranger); toast != "" || len(calls) != 0 {
		t.Errorf("toast %q, calls %v", toast, calls)
	}
	if inc, _ := e.store.Incident(gmsg().GroupKey); inc.Ack != nil {
		t.Error("stranger acknowledged")
	}
}

func reaction(chatID int64, messageID int, user *telegram.User, old []string, new ...string) telegram.Update {
	r := &telegram.MessageReaction{MessageID: messageID, User: user}
	r.Chat.ID = chatID
	for _, e := range old {
		r.OldReaction = append(r.OldReaction, telegram.ReactionType{Type: "emoji", Emoji: e})
	}
	for _, e := range new {
		r.NewReaction = append(r.NewReaction, telegram.ReactionType{Type: "emoji", Emoji: e})
	}
	return telegram.Update{MessageReaction: r}
}

func TestReactionAcks(t *testing.T) {
	e := newButtonEnv(t)
	e.handle(gmsg("a"))
	alice := &telegram.User{ID: admin, FirstName: "Alice"}
	ctx := context.Background()

	for name, u := range map[string]telegram.Update{
		"stranger":        reaction(chat, 1, &telegram.User{ID: stranger}, nil, "👀"),
		"anonymous":       reaction(chat, 1, nil, nil, "👀"),
		"other chat":      reaction(-1009999999999, 1, alice, nil, "👀"),
		"other message":   reaction(chat, 7, alice, nil, "👀"),
		"other emoji":     reaction(chat, 1, alice, nil, "🔥"),
		"👀 kept, 🔥 added": reaction(chat, 1, alice, []string{"👀"}, "👀", "🔥"),
	} {
		e.buttons.HandleUpdate(ctx, u)
		if calls := e.tg.take(); len(calls) > 0 {
			t.Errorf("%s: calls %v", name, calls)
		}
	}
	if inc, _ := e.store.Incident(gmsg().GroupKey); inc.Ack != nil {
		t.Fatal("acknowledged by an ignored reaction")
	}

	e.buttons.HandleUpdate(ctx, reaction(chat, 1, alice, nil, "👍"))
	calls := e.tg.take()
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.Contains(calls[0].Text, "👀 Alice is on it") {
		t.Errorf("text = %q", calls[0].Text)
	}
	if got := e.reactions(); got != "#1 👀" {
		t.Errorf("reactions = %q", got)
	}
	// A second reaction changes nothing.
	e.buttons.HandleUpdate(ctx, reaction(chat, 1, alice, []string{"👍"}, "👍", "👀"))
	if calls := e.tg.take(); len(calls) > 0 {
		t.Errorf("calls %v", calls)
	}
}

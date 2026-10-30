package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// BoardInterval is how often UpdateBoards should run. A board is edited at
// most once per run, and only when its text changed: well within
// Telegram's rate limits.
const BoardInterval = 30 * time.Second

// UpdateBoards keeps one pinned status board per chat in BoardChats (#6):
// the firing incidents, each linking to its message, or "All clear since".
// A board that was deleted is sent and pinned again.
func (p *Processor) UpdateBoards(ctx context.Context) {
	for _, chat := range p.BoardChats {
		if err := p.updateBoard(ctx, chat); err != nil {
			p.Log.Warn("updating the status board failed", "err", err)
		}
	}
}

// boardItems are the chat's firing incidents, oldest first, for the board
// and /alerts.
func (p *Processor) boardItems(chat int64) ([]render.BoardItem, error) {
	incs, err := p.Store.Incidents(func(inc *store.Incident) bool { return inc.ChatID == chat })
	if err != nil {
		return nil, err
	}
	slices.SortFunc(incs, func(a, b *store.Incident) int { return a.OpenedAt.Compare(b.OpenedAt) })
	var items []render.BoardItem
	for _, inc := range incs {
		var m alertmanager.Message
		if json.Unmarshal(inc.Last, &m) != nil || m.Status != alertmanager.StatusFiring {
			continue
		}
		items = append(items, render.BoardItem{Data: p.data(inc, &m, nil), URL: messageLink(chat, inc.ThreadID, inc.MessageID)})
	}
	return items, nil
}

func (p *Processor) updateBoard(ctx context.Context, chat int64) error {
	now := p.Now()
	items, err := p.boardItems(chat)
	if err != nil {
		return err
	}

	board, err := p.Store.Board(chat)
	if err != nil {
		return err
	}
	if board == nil {
		board = &store.Board{ChatID: chat}
	}
	switch {
	case len(items) > 0:
		board.AllClearSince = time.Time{}
	case board.AllClearSince.IsZero():
		board.AllClearSince = now
	}
	text, err := p.Render.Board(items, board.AllClearSince)
	if err != nil {
		p.Metrics.TemplateErrors.Inc()
		return fmt.Errorf("rendering: %w", err)
	}
	h := hash(text, nil)
	if board.MessageID != 0 && board.TextHash == h {
		return p.Store.PutBoard(board) // AllClearSince may have changed
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if board.MessageID != 0 {
		err = p.Telegram.EditMessageText(ctx, chat, board.MessageID, text, nil)
		if telegram.IsMessageGone(err) {
			p.Log.Info("status board was deleted, sending a new one")
			board.MessageID = 0
		} else if err != nil {
			return err
		}
	}
	if board.MessageID == 0 {
		// In a forum, a message without a topic goes to General.
		id, err := p.Telegram.SendMessage(ctx, telegram.Message{ChatID: chat, Text: text, Silent: true})
		if err != nil {
			return err
		}
		board.MessageID = id
		if err := p.Telegram.PinChatMessage(ctx, chat, id); err != nil {
			p.Log.Warn("pinning the status board failed (the bot needs the right to pin messages)", "err", err)
		}
	}
	board.TextHash = h
	return p.Store.PutBoard(board)
}

// messageLink links to a message in a supergroup (chat IDs -100…); other
// chats have no such links.
func messageLink(chat int64, thread, message int) string {
	const supergroup = -1_000_000_000_000
	if chat > supergroup {
		return ""
	}
	internal := -chat + supergroup
	if thread != 0 {
		return fmt.Sprintf("https://t.me/c/%d/%d/%d", internal, thread, message)
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", internal, message)
}

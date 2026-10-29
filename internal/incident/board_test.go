package incident

import (
	"context"
	"strings"
	"testing"
	"time"
)

func (e *env) boards(after time.Duration) []call {
	e.now = e.now.Add(after)
	e.proc.UpdateBoards(context.Background())
	return e.tg.take()
}

func TestStatusBoard(t *testing.T) {
	e := newEnv(t)
	e.proc.BoardChats = []int64{chat}

	calls := e.boards(0)
	expectCalls(t, calls, "sendMessage silent", "pinChatMessage(#1) silent")
	if !strings.HasPrefix(calls[0].Text, "✅ <b>All clear</b> since") || calls[0].ThreadID != 0 {
		t.Errorf("board = %+v", calls[0])
	}
	if calls := e.boards(30 * time.Second); len(calls) > 0 {
		t.Errorf("unchanged board edited: %v", calls)
	}

	e.handle(msg("a")) // message #2
	calls = e.boards(30 * time.Second)
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.Contains(calls[0].Text, "1 firing") || !strings.Contains(calls[0].Text, `<a href="https://t.me/c/1234567890/2">`) {
		t.Errorf("board = %q", calls[0].Text)
	}

	e.handle(msg("a-"))
	calls = e.boards(30 * time.Second)
	expectCalls(t, calls, "editMessageText(#1)")
	if !strings.HasPrefix(calls[0].Text, "✅ <b>All clear</b> since") {
		t.Errorf("board = %q", calls[0].Text)
	}
}

func TestStatusBoardRecreatedWhenDeleted(t *testing.T) {
	e := newEnv(t)
	e.proc.BoardChats = []int64{chat}
	e.boards(0)
	e.handle(msg("a"))
	e.tg.fail = func(c call, _ int) string {
		if c.Method == "editMessageText" && c.MessageID == 1 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
		}
		return ""
	}
	expectCalls(t, e.boards(30*time.Second), "editMessageText(#1)", "sendMessage silent", "pinChatMessage(#3) silent")
	e.tg.fail = nil
	if calls := e.boards(30 * time.Second); len(calls) > 0 {
		t.Errorf("recreated board edited again: %v", calls)
	}
}

func TestMessageLink(t *testing.T) {
	for _, tc := range []struct {
		chat            int64
		thread, message int
		want            string
	}{
		{-1001234567890, 0, 42, "https://t.me/c/1234567890/42"},
		{-1001234567890, 7, 42, "https://t.me/c/1234567890/7/42"},
		{-123456789, 0, 42, ""}, // basic group: no links
		{123456789, 0, 42, ""},  // private chat
	} {
		if got := messageLink(tc.chat, tc.thread, tc.message); got != tc.want {
			t.Errorf("messageLink(%d, %d, %d) = %q, want %q", tc.chat, tc.thread, tc.message, got, tc.want)
		}
	}
}

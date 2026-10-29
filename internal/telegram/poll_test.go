package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPoll(t *testing.T) {
	var mu sync.Mutex
	var offsets []int64
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Offset int64 `json:"offset"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		defer mu.Unlock()
		offsets = append(offsets, p.Offset)
		n++
		switch n {
		case 1:
			io.WriteString(w, `{"ok":true,"result":[{"update_id":10,"callback_query":{"id":"a","data":"x"}},{"update_id":11,"callback_query":{"id":"b","data":"y"}}]}`)
		case 2:
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`)
		case 3:
			io.WriteString(w, `{"ok":true,"result":[{"update_id":12,"callback_query":{"id":"c","data":"z"}}]}`)
		default:
			io.WriteString(w, `{"ok":true,"result":[]}`)
		}
	}))
	defer srv.Close()

	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	oks := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(srv.URL, testToken).Poll(ctx, slog.New(slog.NewTextHandler(&logs, nil)),
			func(_ context.Context, u Update) {
				got = append(got, u.CallbackQuery.ID)
				if len(got) == 3 {
					cancel()
				}
			},
			func() { oks++ })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("poll did not stop")
	}

	if strings.Join(got, ",") != "a,b,c" || oks != 2 {
		t.Errorf("handled %v, ok %d", got, oks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(offsets) < 3 || offsets[0] != 0 || offsets[1] != 12 || offsets[2] != 12 {
		t.Errorf("offsets = %v", offsets)
	}
	if !strings.Contains(logs.String(), "another relay uses this bot token") {
		t.Errorf("409 not explained in logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("token in logs")
	}
}

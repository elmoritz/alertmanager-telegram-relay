package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
)

// Dispatcher queues notifications durably and delivers them with one worker
// per chat, so a slow or rate-limited chat does not delay the others, and
// notifications for one chat keep their order.
type Dispatcher struct {
	Store     *store.Store
	Processor *Processor
	Log       *slog.Logger
	// Route picks the chat for a notification.
	Route func(*alertmanager.Message) int64

	mu      sync.Mutex
	ctx     context.Context
	workers map[int64]chan struct{}
	wg      sync.WaitGroup
}

// Start launches workers for chats that still have queued notifications from
// a previous run. Workers stop when ctx is done; Wait waits for them.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	d.ctx = ctx
	d.workers = map[int64]chan struct{}{}
	d.mu.Unlock()

	chats, err := d.Store.QueueChats()
	if err != nil {
		return err
	}
	for _, c := range chats {
		d.wake(c)
	}
	return nil
}

// Enqueue stores a notification for delivery. Once it returns nil, the
// notification survives a restart.
func (d *Dispatcher) Enqueue(m *alertmanager.Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding notification: %w", err)
	}
	chat := d.Route(m)
	if err := d.Store.Enqueue(chat, payload); err != nil {
		return err
	}
	d.wake(chat)
	return nil
}

// Drain waits until the queue is empty or ctx is done.
func (d *Dispatcher) Drain(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if n, err := d.Store.QueueLen(); err != nil || n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait waits for all workers to stop after the Start context is done.
func (d *Dispatcher) Wait() { d.wg.Wait() }

func (d *Dispatcher) wake(chat int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil || d.ctx.Err() != nil {
		return // not started or stopping: the job stays queued for the next start
	}
	ch, ok := d.workers[chat]
	if !ok {
		ch = make(chan struct{}, 1)
		d.workers[chat] = ch
		d.wg.Add(1)
		go d.work(d.ctx, chat, ch)
	}
	select {
	case ch <- struct{}{}:
	default: // already signalled
	}
}

func (d *Dispatcher) work(ctx context.Context, chat int64, wake <-chan struct{}) {
	defer d.wg.Done()
	backoff := time.Second
	for ctx.Err() == nil {
		job, err := d.Store.Next(chat)
		if err == nil && job == nil {
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		if err == nil {
			err = d.handle(ctx, job)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.Log.Error("delivery failed, will retry", "err", err, "in", backoff.String())
			if SleepContext(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
	}
}

// handle processes one queued job and removes it unless it must be retried.
func (d *Dispatcher) handle(ctx context.Context, job *store.Job) error {
	var m alertmanager.Message
	if err := json.Unmarshal(job.Payload, &m); err != nil {
		d.Log.Error("dropping unreadable queued notification", "err", err)
		return d.Store.Done(job)
	}
	if err := d.Processor.Handle(ctx, job.ChatID, &m); err != nil {
		return err
	}
	return d.Store.Done(job)
}

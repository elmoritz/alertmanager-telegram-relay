package incident

import (
	"context"
	"log/slog"
	"time"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
)

// graphTimeout bounds rendering and sending one graph image.
const graphTimeout = time.Minute

// sendGraph renders the new incident's panel_url annotation and sends it as
// a silent photo reply, in the background: strictly best-effort, it never
// delays or blocks the text message (SPEC §8). Failures are counted and
// logged without the URL, which is alert content.
func (p *Processor) sendGraph(ctx context.Context, log *slog.Logger, inc *store.Incident, m *alertmanager.Message) {
	if p.Graphs == nil {
		return
	}
	panel := render.HTTPURL(annotation(m, "panel_url"))
	if panel == "" {
		return
	}
	chat, thread, replyTo := inc.ChatID, inc.ThreadID, inc.MessageID
	p.graphs.Add(1)
	go func() {
		defer p.graphs.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), graphTimeout)
		defer cancel()
		img, err := p.Graphs.RenderPanel(ctx, panel)
		if err != nil {
			p.Metrics.GraphImages.WithLabelValues("render_error").Inc()
			log.Warn("graph image not rendered", "err", err)
			return
		}
		if _, err := p.Telegram.SendPhoto(ctx, telegram.Photo{ChatID: chat, ThreadID: thread, ReplyTo: replyTo, Silent: true, PNG: img}); err != nil {
			p.Metrics.GraphImages.WithLabelValues("send_error").Inc()
			log.Warn("graph image not sent", "err", err)
			return
		}
		p.Metrics.GraphImages.WithLabelValues("ok").Inc()
	}()
}

// WaitGraphs waits for graph images still being rendered or sent.
func (p *Processor) WaitGraphs() { p.graphs.Wait() }

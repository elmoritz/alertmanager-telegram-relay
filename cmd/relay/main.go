// Command relay receives Alertmanager webhooks and posts them to Telegram.
//
// Configuration is read from the environment only (see docs/SPEC.md §10).
// "relay healthcheck" queries a running relay's /healthz and exits 0 or 1,
// for container health checks in an image without a shell or curl.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/elmoritz/alertmanager-telegram-relay/internal/alertmanager"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/config"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/incident"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/metrics"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/render"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/store"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/telegram"
	"github.com/elmoritz/alertmanager-telegram-relay/internal/webhook"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

const (
	retryFor      = 10 * time.Minute // SPEC §9
	shutdownGrace = 10 * time.Second
	actionTTL     = 7 * 24 * time.Hour // SECURITY.md §3
	pollStale     = 2 * time.Minute    // SPEC §11
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	level := new(slog.LevelVar)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, log, level, telegram.DefaultBaseURL, nil); err != nil {
		log.Error("relay stopped", "err", err)
		os.Exit(1)
	}
}

// run serves until ctx is done. ready, if not nil, receives the listening
// address (tests listen on port 0).
func run(ctx context.Context, log *slog.Logger, level *slog.LevelVar, telegramURL string, ready chan<- string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	level.Set(cfg.LogLevel)
	log.Info("starting", "version", version, "config", cfg)

	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer st.Close()

	renderer, err := render.New(cfg.TemplateFile, cfg.Location, nil)
	if err != nil {
		return err
	}

	m := metrics.New(
		func() float64 { n, _ := st.QueueLen(); return float64(n) },
		func() float64 { n, _ := st.CountIncidents(); return float64(n) },
	)
	tg := telegram.New(telegramURL, cfg.BotToken)
	tg.ProtectContent = cfg.ProtectContent
	tg.OnCall = func(method, result string) { m.TelegramRequests.WithLabelValues(method, result).Inc() }

	if err := checkBot(tg, log); err != nil {
		return err
	}

	runCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()

	processor := &incident.Processor{
		Store:    st,
		Telegram: tg,
		Render:   renderer,
		Metrics:  m,
		Log:      log,
		Buttons: incident.ButtonConfig{
			Silences:   cfg.AlertmanagerURL != nil,
			Durations:  cfg.SilenceDurations,
			ActionTTL:  actionTTL,
			GrafanaURL: cfg.GrafanaURL,
			ShowSource: cfg.ShowSourceButton,
		},
		Reminders:    cfg.Reminders,
		ResolveReply: cfg.ResolveReply,
		Router:       cfg.Router(),
		Quiet:        cfg.Quiet(),
		RetryFor:     retryFor,
		Now:          time.Now,
		Sleep:        incident.SleepContext,
	}
	buttons := &incident.Buttons{
		Processor:    processor,
		Telegram:     tg,
		ChatIDs:      cfg.ChatIDs,
		AdminUserIDs: cfg.AdminUserIDs,
		Log:          log,
	}
	if cfg.AlertmanagerURL != nil {
		am := alertmanager.NewClient(cfg.AlertmanagerURL, cfg.AlertmanagerUsername, cfg.AlertmanagerPassword, cfg.AlertmanagerToken)
		am.OnCall = func(op, result string) { m.AlertmanagerRequests.WithLabelValues(op, result).Inc() }
		buttons.Alertmanager = am
	}
	dispatcher := &incident.Dispatcher{
		Store:     st,
		Log:       log,
		Processor: processor,
	}
	if err := dispatcher.Start(runCtx); err != nil {
		return err
	}
	buttons.Enqueue = dispatcher.Enqueue
	go expire(runCtx, st, cfg.IncidentTTL, log)
	if buttons.Alertmanager != nil {
		go checkSilenced(runCtx, buttons)
	}

	// Button presses arrive by long polling: no inbound connection needed.
	var lastPoll atomic.Int64
	lastPoll.Store(time.Now().Unix()) // grace period for the first poll
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		tg.Poll(runCtx, log, buttons.HandleUpdate, func() { lastPoll.Store(time.Now().Unix()) })
	}()
	defer func() { stopWorkers(); <-pollDone }()

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhook.New(cfg.WebhookSecret, dispatcher, m, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(); err != nil {
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		if time.Since(time.Unix(lastPoll.Load(), 0)) > pollStale {
			http.Error(w, "telegram polling failing", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String())
	if ready != nil {
		ready <- ln.Addr().String()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		return err
	}

	// Stop taking webhooks, give queued notifications up to shutdownGrace to
	// go out; anything left stays in the store for the next start.
	graceCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(graceCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	dispatcher.Drain(graceCtx)
	stopWorkers()
	dispatcher.Wait()
	if n, _ := st.QueueLen(); n > 0 {
		log.Warn("notifications left queued for the next start", "count", n)
	}
	return nil
}

// checkBot verifies the token. A rejected token stops the relay; a network
// problem does not (deliveries are retried, and Telegram may be back soon).
func checkBot(tg *telegram.Client, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	me, err := tg.GetMe(ctx)
	if err != nil {
		var e *telegram.Error
		if errors.As(err, &e) && (e.Code == http.StatusUnauthorized || e.Code == http.StatusNotFound) {
			return fmt.Errorf("telegram rejected the bot token: %w", err)
		}
		log.Warn("could not reach telegram at startup", "err", err)
		return nil
	}
	log.Info("telegram bot ready", "bot", me.Username)
	return nil
}

// checkSilenced looks after incidents silenced from Telegram, whose resolve
// Alertmanager does not report (SPEC §5).
func checkSilenced(ctx context.Context, b *incident.Buttons) {
	t := time.NewTicker(incident.SilencedCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.CheckSilenced(ctx)
		}
	}
}

// expire forgets incidents after INCIDENT_TTL and button actions once they
// expire, hourly.
func expire(ctx context.Context, st *store.Store, ttl time.Duration, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.ExpireIncidents(time.Now().Add(-ttl)); err != nil {
			log.Error("expiring incidents", "err", err)
		} else if n > 0 {
			log.Info("expired incidents", "count", n)
		}
		if _, err := st.ExpireActions(time.Now()); err != nil {
			log.Error("expiring button actions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// healthcheck calls /healthz on LISTEN_ADDR, using loopback when the relay
// listens on all interfaces.
func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9095"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "LISTEN_ADDR:", err)
		return 1
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}

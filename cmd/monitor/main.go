// Command monitor watches the pipeline from outside for silent failures (docs/design.md,
// section 8.1): every few seconds it takes a reading of the stream, the consumers and the
// database, evaluates the alert rules, logs alerts as they start and end, and serves a status
// page on a loopback address. It also sends the canary webhook.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/env"
	"github.com/cferrante/call-event-pipeline/internal/monitor"
)

func main() {
	if err := run(); err != nil {
		slog.Error("monitor stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, cfg.dbURL)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", env.Redact(cfg.dbURL), err)
	}
	defer db.Close()
	nc, err := nats.Connect(cfg.natsURL, nats.Name("monitor"))
	if err != nil {
		return fmt.Errorf("connect to NATS at %s: %w", cfg.natsURL, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	sampler, err := monitor.NewSampler(ctx, js, db, monitor.CanaryFrom)
	if err != nil {
		return err
	}
	svc := monitor.NewService(sampler, cfg.rules, logAlert)

	go every(ctx, cfg.interval, func() { svc.Tick(ctx, time.Now()) })
	if cfg.canaryInterval > 0 {
		canary := monitor.Canary{Target: cfg.ingestURL, PublicURL: cfg.publicURL, AuthToken: cfg.authToken,
			Client: &http.Client{Timeout: 5 * time.Second}}
		go every(ctx, cfg.canaryInterval, func() {
			// A failed canary is only logged here: the arrivals-stopped rule is what alerts,
			// so one alert path covers both a dead edge and a broken pipeline behind it.
			if err := canary.Send(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("canary not accepted", "err", err)
			}
		})
	}

	srv := &http.Server{Addr: cfg.addr, Handler: handler(svc), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("monitor running", "status_page", "http://"+cfg.addr, "interval", cfg.interval,
		"canary_interval", cfg.canaryInterval)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// logAlert is the always-on alerter: one structured line when an alert starts and one when it
// ends.
func logAlert(e monitor.Event) {
	attrs := []any{"rule", e.Rule, "subject", e.Subject, "detail", e.Detail}
	if e.State == monitor.Firing {
		slog.Warn("ALERT firing", attrs...)
		return
	}
	slog.Info("ALERT resolved", append(attrs, "after", e.Duration.Round(time.Second))...)
}

// every runs f now and then at each interval until ctx ends.
func every(ctx context.Context, interval time.Duration, f func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

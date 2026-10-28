// Command attribution consumes lead events from JetStream and records them in Postgres.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/decoupled-ingestion-layer/internal/attribution"
	"github.com/cferrante/decoupled-ingestion-layer/internal/consumer"
	"github.com/cferrante/decoupled-ingestion-layer/internal/migrate"
	"github.com/cferrante/decoupled-ingestion-layer/internal/stream"
	"github.com/cferrante/decoupled-ingestion-layer/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("attribution stopped", "err", err)
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

	db, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	defer db.Close()
	// Safe to run from several instances at once: migrate takes an advisory lock.
	applied, err := migrate.Up(ctx, db, migrations.FS)
	if err != nil {
		return err
	}
	for _, name := range applied {
		slog.Info("applied migration", "migration", name)
	}

	closed := make(chan struct{})
	nc, err := nats.Connect(cfg.natsURL, nats.Name("attribution"),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	if err != nil {
		return fmt.Errorf("connect to NATS at %s: %w", cfg.natsURL, err)
	}
	// Drain only starts closing; wait until it's done so the last acks are sent before exit.
	defer func() {
		if err := nc.Drain(); err == nil {
			<-closed
		}
	}()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	if err := stream.EnsureStream(ctx, js); err != nil {
		return err
	}
	cons, err := consumer.EnsureConsumer(ctx, js, consumer.Attribution)
	if err != nil {
		return err
	}

	slog.Info("attribution consuming", "config", cfg)
	handler := consumer.NewHandler(consumer.Attribution.Name, js, attribution.NewStore(db))
	if err := consumer.Run(ctx, cons, handler, consumer.Attribution.Workers); err != nil {
		return err
	}
	slog.Info("attribution stopped cleanly")
	return nil
}

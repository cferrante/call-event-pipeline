// Command inference classifies lead events with an AI model (a stub in milestone 2) and stores
// the results, never sending the model more work at once than it can serve.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/consumer"
	"github.com/cferrante/call-event-pipeline/internal/inference"
	"github.com/cferrante/call-event-pipeline/internal/migrate"
	"github.com/cferrante/call-event-pipeline/internal/stream"
	"github.com/cferrante/call-event-pipeline/migrations"
)

func main() {
	reset := flag.Bool("reset-consumer", false,
		"delete the inference consumer and recreate it; it then starts at new messages")
	flag.Parse()
	if err := run(*reset); err != nil {
		slog.Error("inference stopped", "err", err)
		os.Exit(1)
	}
}

func run(resetConsumer bool) error {
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
	nc, err := nats.Connect(cfg.natsURL, nats.Name("inference"),
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
	consumerCfg := consumer.Inference(cfg.workers)
	if resetConsumer {
		slog.Warn("resetting consumer: its progress is discarded", "consumer", consumerCfg.Name)
		if err := consumer.ResetConsumer(ctx, js, consumerCfg.Name); err != nil {
			return err
		}
	}
	cons, err := consumer.EnsureConsumer(ctx, js, consumerCfg)
	if err != nil {
		return err
	}

	slog.Info("inference consuming", "config", cfg)
	recorder := inference.NewRecorder(inference.NewStub(cfg.model), inference.NewStore(db))
	handler := consumer.NewHandler(consumerCfg.Name, js, recorder)
	if err := consumer.Run(ctx, cons, handler, consumerCfg.Workers); err != nil {
		return err
	}
	slog.Info("inference stopped cleanly")
	return nil
}

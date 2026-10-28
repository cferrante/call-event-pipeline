// Command migrate applies any pending database migrations to DATABASE_URL.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/decoupled-ingestion-layer/internal/env"
	"github.com/cferrante/decoupled-ingestion-layer/internal/migrate"
	"github.com/cferrante/decoupled-ingestion-layer/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	url, err := env.DatabaseURL()
	if err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := migrate.Up(ctx, db, migrations.FS)
	for _, name := range applied {
		slog.Info("applied", "migration", name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		slog.Info("database is up to date")
	}
	return nil
}

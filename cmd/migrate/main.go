// Command migrate applies any pending database migrations to DATABASE_URL.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	// Without this check pgx falls back to libpq defaults (your own login, which may be a
	// superuser), so a missing variable could migrate the wrong database.
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is not set; run: set -a; source .env; set +a")
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

// Package migrate applies numbered SQL files to a Postgres database, each exactly once.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockID is an arbitrary constant naming this package's Postgres advisory lock.
const lockID = 7_215_310_042

// Up applies every *.sql file in fsys that hasn't been applied yet, in name order, and
// returns the names it applied. Each file runs in its own transaction together with its
// row in schema_migrations, so a failing file leaves no trace and stops the run.
// Concurrent callers (e.g. two services starting at once) take turns on an advisory lock.
func Up(ctx context.Context, db *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	slices.Sort(names)

	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	var applied []string
	for _, name := range names {
		done, err := apply(ctx, conn.Conn(), fsys, name)
		if err != nil {
			return applied, err
		}
		if done {
			applied = append(applied, name)
		}
	}
	return applied, nil
}

// apply runs one migration file unless it is already recorded, reporting whether it ran.
func apply(ctx context.Context, conn *pgx.Conn, fsys fs.FS, name string) (bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check %s: %w", name, err)
	}
	if exists {
		return false, nil
	}

	sql, err := fs.ReadFile(fsys, path.Clean(name))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("apply %s: %w", name, err)
	}
	return true, nil
}

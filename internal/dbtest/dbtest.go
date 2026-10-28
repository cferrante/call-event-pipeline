// Package dbtest gives each integration test its own throwaway Postgres database.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/decoupled-ingestion-layer/internal/env"
	"github.com/cferrante/decoupled-ingestion-layer/internal/migrate"
	"github.com/cferrante/decoupled-ingestion-layer/migrations"
)

// Empty creates a new, empty database named leads_test_<random> using DATABASE_URL's
// credentials, and drops it when the test ends. The test is skipped if DATABASE_URL
// isn't set.
func Empty(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run: set -a; source .env; set +a")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", env.Redact(baseURL), err)
	}
	name := "leads_test_" + randomHex(t)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = admin.Close(ctx)
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close) // runs before the drop: cleanups run last-in, first-out
	return pool
}

// Migrated is Empty with the project's migrations applied.
func Migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := Empty(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := migrate.Up(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random name: %v", err)
	}
	return hex.EncodeToString(b)
}

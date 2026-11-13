package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/env"
)

// config is the attribution service's settings, read from environment variables.
type config struct {
	databaseURL string // DATABASE_URL
	natsURL     string // NATS_URL
}

func loadConfig() (config, error) {
	dbURL, err := env.DatabaseURL()
	if err != nil {
		return config{}, err
	}
	return config{databaseURL: dbURL, natsURL: env.NATSURL()}, nil
}

// LogValue lets the config be logged with the database password hidden.
func (c config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_url", env.Redact(c.databaseURL)),
		slog.String("nats_url", c.natsURL),
	)
}

// parseReplayFrom reads -replay-from: an RFC 3339 time with its zone, such as
// 2026-11-13T09:30:00Z, so a replay never starts an hour off.
func parseReplayFrom(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("-replay-from must be an RFC 3339 time such as 2026-11-13T09:30:00Z, got %q", s)
	}
	return t, nil
}

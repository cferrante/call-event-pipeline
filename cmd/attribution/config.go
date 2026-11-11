package main

import (
	"log/slog"

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

package main

import (
	"errors"
	"log/slog"
	"net/url"
	"os"
)

const defaultNATSURL = "nats://127.0.0.1:4222"

// config is the attribution service's settings, read from environment variables.
type config struct {
	databaseURL string // DATABASE_URL
	natsURL     string // NATS_URL
}

func loadConfig() (config, error) {
	cfg := config{databaseURL: os.Getenv("DATABASE_URL"), natsURL: os.Getenv("NATS_URL")}
	if cfg.databaseURL == "" {
		// Without it, pgx falls back to libpq defaults: your own, possibly superuser, login.
		return config{}, errors.New("DATABASE_URL is required; run: set -a; source .env; set +a")
	}
	if cfg.natsURL == "" {
		cfg.natsURL = defaultNATSURL
	}
	return cfg, nil
}

// LogValue lets the config be logged with the database password hidden.
func (c config) LogValue() slog.Value {
	db := "(unparseable)"
	if u, err := url.Parse(c.databaseURL); err == nil {
		db = u.Redacted()
	}
	return slog.GroupValue(slog.String("database_url", db), slog.String("nats_url", c.natsURL))
}

// Package env reads the settings the commands share from environment variables.
package env

import (
	"errors"
	"net/url"
	"os"
)

// DefaultNATSURL is the local NATS server started as in docs/running.md.
const DefaultNATSURL = "nats://127.0.0.1:4222"

// NATSURL returns NATS_URL, or the local default.
func NATSURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return DefaultNATSURL
}

// DatabaseURL returns DATABASE_URL, which is required: without it pgx falls back to libpq's
// defaults, i.e. your own (possibly superuser) login, and could touch the wrong database.
func DatabaseURL() (string, error) {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		return "", errors.New("DATABASE_URL is not set; run: set -a; source .env; set +a")
	}
	return u, nil
}

// Redact returns rawURL with any password replaced, safe to log.
func Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "(unparseable URL)"
	}
	return u.Redacted()
}

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
)

const (
	defaultPort    = 8080
	defaultNATSURL = "nats://127.0.0.1:4222"
)

// config is the ingest service's settings, read from environment variables.
type config struct {
	addr      string // INGEST_PORT, as a listen address
	authToken string // TWILIO_AUTH_TOKEN
	publicURL string // INGEST_PUBLIC_URL, without a trailing slash
	natsURL   string // NATS_URL
	forwardTo string // INGEST_FORWARD_TO, optional
}

// loadConfig reads and validates the environment, so a mistake stops the service at
// startup with a clear message rather than failing on the first webhook.
func loadConfig() (config, error) {
	addr, err := listenAddr()
	if err != nil {
		return config{}, err
	}
	cfg := config{
		addr:      addr,
		authToken: os.Getenv("TWILIO_AUTH_TOKEN"),
		natsURL:   os.Getenv("NATS_URL"),
		forwardTo: os.Getenv("INGEST_FORWARD_TO"),
	}
	if cfg.authToken == "" {
		return config{}, errors.New("TWILIO_AUTH_TOKEN is required")
	}
	if cfg.publicURL, err = parsePublicURL(os.Getenv("INGEST_PUBLIC_URL")); err != nil {
		return config{}, err
	}
	if cfg.natsURL == "" {
		cfg.natsURL = defaultNATSURL
	}
	if cfg.forwardTo != "" && !envelope.IsE164(cfg.forwardTo) {
		return config{}, fmt.Errorf("INGEST_FORWARD_TO must be an E.164 number like +14158675310, got %q", cfg.forwardTo)
	}
	return cfg, nil
}

// parsePublicURL accepts only scheme://host[:port]: the handler appends the request path,
// so anything else would make every signature check fail.
func parsePublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("INGEST_PUBLIC_URL must look like https://hooks.example.com (no path), got %q", raw)
	}
	return u.String(), nil
}

// LogValue lets the config be logged without its secret.
func (c config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.addr),
		slog.String("public_url", c.publicURL),
		slog.String("nats_url", c.natsURL),
		slog.String("forward_to", c.forwardTo),
	)
}

// listenAddr returns the HTTP listen address from INGEST_PORT, defaulting to :8080.
// An invalid port is an error so a typo fails at startup rather than at bind time.
func listenAddr() (string, error) {
	raw := os.Getenv("INGEST_PORT")
	if raw == "" {
		return fmt.Sprintf(":%d", defaultPort), nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("INGEST_PORT must be a number from 1 to 65535, got %q", raw)
	}
	return fmt.Sprintf(":%d", port), nil
}

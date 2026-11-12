package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/env"
	"github.com/cferrante/call-event-pipeline/internal/monitor"
)

// config is the monitor's settings, read from environment variables.
type config struct {
	addr           string        // MONITOR_ADDR: status page, loopback only
	interval       time.Duration // MONITOR_INTERVAL: how often to take a reading
	canaryInterval time.Duration // CANARY_INTERVAL: how often to send the canary; 0 turns it off
	rules          monitor.Config
	dbURL, natsURL string
	ingestURL      string // INGEST_URL: where to send the canary
	publicURL      string // INGEST_PUBLIC_URL: what the canary's signature covers
	authToken      string // TWILIO_AUTH_TOKEN
}

// loadConfig reads and validates the environment, so a mistake stops the monitor at startup.
func loadConfig() (config, error) {
	cfg := config{
		addr:      getenv("MONITOR_ADDR", "127.0.0.1:8090"),
		rules:     monitor.DefaultConfig,
		natsURL:   env.NATSURL(),
		publicURL: strings.TrimSuffix(os.Getenv("INGEST_PUBLIC_URL"), "/"),
		authToken: os.Getenv("TWILIO_AUTH_TOKEN"),
		ingestURL: getenv("INGEST_URL", "http://127.0.0.1:"+getenv("INGEST_PORT", "8080")),
	}
	var err error
	if cfg.dbURL, err = env.DatabaseURL(); err != nil {
		return config{}, err
	}
	if err := loopbackOnly(cfg.addr); err != nil {
		return config{}, err
	}
	if cfg.interval, err = duration("MONITOR_INTERVAL", 5*time.Second, false); err != nil {
		return config{}, err
	}
	if cfg.canaryInterval, err = duration("CANARY_INTERVAL", 10*time.Second, true); err != nil {
		return config{}, err
	}
	if cfg.rules.CanaryMaxAge, err = duration("CANARY_MAX_AGE", cfg.rules.CanaryMaxAge, false); err != nil {
		return config{}, err
	}
	if cfg.canaryInterval > 0 && (cfg.authToken == "" || cfg.publicURL == "") {
		return config{}, fmt.Errorf("the canary needs TWILIO_AUTH_TOKEN and INGEST_PUBLIC_URL (or CANARY_INTERVAL=0 to turn it off)")
	}
	return cfg, nil
}

// loopbackOnly refuses any listen address beyond this machine: the status page has no login,
// so on a server it is reached through a port forward, never directly.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("MONITOR_ADDR must be host:port, got %q", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("MONITOR_ADDR must be a loopback address such as 127.0.0.1:8090 (the status page has no login), got %q", addr)
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// duration reads a Go duration such as 5s; zero is allowed only where it means "off".
func duration(key string, def time.Duration, zeroOK bool) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || (d == 0 && !zeroOK) {
		return 0, fmt.Errorf("%s must be a positive duration such as 5s, got %q", key, raw)
	}
	return d, nil
}

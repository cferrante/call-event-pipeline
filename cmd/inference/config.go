package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/cferrante/decoupled-ingestion-layer/internal/env"
	"github.com/cferrante/decoupled-ingestion-layer/internal/inference"
)

// config is the inference service's settings, read from environment variables.
type config struct {
	databaseURL string // DATABASE_URL
	natsURL     string // NATS_URL
	// workers is INFERENCE_WORKERS: messages handled at once, and the consumer's MaxAckPending.
	// It defaults to the model's capacity, so the model is never sent more than it can serve.
	workers int
	model   inference.StubConfig // MODEL_CAPACITY, MODEL_LATENCY, MODEL_TIMEOUT
}

func loadConfig() (config, error) {
	dbURL, err := env.DatabaseURL()
	if err != nil {
		return config{}, err
	}
	cfg := config{databaseURL: dbURL, natsURL: env.NATSURL(), model: inference.DefaultStubConfig}
	if cfg.model.Capacity, err = positiveInt("MODEL_CAPACITY", cfg.model.Capacity); err != nil {
		return config{}, err
	}
	if cfg.workers, err = positiveInt("INFERENCE_WORKERS", cfg.model.Capacity); err != nil {
		return config{}, err
	}
	if cfg.model.BaseLatency, err = positiveDuration("MODEL_LATENCY", cfg.model.BaseLatency); err != nil {
		return config{}, err
	}
	if cfg.model.Timeout, err = positiveDuration("MODEL_TIMEOUT", cfg.model.Timeout); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func positiveInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1, got %q", key, raw)
	}
	return n, nil
}

func positiveDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 200ms or 2s, got %q", key, raw)
	}
	return d, nil
}

// LogValue lets the config be logged with the database password hidden.
func (c config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_url", env.Redact(c.databaseURL)),
		slog.String("nats_url", c.natsURL),
		slog.Int("workers", c.workers),
		slog.Int("model_capacity", c.model.Capacity),
		slog.Duration("model_latency", c.model.BaseLatency),
		slog.Duration("model_timeout", c.model.Timeout),
	)
}

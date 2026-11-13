package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/env"
	"github.com/cferrante/call-event-pipeline/internal/inference"
)

// config is the inference service's settings, read from environment variables.
type config struct {
	databaseURL string // DATABASE_URL
	natsURL     string // NATS_URL
	// workers is INFERENCE_WORKERS: messages handled at once, and the consumer's MaxAckPending.
	// It defaults to the model's capacity, so the model is never sent more than it can serve.
	workers int
	model   inference.StubConfig // MODEL_CAPACITY, MODEL_LATENCY, MODEL_TIMEOUT

	modelKind     string               // MODEL: stub (default) or groq
	groq          inference.GroqConfig // GROQ_*; used when modelKind is groq
	groqPerMinute int                  // GROQ_TOKENS_PER_MINUTE
	groqPerDay    int                  // GROQ_TOKENS_PER_DAY
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
	switch cfg.modelKind = getenv("MODEL", "stub"); cfg.modelKind {
	case "stub":
	case "groq":
		if err := loadGroq(&cfg); err != nil {
			return config{}, err
		}
	default:
		return config{}, fmt.Errorf("MODEL must be stub or groq, got %q", cfg.modelKind)
	}
	return cfg, nil
}

// loadGroq reads the Groq adapter's settings. The key is read from the environment, which the
// runner fills from SSM Parameter Store, so it never sits in a file.
func loadGroq(cfg *config) error {
	g := inference.GroqConfig{
		BaseURL:             getenv("GROQ_BASE_URL", "https://api.groq.com"),
		APIKey:              os.Getenv("GROQ_API_KEY"),
		Model:               getenv("GROQ_MODEL", "openai/gpt-oss-20b"),
		MaxCompletionTokens: 256,
		MaxRetries:          2,
		Timeout:             10 * time.Second,
	}
	if g.APIKey == "" {
		return fmt.Errorf("MODEL=groq needs GROQ_API_KEY")
	}
	// gpt-oss reasons before answering; "low" keeps that, and its token cost, small.
	effort := ""
	if strings.HasPrefix(g.Model, "openai/gpt-oss") {
		effort = "low"
	}
	g.ReasoningEffort = getenv("GROQ_REASONING_EFFORT", effort)
	var err error
	// Free tier: 8,000 tokens a minute. 90% leaves room for anything else using the same key.
	if cfg.groqPerMinute, err = positiveInt("GROQ_TOKENS_PER_MINUTE", 7200); err != nil {
		return err
	}
	if cfg.groqPerDay, err = positiveInt("GROQ_TOKENS_PER_DAY", 100_000); err != nil {
		return err
	}
	cfg.groq = g
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
		slog.String("model", c.modelKind),
		slog.String("groq_model", c.groq.Model),
		slog.Int("groq_tokens_per_minute", c.groqPerMinute),
		slog.Int("groq_tokens_per_day", c.groqPerDay),
	)
}

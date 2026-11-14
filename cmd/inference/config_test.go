package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/inference"
)

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	for _, k := range []string{"NATS_URL", "INFERENCE_WORKERS", "MODEL_CAPACITY", "MODEL_LATENCY", "MODEL_TIMEOUT",
		"MODEL_JITTER", "MODEL", "GROQ_API_KEY", "GROQ_MODEL", "GROQ_REASONING_EFFORT", "GROQ_TOKENS_PER_MINUTE", "GROQ_TOKENS_PER_DAY"} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.workers != 4 || cfg.model != inference.DefaultStubConfig || cfg.natsURL != "nats://127.0.0.1:4222" {
		t.Errorf("config = %+v, want 4 workers, the default stub and local NATS", cfg)
	}
}

func TestLoadConfigReadsValues(t *testing.T) {
	setValidEnv(t)
	t.Setenv("INFERENCE_WORKERS", "8")
	t.Setenv("MODEL_CAPACITY", "6")
	t.Setenv("MODEL_LATENCY", "50ms")
	t.Setenv("MODEL_TIMEOUT", "1s")
	t.Setenv("MODEL_JITTER", "0.1")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.workers != 8 || cfg.model.Capacity != 6 || cfg.model.BaseLatency != 50*time.Millisecond ||
		cfg.model.Timeout != time.Second || cfg.model.Jitter != 0.1 {
		t.Errorf("config = %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"DATABASE_URL", ""},
		{"INFERENCE_WORKERS", "0"},
		{"INFERENCE_WORKERS", "many"},
		{"MODEL_CAPACITY", "-1"},
		{"MODEL_LATENCY", "fast"},
		{"MODEL_LATENCY", "0s"},
		{"MODEL_TIMEOUT", "-1s"},
		{"MODEL_JITTER", "1"},
		{"MODEL_JITTER", "-0.1"},
		{"MODEL_JITTER", "lots"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tt.key, tt.value)

			if cfg, err := loadConfig(); err == nil {
				t.Errorf("loadConfig succeeded: %+v", cfg)
			}
		})
	}
}

func TestConfigLogValueHidesDatabasePassword(t *testing.T) {
	setValidEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := cfg.LogValue().String(); strings.Contains(got, "s3cret") || !strings.Contains(got, "workers=4") {
		t.Errorf("logged config = %s, want workers shown and the password hidden", got)
	}
}

// Workers should match the model's capacity, so by default they follow it.
func TestWorkersDefaultToModelCapacity(t *testing.T) {
	setValidEnv(t)
	t.Setenv("MODEL_CAPACITY", "8")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.workers != 8 {
		t.Errorf("workers = %d, want 8 (MODEL_CAPACITY)", cfg.workers)
	}
}

func TestLoadConfigGroq(t *testing.T) {
	setValidEnv(t)
	t.Setenv("MODEL", "groq")
	t.Setenv("GROQ_API_KEY", "gsk_secret-value")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	g := cfg.groq
	if cfg.modelKind != "groq" || g.APIKey != "gsk_secret-value" || g.Model != "openai/gpt-oss-20b" ||
		g.ReasoningEffort != "low" || g.BaseURL != "https://api.groq.com" || g.MaxCompletionTokens != 256 ||
		g.MaxRetries != 2 || g.Timeout != 10*time.Second {
		t.Errorf("groq config = %+v", g)
	}
	// Defaults sit under the free tier's 8,000 tokens a minute, leaving room for other users of the key.
	if cfg.groqPerMinute != 7200 || cfg.groqPerDay != 100_000 {
		t.Errorf("limits = %d/min, %d/day; want 7200 and 100000", cfg.groqPerMinute, cfg.groqPerDay)
	}
	if got := cfg.LogValue().String(); strings.Contains(got, "gsk_secret-value") || !strings.Contains(got, "openai/gpt-oss-20b") {
		t.Errorf("logged config = %s, want the model shown and the key hidden", got)
	}
}

func TestLoadConfigGroqOtherModels(t *testing.T) {
	setValidEnv(t)
	t.Setenv("MODEL", "groq")
	t.Setenv("GROQ_API_KEY", "k")
	t.Setenv("GROQ_MODEL", "qwen/qwen3.8-27b")
	t.Setenv("GROQ_TOKENS_PER_MINUTE", "4000")
	t.Setenv("GROQ_TOKENS_PER_DAY", "50000")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// reasoning_effort "low" is a gpt-oss setting; other models keep their default.
	if cfg.groq.ReasoningEffort != "" || cfg.groqPerMinute != 4000 || cfg.groqPerDay != 50000 {
		t.Errorf("config = %+v, %d/min, %d/day", cfg.groq, cfg.groqPerMinute, cfg.groqPerDay)
	}
}

func TestLoadConfigRejectsInvalidModelSettings(t *testing.T) {
	for _, tt := range []struct{ name, model, key, perMinute string }{
		{"unknown model kind", "llama", "k", ""},
		{"groq without a key", "groq", "", ""},
		{"groq with a zero rate", "groq", "k", "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("MODEL", tt.model)
			t.Setenv("GROQ_API_KEY", tt.key)
			t.Setenv("GROQ_TOKENS_PER_MINUTE", tt.perMinute)
			if cfg, err := loadConfig(); err == nil {
				t.Errorf("loadConfig succeeded: %+v", cfg)
			}
		})
	}
}

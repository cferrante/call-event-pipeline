package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cferrante/decoupled-ingestion-layer/internal/inference"
)

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	for _, k := range []string{"NATS_URL", "INFERENCE_WORKERS", "MODEL_CAPACITY", "MODEL_LATENCY", "MODEL_TIMEOUT"} {
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

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.workers != 8 || cfg.model.Capacity != 6 || cfg.model.BaseLatency != 50*time.Millisecond ||
		cfg.model.Timeout != time.Second {
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

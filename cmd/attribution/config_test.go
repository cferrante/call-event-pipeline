package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	t.Setenv("NATS_URL", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.databaseURL != "postgres://leads_app:s3cret@127.0.0.1:5432/leads" || cfg.natsURL != "nats://127.0.0.1:4222" {
		t.Errorf("config = %+v", cfg)
	}
}

// Without DATABASE_URL, pgx would fall back to the developer's own (possibly superuser) login.
func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig succeeded without DATABASE_URL")
	}
}

func TestConfigLogValueHidesDatabasePassword(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	got := cfg.LogValue().String()
	if strings.Contains(got, "s3cret") {
		t.Errorf("logged config contains the password: %s", got)
	}
	if !strings.Contains(got, "leads_app") {
		t.Errorf("logged config = %s, want the redacted URL kept for debugging", got)
	}
}

// -replay-from takes an RFC 3339 time, so there is no doubt about its time zone.
func TestParseReplayFrom(t *testing.T) {
	got, err := parseReplayFrom("2026-11-13T09:30:00Z")
	if err != nil || !got.Equal(time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "yesterday", "2026-11-13 09:30", "09:30"} {
		if _, err := parseReplayFrom(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Profiling is opt-in (PPROF_ADDR), for the server benchmarks' CPU profiles.
func TestLoadConfigPprofAddr(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	t.Setenv("PPROF_ADDR", "127.0.0.1:6061")
	cfg, err := loadConfig()
	if err != nil || cfg.pprofAddr != "127.0.0.1:6061" {
		t.Errorf("pprofAddr = %q, err %v; want 127.0.0.1:6061", cfg.pprofAddr, err)
	}
	t.Setenv("PPROF_ADDR", "")
	if cfg, _ := loadConfig(); cfg.pprofAddr != "" {
		t.Errorf("pprofAddr = %q without PPROF_ADDR; want off", cfg.pprofAddr)
	}
}

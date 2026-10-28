package main

import (
	"strings"
	"testing"
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

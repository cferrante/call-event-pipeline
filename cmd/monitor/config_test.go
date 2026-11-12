package main

import (
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"MONITOR_ADDR", "MONITOR_INTERVAL", "CANARY_INTERVAL", "CANARY_MAX_AGE",
		"INGEST_URL", "INGEST_PORT", "INGEST_PUBLIC_URL", "TWILIO_AUTH_TOKEN", "DATABASE_URL", "NATS_URL"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

var baseEnv = map[string]string{
	"DATABASE_URL": "postgres://x@127.0.0.1/leads", "TWILIO_AUTH_TOKEN": "tok",
	"INGEST_PUBLIC_URL": "http://localhost:8081", "INGEST_PORT": "8081",
}

func TestConfigDefaults(t *testing.T) {
	setEnv(t, baseEnv)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != "127.0.0.1:8090" || cfg.interval != 5*time.Second || cfg.canaryInterval != 10*time.Second ||
		cfg.rules.CanaryMaxAge != 30*time.Second || cfg.ingestURL != "http://127.0.0.1:8081" {
		t.Errorf("config = %+v", cfg)
	}
}

// The status page has no login, so it must never listen beyond this machine; on a server it is
// reached through an SSH or Session Manager port forward.
func TestStatusPageListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{":8090", "0.0.0.0:8090", "10.0.0.5:8090", "example.com:8090"} {
		env := map[string]string{"MONITOR_ADDR": addr}
		for k, v := range baseEnv {
			env[k] = v
		}
		setEnv(t, env)
		if _, err := loadConfig(); err == nil {
			t.Errorf("MONITOR_ADDR=%s accepted, want only loopback", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:9000", "[::1]:9000", "localhost:9000"} {
		env := map[string]string{"MONITOR_ADDR": addr}
		for k, v := range baseEnv {
			env[k] = v
		}
		setEnv(t, env)
		if _, err := loadConfig(); err != nil {
			t.Errorf("MONITOR_ADDR=%s rejected: %v", addr, err)
		}
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{"MONITOR_INTERVAL": "soon", "CANARY_INTERVAL": "-1s", "CANARY_MAX_AGE": "0s"} {
		env := map[string]string{k: v}
		for bk, bv := range baseEnv {
			env[bk] = bv
		}
		setEnv(t, env)
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	setEnv(t, map[string]string{"TWILIO_AUTH_TOKEN": "tok"})
	if _, err := loadConfig(); err == nil {
		t.Error("missing DATABASE_URL accepted")
	}
}

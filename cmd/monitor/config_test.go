package main

import (
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"MONITOR_ADDR", "MONITOR_INTERVAL", "CANARY_INTERVAL", "CANARY_MAX_AGE",
		"INGEST_URL", "INGEST_PORT", "INGEST_PUBLIC_URL", "TWILIO_AUTH_TOKEN", "DATABASE_URL", "NATS_URL",
		"ALERT_EMAIL_TO", "ALERT_EMAIL_FROM", "SENDGRID_BASE_URL", "SENDGRID_API_KEY"} {
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

// Alert emails are off unless a recipient is set; once one is, every setting the send needs
// must be there too, and there is no default base URL, so a development machine can't email
// through real SendGrid by accident.
func TestAlertEmailConfig(t *testing.T) {
	setEnv(t, baseEnv)
	if cfg, err := loadConfig(); err != nil || cfg.email != nil {
		t.Fatalf("without ALERT_EMAIL_TO: email %+v, err %v; want off", cfg.email, err)
	}
	full := map[string]string{"ALERT_EMAIL_TO": "oncall@example.com", "ALERT_EMAIL_FROM": "monitor@example.com",
		"SENDGRID_BASE_URL": "http://127.0.0.1:8025", "SENDGRID_API_KEY": "SG.k"}
	env := map[string]string{}
	for k, v := range baseEnv {
		env[k] = v
	}
	for k, v := range full {
		env[k] = v
	}
	setEnv(t, env)
	cfg, err := loadConfig()
	if err != nil || cfg.email == nil || cfg.email.To != "oncall@example.com" || cfg.email.From != "monitor@example.com" ||
		cfg.email.BaseURL != "http://127.0.0.1:8025" || cfg.email.APIKey != "SG.k" {
		t.Fatalf("email %+v, err %v", cfg.email, err)
	}
	for _, missing := range []string{"ALERT_EMAIL_FROM", "SENDGRID_BASE_URL", "SENDGRID_API_KEY"} {
		e := map[string]string{}
		for k, v := range env {
			e[k] = v
		}
		delete(e, missing)
		setEnv(t, e)
		if _, err := loadConfig(); err == nil {
			t.Errorf("ALERT_EMAIL_TO set without %s accepted", missing)
		}
	}
	env["ALERT_EMAIL_TO"] = "not-an-address"
	setEnv(t, env)
	if _, err := loadConfig(); err == nil {
		t.Error("an invalid ALERT_EMAIL_TO accepted")
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

func TestListenAddrDefaultsTo8080(t *testing.T) {
	t.Setenv("INGEST_PORT", "")

	addr, err := listenAddr()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != ":8080" {
		t.Errorf("addr = %q, want %q", addr, ":8080")
	}
}

func TestListenAddrUsesIngestPort(t *testing.T) {
	t.Setenv("INGEST_PORT", "9091")

	addr, err := listenAddr()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != ":9091" {
		t.Errorf("addr = %q, want %q", addr, ":9091")
	}
}

func TestListenAddrRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "65536", "-1"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("INGEST_PORT", port)

			if addr, err := listenAddr(); err == nil {
				t.Errorf("want error, got addr %q", addr)
			}
		})
	}
}

// setValidEnv sets every required variable; tests then change one at a time.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("INGEST_PORT", "")
	t.Setenv("TWILIO_AUTH_TOKEN", "secret-token")
	t.Setenv("INGEST_PUBLIC_URL", "https://hooks.example.com")
	t.Setenv("NATS_URL", "")
	t.Setenv("INGEST_FORWARD_TO", "")
	t.Setenv("SENDGRID_PUBLIC_KEY", "")
	t.Setenv("PPROF_ADDR", "")
}

// The email route turns on only with a valid SendGrid key; a bad one stops ingest at startup.
func TestLoadConfigSendGridKey(t *testing.T) {
	setValidEnv(t)
	signer, err := sendgrid.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SENDGRID_PUBLIC_KEY", signer.PublicKeyBase64())
	cfg, err := loadConfig()
	if err != nil || cfg.sendgridKey == nil {
		t.Fatalf("valid key: key %v, err %v", cfg.sendgridKey, err)
	}
	t.Setenv("SENDGRID_PUBLIC_KEY", "not-a-key")
	if _, err := loadConfig(); err == nil {
		t.Error("an invalid SENDGRID_PUBLIC_KEY was accepted")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := config{
		addr:      ":8080",
		authToken: "secret-token",
		publicURL: "https://hooks.example.com",
		natsURL:   "nats://127.0.0.1:4222",
	}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigReadsOptionalValues(t *testing.T) {
	setValidEnv(t)
	t.Setenv("NATS_URL", "nats://10.0.0.5:4222")
	t.Setenv("INGEST_FORWARD_TO", "+14158675310")
	t.Setenv("INGEST_PUBLIC_URL", "https://hooks.example.com:8443/")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.natsURL != "nats://10.0.0.5:4222" || cfg.forwardTo != "+14158675310" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.publicURL != "https://hooks.example.com:8443" {
		t.Errorf("publicURL = %q, want the trailing slash removed", cfg.publicURL)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct{ name, key, value string }{
		{"missing auth token", "TWILIO_AUTH_TOKEN", ""},
		{"missing public URL", "INGEST_PUBLIC_URL", ""},
		{"public URL without scheme", "INGEST_PUBLIC_URL", "hooks.example.com"},
		{"public URL with a path", "INGEST_PUBLIC_URL", "https://hooks.example.com/ingest"},
		{"public URL with a query", "INGEST_PUBLIC_URL", "https://hooks.example.com?x=1"},
		{"forward-to not E.164", "INGEST_FORWARD_TO", "415-867-5310"},
		{"invalid port", "INGEST_PORT", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tt.key, tt.value)

			if cfg, err := loadConfig(); err == nil {
				t.Errorf("loadConfig succeeded with %s=%q: %+v", tt.key, tt.value, cfg)
			}
		})
	}
}

// The auth token is a credential; it must never appear when the config is logged.
func TestConfigLogValueHidesAuthToken(t *testing.T) {
	setValidEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if got := cfg.LogValue().String(); strings.Contains(got, "secret-token") {
		t.Errorf("logged config contains the auth token: %s", got)
	}
}

// Profiling is opt-in (PPROF_ADDR), for the server benchmarks' CPU profiles.
func TestLoadConfigPprofAddr(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PPROF_ADDR", "127.0.0.1:6060")
	cfg, err := loadConfig()
	if err != nil || cfg.pprofAddr != "127.0.0.1:6060" {
		t.Errorf("pprofAddr = %q, err %v; want 127.0.0.1:6060", cfg.pprofAddr, err)
	}
	t.Setenv("PPROF_ADDR", "")
	if cfg, _ := loadConfig(); cfg.pprofAddr != "" {
		t.Errorf("pprofAddr = %q without PPROF_ADDR; want off", cfg.pprofAddr)
	}
}

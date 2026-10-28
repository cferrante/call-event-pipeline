package env

import (
	"strings"
	"testing"
)

func TestNATSURL(t *testing.T) {
	t.Setenv("NATS_URL", "")
	if got := NATSURL(); got != "nats://127.0.0.1:4222" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("NATS_URL", "nats://10.0.0.5:4222")
	if got := NATSURL(); got != "nats://10.0.0.5:4222" {
		t.Errorf("set = %q", got)
	}
}

func TestDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	if got, err := DatabaseURL(); err != nil || got != "postgres://leads_app:s3cret@127.0.0.1:5432/leads" {
		t.Errorf("DatabaseURL = %q, %v", got, err)
	}
}

// Without DATABASE_URL, pgx would fall back to the developer's own (possibly superuser) login.
func TestDatabaseURLIsRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	_, err := DatabaseURL()
	if err == nil || !strings.Contains(err.Error(), "source .env") {
		t.Errorf("err = %v, want an error explaining how to load .env", err)
	}
}

func TestRedact(t *testing.T) {
	got := Redact("postgres://leads_app:s3cret@127.0.0.1:5432/leads")
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "leads_app") {
		t.Errorf("Redact = %q, want the password hidden and the rest kept", got)
	}
	if got := Redact("://not a url"); strings.Contains(got, "not a url") {
		t.Errorf("Redact(unparseable) = %q, want nothing of the input", got)
	}
}

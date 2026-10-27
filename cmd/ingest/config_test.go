package main

import "testing"

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

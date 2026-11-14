package profiling

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Off unless set: a service started without PPROF_ADDR must not listen anywhere.
func TestStartIsOffWithoutAnAddress(t *testing.T) {
	addr, stop, err := Start("")
	if err != nil || addr != "" {
		t.Fatalf("Start(\"\") = %q, %v; want no listener and no error", addr, err)
	}
	stop()
}

// Profiles reveal internals (and goroutine dumps can hold request data), so only a loopback
// address is accepted. On the demo server they're read through Session Manager.
func TestStartRefusesNonLoopbackAddresses(t *testing.T) {
	for _, addr := range []string{":6060", "0.0.0.0:6060", "10.0.0.5:6060", "[::]:6060", "example.com:6060", "6060"} {
		if _, stop, err := Start(addr); err == nil {
			stop()
			t.Errorf("Start(%q) accepted a non-loopback address", addr)
		}
	}
}

func TestStartServesProfilesOnLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			got, stop, err := Start(addr)
			if err != nil {
				if strings.Contains(addr, "::1") && strings.Contains(err.Error(), "cannot assign") {
					t.Skip("no IPv6 loopback here")
				}
				t.Fatalf("Start(%q): %v", addr, err)
			}
			defer stop()
			for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile?seconds=1", "/debug/pprof/goroutine"} {
				body := get(t, "http://"+got+path)
				if len(body) == 0 {
					t.Errorf("%s returned an empty body", path)
				}
			}
		})
	}
}

// stop closes the listener, so a stopped service's port is free again.
func TestStopClosesTheListener(t *testing.T) {
	addr, stop, err := Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/debug/pprof/", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("still serving after stop")
	}
}

func get(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	return body
}

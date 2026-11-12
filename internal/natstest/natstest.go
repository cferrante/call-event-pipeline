// Package natstest starts a throwaway nats-server with JetStream for integration tests.
package natstest

import (
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/cferrante/call-event-pipeline/internal/testenv"
)

// Start runs the nats-server binary on a free local port with JetStream storage in a
// temporary directory, and returns a connection to it. The server is stopped and its
// data deleted when the test ends. If nats-server isn't installed the test is skipped, or
// fails under REQUIRE_INTEGRATION=1 (see package testenv).
func Start(t *testing.T) *nats.Conn {
	t.Helper()
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		testenv.Missing(t, "nats-server not found on PATH; see docs/plan-m1.md step 0")
		return nil
	}

	port := freePort(t)
	cmd := exec.Command(bin, "-js", "-sd", t.TempDir(), "-a", "127.0.0.1", "-p", strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	url := "nats://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		nc, err := nats.Connect(url)
		if err == nil {
			t.Cleanup(nc.Close)
			return nc
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server did not accept connections: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

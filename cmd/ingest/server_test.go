package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":8080", http.NewServeMux())

	// Slow or stalled clients can't tie up connections during a spike.
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 {
		t.Errorf("read header/read/write timeouts = %v/%v/%v, want all set",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout)
	}
	// Idle keep-alive connections must outlive the client's idle timeout (90s for Go clients,
	// including k6), so the client closes them first. Otherwise a client can send a request on
	// a connection the server is closing, and it fails with no response. Left unset, Go uses
	// ReadTimeout (10s).
	if srv.IdleTimeout < 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want at least 2m", srv.IdleTimeout)
	}
}

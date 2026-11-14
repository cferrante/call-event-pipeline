// Package profiling serves Go's pprof profiles, for the benchmarks' CPU profiles
// (docs/plan-m3.md, step 16). It is off unless a service is given an address, and only a
// loopback address is accepted: profiles and goroutine dumps reveal internals, so on the demo
// server they are read through Session Manager, never over the network.
package profiling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Start serves pprof on addr (host:port, port 0 picks one) and returns the address it listens
// on and a function that stops it. With an empty addr it does nothing: profiling is opt-in.
func Start(addr string) (string, func(), error) {
	if addr == "" {
		return "", func() {}, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", nil, fmt.Errorf("PPROF_ADDR must be host:port, got %q", addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", nil, fmt.Errorf("PPROF_ADDR must be a loopback address such as 127.0.0.1:6060, got %q", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("PPROF_ADDR: %w", err)
	}
	// Its own mux, never http.DefaultServeMux, so the profiles can't leak onto another server.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// No write timeout: a CPU profile streams for as many seconds as it was asked for.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("pprof server stopped", "err", err)
		}
	}()
	slog.Info("pprof listening", "addr", ln.Addr().String())
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// Command ingest is the webhook edge: it verifies, stores and acknowledges lead events.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	addr, err := listenAddr()
	if err != nil {
		slog.Error("invalid config", "err", err)
		os.Exit(1)
	}
	slog.Info("ingest listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// healthz reports that the process is up and serving HTTP.
func healthz(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

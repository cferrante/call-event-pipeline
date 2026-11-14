// Command ingest is the webhook edge: it verifies, stores and acknowledges lead events.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/profiling"
	"github.com/cferrante/call-event-pipeline/internal/stream"
)

// shutdownTimeout is how long in-flight webhooks get to finish after Ctrl-C.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("ingest stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Opt-in CPU profiles for the benchmarks; a bad address stops the service at startup.
	_, stopPprof, err := profiling.Start(cfg.pprofAddr)
	if err != nil {
		return err
	}
	defer stopPprof()

	nc, err := nats.Connect(cfg.natsURL, nats.Name("ingest"))
	if err != nil {
		return fmt.Errorf("connect to NATS at %s: %w", cfg.natsURL, err)
	}
	defer nc.Drain()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	if err := stream.EnsureStream(ctx, js); err != nil {
		return err
	}

	srv := newHTTPServer(cfg.addr, routes(cfg, stream.NewPublisher(js)))
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("ingest listening", "config", cfg)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down; finishing in-flight webhooks")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// routes maps the edge's endpoints. The email route exists only when SendGrid's public key is
// configured, so the edge never accepts email it can't verify.
func routes(cfg config, pub publisher) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("POST /webhooks/voice", newWebhook(cfg, envelope.Voice, pub, voiceResponse(cfg.forwardTo)))
	mux.Handle("POST /webhooks/sms", newWebhook(cfg, envelope.SMS, pub, smsResponse))
	if cfg.sendgridKey != nil {
		mux.Handle("POST /webhooks/sendgrid/inbound",
			&emailWebhook{key: cfg.sendgridKey, pub: pub, now: time.Now, maxBytes: maxEmailBytes, timeout: emailTimeout})
	}
	return mux
}

// newHTTPServer returns the server with its timeouts set.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: h,
		// Limits stop slow or stalled clients from tying up connections during a spike.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		// Keep idle connections longer than clients do (90s for Go clients), so the client
		// closes them first. If the server closed first, a client could send a request on a
		// closing connection and get no response at all. Unset, Go would use ReadTimeout (10s).
		IdleTimeout: 120 * time.Second,
	}
}

func newWebhook(cfg config, ch envelope.Channel, pub publisher, response string) *webhook {
	return &webhook{
		channel:   ch,
		authToken: cfg.authToken,
		publicURL: cfg.publicURL,
		pub:       pub,
		now:       time.Now,
		response:  response,
	}
}

// healthz reports that the process is up and serving HTTP.
func healthz(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

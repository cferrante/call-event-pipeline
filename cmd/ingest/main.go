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

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
	"github.com/cferrante/decoupled-ingestion-layer/internal/stream"
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

	pub := stream.NewPublisher(js)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("POST /webhooks/voice", newWebhook(cfg, envelope.Voice, pub, voiceResponse(cfg.forwardTo)))
	mux.Handle("POST /webhooks/sms", newWebhook(cfg, envelope.SMS, pub, smsResponse))

	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: mux,
		// Limits stop slow or stalled clients from tying up connections during a spike.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
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

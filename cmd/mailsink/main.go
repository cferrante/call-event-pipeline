// Command mailsink stands in for SendGrid's Mail Send API (docs/design.md, section 8.2), so
// alert emails can be shown end to end without an account. It accepts POST /v3/mail/send with
// the bearer key from SENDGRID_API_KEY, checks the request against SendGrid's documented
// shape, answers 202, and keeps the messages instead of delivering them. GET /messages lists
// them, newest first. It listens on a loopback address only.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mailsink stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := os.Getenv("MAILSINK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8025"
	}
	if err := loopbackOnly(addr); err != nil {
		return err
	}
	key := os.Getenv("SENDGRID_API_KEY")
	if key == "" {
		return errors.New("SENDGRID_API_KEY is not set (scripts/env-setup.sh creates one)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: addr, Handler: newSink(key).handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("mailsink running", "mail_send", "http://"+addr+"/v3/mail/send", "messages", "http://"+addr+"/messages")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// loopbackOnly refuses any address beyond this machine: the sink has no TLS, and its message
// list has no login.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("MAILSINK_ADDR must be host:port, got %q", addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("MAILSINK_ADDR must be a loopback address such as 127.0.0.1:8025, got %q", addr)
	}
	return nil
}

//go:build faultinject

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/cferrante/call-event-pipeline/internal/consumer"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// silentWrites is the drill fault (docs/design.md, section 8.3): while on, every event is
// reported as stored, so the consumer acknowledges it, but nothing reaches Postgres. The
// process stays up, its health is fine and its lag stays at zero; only the monitor's
// acked-but-not-written rule can see it. It exists only in builds with the faultinject tag.
type silentWrites struct {
	next consumer.Recorder
	on   atomic.Bool
}

func (s *silentWrites) Record(ctx context.Context, env envelope.Envelope) (bool, error) {
	if s.on.Load() {
		return true, nil
	}
	return s.next.Record(ctx, env)
}

// injectFault wraps the store so that SIGUSR1 toggles the fault.
func injectFault(ctx context.Context, next consumer.Recorder) consumer.Recorder {
	s := &silentWrites{next: next}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if on := !s.on.Load(); on {
					s.on.Store(true)
					slog.Warn("FAULT INJECTION on: acknowledging events without writing them (SIGUSR1 again to stop)")
				} else {
					s.on.Store(false)
					slog.Warn("FAULT INJECTION off: writing events again")
				}
			}
		}
	}()
	slog.Warn("built with faultinject: SIGUSR1 toggles acknowledging without writing")
	return s
}

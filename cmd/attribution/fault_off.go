//go:build !faultinject

package main

import (
	"context"

	"github.com/cferrante/call-event-pipeline/internal/consumer"
)

// injectFault is a no-op in normal builds: the drill fault exists only with the faultinject
// build tag (see fault_on.go), so a production binary can't contain it.
func injectFault(_ context.Context, next consumer.Recorder) consumer.Recorder { return next }

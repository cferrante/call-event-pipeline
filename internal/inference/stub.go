// Package inference classifies lead interactions with an AI model. In milestone 2 the model is
// a stub that behaves like a real inference server under load; see docs/design.md, section 5.4.
package inference

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// ErrOverloaded means the model had too many calls in flight to answer within its timeout.
// Like a real overloaded server, it's temporary: the caller should retry later.
var ErrOverloaded = errors.New("model overloaded: timed out")

// Input is what the model sees of one interaction.
type Input struct {
	Channel envelope.Channel
	Body    string // SMS text; empty for calls
}

// Result is the model's classification.
type Result struct {
	Intent       string  // sales, support or unknown
	Score        float64 // confidence, 0 to 1
	ModelVersion string
}

// Model classifies interactions. The stub implements it; a real model could replace it.
type Model interface {
	Classify(ctx context.Context, in Input) (Result, error)
}

// StubConfig sets how the stub behaves under load.
type StubConfig struct {
	Capacity    int           // concurrent calls served at full speed
	BaseLatency time.Duration // one call's latency within capacity
	Timeout     time.Duration // calls that would take longer fail with ErrOverloaded
	Jitter      float64       // random variation, e.g. 0.1 for ±10%
}

// DefaultStubConfig is calibrated from a real model (gpt-oss-20b on Groq, measured at a low
// rate; docs/benchmarks.md): p50 309 ms and p95 519 ms, which a ±75% spread around 309 ms
// reproduces. The fastest calls come out quicker than measured (77 ms against 107 ms) and the
// slowest slower than the measured p99's long tail allows for; the middle matches. Capacity
// stays 4: a hosted model's real limit is tokens a minute, which the Groq adapter's limiter
// enforces instead.
var DefaultStubConfig = StubConfig{Capacity: 4, BaseLatency: 309 * time.Millisecond, Timeout: 2 * time.Second, Jitter: 0.75}

// Stub is a fake model with a fixed capacity. A call that starts while k calls are in flight
// (itself included) takes BaseLatency × max(1, k/Capacity): beyond capacity, calls share the
// model and each gets slower, as on a real inference server. The latency is fixed when the call
// starts; calls that start or finish later don't change it.
type Stub struct {
	cfg      StubConfig
	inflight atomic.Int64
	sleep    func(context.Context, time.Duration) error // replaced in tests
	random   func() float64                             // in [0, 1); replaced in tests
}

func NewStub(cfg StubConfig) *Stub {
	return &Stub{cfg: cfg, sleep: sleepContext, random: rand.Float64}
}

const stubVersion = "stub-v1"

func (s *Stub) Classify(ctx context.Context, in Input) (Result, error) {
	k := s.inflight.Add(1)
	defer s.inflight.Add(-1)

	latency := s.latency(int(k))
	if latency > s.cfg.Timeout {
		if err := s.sleep(ctx, s.cfg.Timeout); err != nil {
			return Result{}, err
		}
		return Result{}, ErrOverloaded
	}
	if err := s.sleep(ctx, latency); err != nil {
		return Result{}, err
	}
	return classify(in), nil
}

// latency is the time a call takes when it starts as the kth call in flight.
func (s *Stub) latency(k int) time.Duration {
	load := max(1, float64(k)/float64(s.cfg.Capacity))
	jitter := 1 + s.cfg.Jitter*(2*s.random()-1)
	return time.Duration(float64(s.cfg.BaseLatency) * load * jitter)
}

var keywords = []struct {
	intent string
	words  []string
}{
	{"sales", []string{"price", "quote", "cost", "buy", "order now"}},
	{"support", []string{"help", "broken", "refund", "support", "cancel"}},
}

// classify applies fixed keyword rules, so results are predictable in tests.
func classify(in Input) Result {
	body := strings.ToLower(in.Body)
	for _, k := range keywords {
		for _, w := range k.words {
			if strings.Contains(body, w) {
				return Result{Intent: k.intent, Score: 0.9, ModelVersion: stubVersion}
			}
		}
	}
	return Result{Intent: "unknown", Score: 0.5, ModelVersion: stubVersion}
}

// sleepContext waits for d, or returns early with ctx's error if it's cancelled first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

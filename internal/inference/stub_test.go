package inference

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// fakeClock records each delay the stub asks for. While held, sleeps block until released,
// so tests can line up several calls in flight at once.
type fakeClock struct {
	mu      sync.Mutex
	slept   []time.Duration
	release chan struct{} // nil: sleeps return at once
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *fakeClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(slices.Values(c.slept))
}

func testConfig() StubConfig {
	return StubConfig{Capacity: 4, BaseLatency: 200 * time.Millisecond, Timeout: 2 * time.Second, Jitter: 0.1}
}

// newTestStub uses the fake clock and a fixed random value of 0.5, which means no jitter.
func newTestStub(cfg StubConfig, clock *fakeClock) *Stub {
	s := NewStub(cfg)
	s.sleep = clock.sleep
	s.random = func() float64 { return 0.5 }
	return s
}

func sms(body string) Input { return Input{Channel: envelope.SMS, Body: body} }

func TestClassifyByKeywords(t *testing.T) {
	tests := []struct {
		in     Input
		intent string
		score  float64
	}{
		{sms("How much would a quote cost?"), "sales", 0.9},
		{sms("I want to BUY two"), "sales", 0.9},
		{sms("My order arrived broken, please help"), "support", 0.9},
		{sms("hello there"), "unknown", 0.5},
		{Input{Channel: envelope.Voice}, "unknown", 0.5},
	}
	for _, tt := range tests {
		got, err := newTestStub(testConfig(), &fakeClock{}).Classify(context.Background(), tt.in)
		if err != nil {
			t.Fatalf("Classify(%+v): %v", tt.in, err)
		}
		if got.Intent != tt.intent || got.Score != tt.score || got.ModelVersion != "stub-v1" {
			t.Errorf("Classify(%q) = %+v, want %s %.1f stub-v1", tt.in.Body, got, tt.intent, tt.score)
		}
	}
}

func TestLatencyWithinCapacityIsBase(t *testing.T) {
	clock := &fakeClock{}

	if _, err := newTestStub(testConfig(), clock).Classify(context.Background(), sms("hi")); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := clock.delays(); !slices.Equal(got, []time.Duration{200 * time.Millisecond}) {
		t.Errorf("delays = %v, want [200ms]", got)
	}
}

// classifyConcurrently starts n calls, holds them all in flight, then lets them finish.
func classifyConcurrently(t *testing.T, s *Stub, clock *fakeClock, n int) []error {
	t.Helper()
	clock.release = make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = s.Classify(context.Background(), sms("hi")) })
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(clock.delays()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d calls started", len(clock.delays()), n)
		}
		time.Sleep(time.Millisecond)
	}
	close(clock.release)
	wg.Wait()
	return errs
}

// Beyond capacity, calls share the model: the kth concurrent call takes base × k / capacity.
func TestLatencyGrowsBeyondCapacity(t *testing.T) {
	clock := &fakeClock{}
	s := newTestStub(testConfig(), clock)

	for i, err := range classifyConcurrently(t, s, clock, 8) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}

	ms := time.Millisecond
	want := []time.Duration{200 * ms, 200 * ms, 200 * ms, 200 * ms, 250 * ms, 300 * ms, 350 * ms, 400 * ms}
	if got := clock.delays(); !slices.Equal(got, want) {
		t.Errorf("delays = %v, want %v", got, want)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("in flight after all calls = %d, want 0", n)
	}
}

// A call whose latency would exceed the timeout waits the timeout, then fails.
func TestOverloadedCallTimesOut(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 300 * time.Millisecond // the 7th concurrent call would take 350ms
	clock := &fakeClock{}
	s := newTestStub(cfg, clock)

	errs := classifyConcurrently(t, s, clock, 7)

	overloaded := 0
	for _, err := range errs {
		if errors.Is(err, ErrOverloaded) {
			overloaded++
		} else if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if overloaded != 1 {
		t.Errorf("%d calls overloaded, want 1", overloaded)
	}
	if got := clock.delays(); len(got) == 0 || got[len(got)-1] != 300*time.Millisecond {
		t.Errorf("delays = %v, want the longest to be the 300ms timeout", got)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("in flight after all calls = %d, want 0", n)
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	for _, tt := range []struct {
		random float64
		want   time.Duration
	}{
		{0, 180 * time.Millisecond},   // −10%
		{0.5, 200 * time.Millisecond}, // none
		{1, 220 * time.Millisecond},   // +10%
	} {
		clock := &fakeClock{}
		s := newTestStub(testConfig(), clock)
		s.random = func() float64 { return tt.random }

		if _, err := s.Classify(context.Background(), sms("hi")); err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if got := clock.delays(); len(got) != 1 || got[0] != tt.want {
			t.Errorf("random %.1f: delays = %v, want [%v]", tt.random, got, tt.want)
		}
	}
}

func TestCancelledCallReturnsContextError(t *testing.T) {
	clock := &fakeClock{release: make(chan struct{})} // never released
	s := newTestStub(testConfig(), clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Classify(ctx, sms("hi")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("in flight = %d, want 0", n)
	}
}

// The default clock really sleeps, and stops early when the context is cancelled.
func TestRealSleep(t *testing.T) {
	cfg := testConfig()
	cfg.BaseLatency, cfg.Jitter = 30*time.Millisecond, 0
	s := NewStub(cfg)

	start := time.Now()
	if _, err := s.Classify(context.Background(), sms("hi")); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned after %v, want at least 30ms", elapsed)
	}

	cfg.BaseLatency = time.Hour
	cfg.Timeout = 2 * time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewStub(cfg).Classify(ctx, sms("hi"))
		done <- err
	}()
	// Fail fast rather than hang for an hour if cancellation is ignored.
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled call still running after 1s: the context was ignored")
	}
}

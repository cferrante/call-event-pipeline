package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptedAlerter answers each attempt from a script of errors (nil = delivered).
type scriptedAlerter struct {
	mu       sync.Mutex
	script   []error
	attempts []time.Time
	got      []Event
}

func (a *scriptedAlerter) Alert(_ context.Context, e Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = append(a.attempts, time.Now())
	var err error
	if len(a.script) > 0 {
		err, a.script = a.script[0], a.script[1:]
	}
	if err == nil {
		a.got = append(a.got, e)
	}
	return err
}

func (a *scriptedAlerter) snapshot() ([]time.Time, []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.attempts...), append([]Event(nil), a.got...)
}

type retryErr struct {
	retryable bool
	at        time.Time
}

func (e retryErr) Error() string      { return "scripted failure" }
func (e retryErr) Retryable() bool    { return e.retryable }
func (e retryErr) RetryAt() time.Time { return e.at }

var fastQueue = QueueOptions{Size: 10, MinBackoff: 10 * time.Millisecond, MaxBackoff: 80 * time.Millisecond,
	MaxAttempts: 4, AttemptTimeout: time.Second}

func runQueue(t *testing.T, a Alerter, opts QueueOptions) *Queue {
	t.Helper()
	q := NewQueue(a, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return q
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var ev = Event{State: Firing, Rule: RuleArrivalsStopped, Detail: "no canary", At: simStart}

// A rate limit (429) or server error is retried with growing waits until it gets through.
func TestQueueRetriesWithBackoff(t *testing.T) {
	a := &scriptedAlerter{script: []error{retryErr{retryable: true}, retryErr{retryable: true}, nil}}
	q := runQueue(t, a, fastQueue)
	q.Enqueue(ev)
	waitFor(t, "delivery", func() bool { _, got := a.snapshot(); return len(got) == 1 })

	at, _ := a.snapshot()
	if len(at) != 3 {
		t.Fatalf("%d attempts, want 3", len(at))
	}
	if gap1, gap2 := at[1].Sub(at[0]), at[2].Sub(at[1]); gap1 < 10*time.Millisecond || gap2 < 20*time.Millisecond {
		t.Errorf("waits %v then %v; want at least 10ms then 20ms (doubling)", gap1, gap2)
	}
	if out := q.Outbox(); len(out) != 1 || out[0].State != DeliverySent || out[0].Attempts != 3 {
		t.Errorf("outbox %+v, want one sent after 3 attempts", out)
	}
}

// A rate limit that says when it resets is retried no earlier than that.
func TestQueueWaitsForRateLimitReset(t *testing.T) {
	a := &scriptedAlerter{script: []error{retryErr{retryable: true, at: time.Now().Add(60 * time.Millisecond)}, nil}}
	q := runQueue(t, a, fastQueue)
	q.Enqueue(ev)
	waitFor(t, "delivery", func() bool { _, got := a.snapshot(); return len(got) == 1 })
	if at, _ := a.snapshot(); at[1].Sub(at[0]) < 50*time.Millisecond {
		t.Errorf("retried after %v, before the reset", at[1].Sub(at[0]))
	}
}

// A request the API rejects as wrong (a bad key, a malformed body) is not repeated, and
// neither is one that keeps failing past the attempt limit; both show as failed.
func TestQueueGivesUp(t *testing.T) {
	a := &scriptedAlerter{script: []error{retryErr{retryable: false}}}
	q := runQueue(t, a, fastQueue)
	q.Enqueue(ev)
	waitFor(t, "failure", func() bool { o := q.Outbox(); return len(o) == 1 && o[0].State == DeliveryFailed })
	if at, _ := a.snapshot(); len(at) != 1 {
		t.Errorf("%d attempts at a permanent error, want 1", len(at))
	}
	if o := q.Outbox(); o[0].LastError == "" {
		t.Errorf("outbox doesn't say why it failed: %+v", o[0])
	}

	always := &scriptedAlerter{script: []error{errors.New("x"), errors.New("x"), errors.New("x"), errors.New("x"), errors.New("x")}}
	q = runQueue(t, always, fastQueue)
	q.Enqueue(ev)
	waitFor(t, "failure", func() bool { o := q.Outbox(); return len(o) == 1 && o[0].State == DeliveryFailed })
	if at, _ := always.snapshot(); len(at) != fastQueue.MaxAttempts {
		t.Errorf("%d attempts, want the limit %d", len(at), fastQueue.MaxAttempts)
	}
}

// Alerts are delivered in the order they happened, so "resolved" never arrives before
// "firing".
func TestQueueKeepsOrder(t *testing.T) {
	a := &firstFiringFails{}
	q := runQueue(t, a, fastQueue)
	resolved := ev
	resolved.State = Resolved
	q.Enqueue(ev)
	q.Enqueue(resolved)
	waitFor(t, "both", func() bool { _, got := a.snapshot(); return len(got) == 2 })
	if _, got := a.snapshot(); got[0].State != Firing || got[1].State != Resolved {
		t.Errorf("delivered %v then %v", got[0].State, got[1].State)
	}
}

// firstFiringFails fails the first attempt at a firing event, whichever order attempts
// come in, so delivering in parallel would let "resolved" overtake it.
type firstFiringFails struct {
	scriptedAlerter
	failed bool
}

func (a *firstFiringFails) Alert(ctx context.Context, e Event) error {
	a.mu.Lock()
	fail := e.State == Firing && !a.failed
	a.failed = a.failed || fail
	a.mu.Unlock()
	if fail {
		return retryErr{retryable: true}
	}
	return a.scriptedAlerter.Alert(ctx, e)
}

// hungAlerter never answers until its context ends.
type hungAlerter struct{}

func (hungAlerter) Alert(ctx context.Context, _ Event) error { <-ctx.Done(); return ctx.Err() }

// A hung alerter never delays rule evaluation: ticks that raise alerts, and far more alerts
// than the queue holds, all return at once; the overflow is dropped and shown, not blocked on.
func TestHungAlerterNeverDelaysRules(t *testing.T) {
	opts := fastQueue
	opts.AttemptTimeout = time.Hour
	q := runQueue(t, hungAlerter{}, opts)

	cfg := DefaultConfig
	cfg.CanaryMaxAge = 10 * time.Second
	svc := NewService(&fakeSampler{next: Sample{CanarySeen: simStart}}, cfg, q.Enqueue)
	start := time.Now()
	for i := range 10 {
		svc.Tick(context.Background(), simStart.Add(time.Duration(i)*5*time.Second))
	}
	for range 3 * opts.Size {
		q.Enqueue(ev)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("ticks and enqueues took %v with a hung alerter", d)
	}
	if len(svc.Status().Active) != 1 {
		t.Errorf("the rule didn't fire")
	}
	dropped := 0
	for _, d := range q.Outbox() {
		if d.State == DeliveryDropped {
			dropped++
		}
	}
	if dropped == 0 {
		t.Errorf("overflow wasn't recorded as dropped")
	}
}

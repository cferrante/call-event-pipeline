//go:build integration

package consumer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/decoupled-ingestion-layer/internal/attribution"
	"github.com/cferrante/decoupled-ingestion-layer/internal/dbtest"
	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
	"github.com/cferrante/decoupled-ingestion-layer/internal/natstest"
	"github.com/cferrante/decoupled-ingestion-layer/internal/stream"
)

// fakeRecorder fails the first failures calls, optionally sleeping on the first call.
type fakeRecorder struct {
	mu         sync.Mutex
	calls      []string
	failures   int
	firstDelay time.Duration
	result     bool
}

func (f *fakeRecorder) Record(_ context.Context, env envelope.Envelope) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, env.ProviderID)
	n := len(f.calls)
	f.mu.Unlock()

	if n == 1 && f.firstDelay > 0 {
		time.Sleep(f.firstDelay)
	}
	if n <= f.failures {
		return false, errors.New("database unavailable")
	}
	return f.result, nil
}

func (f *fakeRecorder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type harness struct {
	ctx  context.Context
	js   jetstream.JetStream
	cons jetstream.Consumer
	// stopRun cancels Run as a shutdown signal would, and returns its error once it has
	// returned. Safe to call more than once.
	stopRun func() error
}

// start runs a NATS server with the LEADS stream, the DLQ and the attribution consumer, and
// processes messages with rec until the test ends. Retry delays are shortened for tests.
func start(t *testing.T, rec Recorder) harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	js, err := jetstream.New(natstest.Start(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := stream.EnsureStream(ctx, js); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	cons, err := EnsureConsumer(ctx, js)
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}

	h := &Handler{js: js, rec: rec, retryDelays: []time.Duration{50 * time.Millisecond}}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, cons, h) }()
	stopRun := sync.OnceValue(func() error {
		stop()
		return <-done
	})
	t.Cleanup(func() {
		if err := stopRun(); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return harness{ctx: ctx, js: js, cons: cons, stopRun: stopRun}
}

func (h harness) publish(t *testing.T, id string) {
	t.Helper()
	env, err := envelope.FromWebhook(envelope.Voice, url.Values{
		"CallSid": {id}, "From": {"+14158675310"}, "To": {"+18005551212"},
	}, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromWebhook: %v", err)
	}
	if err := stream.NewPublisher(h.js).Publish(h.ctx, env); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// publishRaw stores bytes that aren't a valid envelope, as a buggy producer might.
func (h harness) publishRaw(t *testing.T, data string) {
	t.Helper()
	if _, err := h.js.Publish(h.ctx, "leads.raw.voice", []byte(data)); err != nil {
		t.Fatalf("publish raw: %v", err)
	}
}

// waitSettled waits until the consumer has no unprocessed or unacknowledged messages.
func (h harness) waitSettled(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := h.cons.Info(h.ctx)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.NumPending == 0 && info.NumAckPending == 0 && info.Delivered.Stream > 0 {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer did not settle: pending=%d ack_pending=%d", info.NumPending, info.NumAckPending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h harness) dlqMessages(t *testing.T) []*jetstream.RawStreamMsg {
	t.Helper()
	s, err := h.js.Stream(h.ctx, DLQStream)
	if err != nil {
		t.Fatalf("stream %s: %v", DLQStream, err)
	}
	info, err := s.Info(h.ctx)
	if err != nil {
		t.Fatalf("dlq info: %v", err)
	}
	var msgs []*jetstream.RawStreamMsg
	for seq := info.State.FirstSeq; seq > 0 && seq <= info.State.LastSeq; seq++ {
		m, err := s.GetMsg(h.ctx, seq)
		if err != nil {
			t.Fatalf("dlq message %d: %v", seq, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestValidMessageIsRecordedAndAcked(t *testing.T) {
	rec := &fakeRecorder{result: true}
	h := start(t, rec)

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if rec.callCount() != 1 {
		t.Errorf("Record called %d times, want 1", rec.callCount())
	}
	if info.Delivered.Consumer != 1 {
		t.Errorf("deliveries = %d, want 1", info.Delivered.Consumer)
	}
	if n := len(h.dlqMessages(t)); n != 0 {
		t.Errorf("dead-lettered %d messages, want 0", n)
	}
}

// Record returns false for a redelivery it already processed; that is success, not an error.
func TestAlreadyRecordedMessageIsAcked(t *testing.T) {
	rec := &fakeRecorder{result: false}
	h := start(t, rec)

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if info.Delivered.Consumer != 1 || rec.callCount() != 1 {
		t.Errorf("calls = %d, deliveries = %d; want 1 of each", rec.callCount(), info.Delivered.Consumer)
	}
}

func TestUnparseableMessageIsDeadLetteredNotRetried(t *testing.T) {
	rec := &fakeRecorder{result: true}
	h := start(t, rec)

	h.publishRaw(t, `{"schema_version": 99}`)
	info := h.waitSettled(t)

	if rec.callCount() != 0 {
		t.Errorf("Record called %d times, want 0", rec.callCount())
	}
	if info.Delivered.Consumer != 1 {
		t.Errorf("deliveries = %d, want 1: a permanent failure must not be retried", info.Delivered.Consumer)
	}
	dlq := h.dlqMessages(t)
	if len(dlq) != 1 {
		t.Fatalf("dead-lettered %d messages, want 1", len(dlq))
	}
	m := dlq[0]
	if string(m.Data) != `{"schema_version": 99}` {
		t.Errorf("dead-letter data = %q, want the original bytes", m.Data)
	}
	if m.Header.Get(HeaderReason) == "" {
		t.Errorf("dead letter has no %s header", HeaderReason)
	}
	if got := m.Header.Get(HeaderSubject); got != "leads.raw.voice" {
		t.Errorf("%s = %q, want leads.raw.voice", HeaderSubject, got)
	}
}

func TestTransientFailureIsRetriedUntilItSucceeds(t *testing.T) {
	rec := &fakeRecorder{failures: 2, result: true}
	h := start(t, rec)

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if rec.callCount() != 3 {
		t.Errorf("Record called %d times, want 3 (two failures, then success)", rec.callCount())
	}
	if info.Delivered.Consumer != 3 {
		t.Errorf("deliveries = %d, want 3", info.Delivered.Consumer)
	}
	if n := len(h.dlqMessages(t)); n != 0 {
		t.Errorf("dead-lettered %d messages, want 0", n)
	}
}

// Processing slower than the first retry delay must not trigger a redelivery while the
// first attempt is still running (see docs/design.md, section 5.5).
func TestSlowProcessingIsNotRedelivered(t *testing.T) {
	rec := &fakeRecorder{firstDelay: 1500 * time.Millisecond, result: true}
	h := start(t, rec)

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if rec.callCount() != 1 || info.Delivered.Consumer != 1 {
		t.Errorf("calls = %d, deliveries = %d; want 1 of each", rec.callCount(), info.Delivered.Consumer)
	}
}

// End to end with the real store: a published webhook event becomes an interaction row.
func TestMessageIsStoredInPostgres(t *testing.T) {
	db := dbtest.Migrated(t)
	h := start(t, attribution.NewStore(db))

	h.publish(t, "CA1")
	h.publish(t, "CA2")
	h.waitSettled(t)

	var n int
	if err := db.QueryRow(h.ctx, "SELECT count(*) FROM interactions").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("interactions = %d, want 2", n)
	}
}

// A failure that never clears is dead-lettered on the final attempt, by the worker itself,
// so it reaches the DLQ even though nothing listens for JetStream's MaxDeliver advisory.
func TestFailureOnFinalAttemptIsDeadLettered(t *testing.T) {
	rec := &fakeRecorder{failures: 1000, result: true}
	h := start(t, rec)

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if rec.callCount() != maxDeliver {
		t.Errorf("Record called %d times, want %d", rec.callCount(), maxDeliver)
	}
	if info.Delivered.Consumer != maxDeliver {
		t.Errorf("deliveries = %d, want %d", info.Delivered.Consumer, maxDeliver)
	}
	dlq := h.dlqMessages(t)
	if len(dlq) != 1 {
		t.Fatalf("dead-lettered %d messages, want 1", len(dlq))
	}
	reason := dlq[0].Header.Get(HeaderReason)
	if !strings.Contains(reason, "5 attempts") || !strings.Contains(reason, "database unavailable") {
		t.Errorf("%s = %q, want the attempt count and the last error", HeaderReason, reason)
	}
}

// Succeeding on the last allowed attempt is a success, not a dead letter.
func TestSuccessOnFinalAttemptIsRecorded(t *testing.T) {
	rec := &fakeRecorder{failures: maxDeliver - 1, result: true}
	h := start(t, rec)

	h.publish(t, "CA1")
	h.waitSettled(t)

	if rec.callCount() != maxDeliver {
		t.Errorf("Record called %d times, want %d", rec.callCount(), maxDeliver)
	}
	if n := len(h.dlqMessages(t)); n != 0 {
		t.Errorf("dead-lettered %d messages, want 0", n)
	}
}

// ctxRecorder succeeds slowly, and counts calls made with an already-cancelled context, as a
// real database call would fail on one.
type ctxRecorder struct {
	delay     time.Duration
	mu        sync.Mutex
	calls     int
	cancelled int
}

func (r *ctxRecorder) Record(ctx context.Context, _ envelope.Envelope) (bool, error) {
	r.mu.Lock()
	r.calls++
	if ctx.Err() != nil {
		r.cancelled++
	}
	r.mu.Unlock()
	time.Sleep(r.delay)
	return true, ctx.Err()
}

func (r *ctxRecorder) counts() (calls, cancelled int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.cancelled
}

// On shutdown, messages already pulled into the client are finished, not failed: their
// processing must not inherit the shutdown signal's cancelled context.
func TestShutdownFinishesBufferedMessagesWithLiveContext(t *testing.T) {
	rec := &ctxRecorder{delay: 20 * time.Millisecond}
	h := start(t, rec)
	for i := range 30 {
		h.publish(t, fmt.Sprintf("CA%02d", i))
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls, _ := rec.counts(); calls == 0; calls, _ = rec.counts() {
		if time.Now().After(deadline) {
			t.Fatal("no message was processed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := h.stopRun(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls, cancelled := rec.counts()
	if cancelled != 0 {
		t.Errorf("%d of %d messages were processed with a cancelled context", cancelled, calls)
	}
	info, err := h.cons.Info(h.ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumAckPending != 0 {
		t.Errorf("ack pending after shutdown = %d, want 0: every delivered message should be finished", info.NumAckPending)
	}
	if int(info.Delivered.Consumer) != calls {
		t.Errorf("deliveries = %d, Record calls = %d; want equal (no retries caused by shutdown)", info.Delivered.Consumer, calls)
	}
}

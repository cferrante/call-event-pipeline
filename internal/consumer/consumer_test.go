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

	"github.com/cferrante/call-event-pipeline/internal/attribution"
	"github.com/cferrante/call-event-pipeline/internal/dbtest"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/natstest"
	"github.com/cferrante/call-event-pipeline/internal/stream"
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

// newServer starts a NATS server with the LEADS stream.
func newServer(t *testing.T) harness {
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
	return harness{ctx: ctx, js: js}
}

// run ensures the consumer described by cfg (and the DLQ), and processes messages with rec
// until the test ends. Retry delays are shortened for tests.
func (h harness) run(t *testing.T, cfg Config, rec Recorder) harness {
	t.Helper()
	cons, err := EnsureConsumer(h.ctx, h.js, cfg)
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}
	handler := &Handler{consumer: cfg.Name, js: h.js, rec: rec, retryDelays: []time.Duration{50 * time.Millisecond}}
	runCtx, stop := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, cons, handler, cfg.Workers) }()
	stopRun := sync.OnceValue(func() error {
		stop()
		return <-done
	})
	t.Cleanup(func() {
		if err := stopRun(); err != nil {
			t.Errorf("Run %s: %v", cfg.Name, err)
		}
	})
	h.cons, h.stopRun = cons, stopRun
	return h
}

// start runs the attribution consumer with rec on a fresh server.
func start(t *testing.T, rec Recorder) harness {
	t.Helper()
	return newServer(t).run(t, Attribution, rec)
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

// waitSettled waits until the consumer has acknowledged (or terminated) every message up to
// the stream's last one.
//
// It compares the consumer's ack floor with the stream's last sequence rather than trusting
// NumPending == 0 alone: JetStream updates a consumer's pending count asynchronously after a
// publish, so under CPU contention it can briefly read 0 while messages are still undelivered
// (seen in CI: 7 of 10 delivered when this returned).
func (h harness) waitSettled(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	s, err := h.js.Stream(h.ctx, stream.StreamName)
	if err != nil {
		t.Fatalf("stream %s: %v", stream.StreamName, err)
	}
	sinfo, err := s.Info(h.ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	last := sinfo.State.LastSeq
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := h.cons.Info(h.ctx)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if last > 0 && info.AckFloor.Stream >= last && info.NumAckPending == 0 && info.NumPending == 0 {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer did not settle: ack floor %d of %d, pending=%d ack_pending=%d",
				info.AckFloor.Stream, last, info.NumPending, info.NumAckPending)
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
	// Acks are sent without waiting for the server. A flush is a round trip, and the server
	// handles messages in order, so afterwards every ack sent before it has arrived.
	if err := h.js.Conn().FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
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

// hookRecorder always fails, calling onCall(n) first on its nth call.
type hookRecorder struct {
	mu     sync.Mutex
	n      int
	onCall func(n int)
}

func (r *hookRecorder) Record(context.Context, envelope.Envelope) (bool, error) {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	r.onCall(n)
	return false, errors.New("database unavailable")
}

// If dead-lettering fails on the final attempt, JetStream must still have one delivery left,
// so the message is retried and dead-lettered rather than stranded. The failure is real: the
// DLQ stream is deleted before attempt 5 and recreated before attempt 6.
func TestFailedDeadLetterOnFinalAttemptIsRetried(t *testing.T) {
	rec := &hookRecorder{}
	h := start(t, rec)
	rec.onCall = func(n int) {
		switch n {
		case maxDeliver:
			if err := h.js.DeleteStream(h.ctx, DLQStream); err != nil {
				t.Errorf("delete DLQ: %v", err)
			}
		case maxDeliver + 1:
			if _, err := EnsureConsumer(h.ctx, h.js, Attribution); err != nil {
				t.Errorf("recreate DLQ: %v", err)
			}
		}
	}

	h.publish(t, "CA1")
	info := h.waitSettled(t)

	if info.Delivered.Consumer != maxDeliver+1 {
		t.Fatalf("deliveries = %d, want %d: no spare delivery after the failed dead-letter",
			info.Delivered.Consumer, maxDeliver+1)
	}
	if n := len(h.dlqMessages(t)); n != 1 {
		t.Errorf("dead-lettered %d messages, want 1", n)
	}
}

// AckWait counts down for every message the client holds, including ones still waiting in its
// buffer. With a large buffer and slow writes, buffered messages expire and are redelivered
// before they're even handled, so the client must never hold more than a small batch.
func TestClientHoldsOnlyASmallBatch(t *testing.T) {
	rec := &ctxRecorder{delay: 10 * time.Millisecond}
	h := start(t, rec)
	for i := range 100 {
		h.publish(t, fmt.Sprintf("CA%03d", i))
	}

	maxHeld := 0
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := h.cons.Info(h.ctx)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		maxHeld = max(maxHeld, info.NumAckPending)
		if info.NumPending == 0 && info.NumAckPending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backlog did not drain")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The buffer holds up to pullBatch; each worker may be processing one more, and the fetch
	// loop may hold one more while waiting to hand it to a free worker.
	limit := pullBatch + Attribution.Workers + 1
	if maxHeld > limit {
		t.Errorf("client held up to %d unacked messages, want at most %d", maxHeld, limit)
	}
}

// concurrencyRecorder succeeds slowly and records the most calls it saw running at once.
type concurrencyRecorder struct {
	delay   time.Duration
	mu      sync.Mutex
	running int
	maxSeen int
	calls   int
}

func (r *concurrencyRecorder) Record(context.Context, envelope.Envelope) (bool, error) {
	r.mu.Lock()
	r.running++
	r.calls++
	r.maxSeen = max(r.maxSeen, r.running)
	r.mu.Unlock()
	time.Sleep(r.delay)
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return true, nil
}

func (r *concurrencyRecorder) stats() (calls, maxSeen int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.maxSeen
}

// processAll publishes n messages to a consumer running with cfg and returns the most
// messages it processed at once.
func processAll(t *testing.T, cfg Config, n int) int {
	t.Helper()
	rec := &concurrencyRecorder{delay: 30 * time.Millisecond}
	h := newServer(t).run(t, cfg, rec)
	for i := range n {
		h.publish(t, fmt.Sprintf("CA%03d", i))
	}
	h.waitSettled(t)
	calls, maxSeen := rec.stats()
	if calls != n {
		t.Fatalf("Record called %d times, want %d", calls, n)
	}
	return maxSeen
}

func TestWorkersProcessConcurrently(t *testing.T) {
	cfg := Config{Name: "inference", Workers: 4, MaxAckPending: 4}

	if got := processAll(t, cfg, 40); got != 4 {
		t.Errorf("at most %d messages processed at once, want 4", got)
	}
}

// MaxAckPending is enforced by JetStream, so it holds even if a process runs more workers
// than the model can serve (or several processes run).
func TestMaxAckPendingCapsWorkers(t *testing.T) {
	cfg := Config{Name: "inference", Workers: 10, MaxAckPending: 4}

	if got := processAll(t, cfg, 40); got > 4 {
		t.Errorf("at most %d messages processed at once, want no more than MaxAckPending 4", got)
	}
}

// Attribution keeps processing one message at a time.
func TestAttributionIsSequential(t *testing.T) {
	if got := processAll(t, Attribution, 20); got != 1 {
		t.Errorf("at most %d messages processed at once, want 1", got)
	}
}

// Bulkhead: two consumers on one stream keep separate progress, so a stalled one doesn't
// hold the other back.
func TestConsumersProgressIndependently(t *testing.T) {
	srv := newServer(t)
	stalled := make(chan struct{})
	inference := srv.run(t, Config{Name: "inference", Workers: 1, MaxAckPending: 1}, blockingRecorder(stalled))
	attr := srv.run(t, Attribution, &fakeRecorder{result: true})
	// Registered after run, so it runs before run's cleanup (cleanups run last-in, first-out):
	// releasing the stalled recorder first lets shutdown finish at once.
	t.Cleanup(func() { close(stalled) })

	for i := range 10 {
		srv.publish(t, fmt.Sprintf("CA%02d", i))
	}
	info := attr.waitSettled(t)

	if info.Delivered.Consumer != 10 {
		t.Errorf("attribution deliveries = %d, want 10", info.Delivered.Consumer)
	}
	inf, err := inference.cons.Info(srv.ctx)
	if err != nil {
		t.Fatalf("inference info: %v", err)
	}
	if inf.NumPending+uint64(inf.NumAckPending) != 10 {
		t.Errorf("inference pending = %d + %d in progress, want all 10 still waiting", inf.NumPending, inf.NumAckPending)
	}
}

// blockingRecorder never finishes until release is closed.
type blockingRecorder chan struct{}

func (b blockingRecorder) Record(ctx context.Context, _ envelope.Envelope) (bool, error) {
	select {
	case <-b:
	case <-ctx.Done():
	}
	return false, errors.New("stalled")
}

// Both consumers share the DLQ. Each dead letter records which consumer gave up, and one
// consumer's dead letter must not be dropped as a duplicate of the other's.
func TestDeadLettersFromBothConsumersAreKept(t *testing.T) {
	srv := newServer(t)
	attr := srv.run(t, Attribution, &fakeRecorder{result: true})
	inference := srv.run(t, Config{Name: "inference", Workers: 4, MaxAckPending: 4}, &fakeRecorder{result: true})

	srv.publishRaw(t, "not an envelope")
	attr.waitSettled(t)
	inference.waitSettled(t)

	dlq := srv.dlqMessages(t)
	got := map[string]bool{}
	for _, m := range dlq {
		got[m.Header.Get(HeaderConsumer)] = true
	}
	if len(dlq) != 2 || !got["attribution"] || !got["inference"] {
		t.Errorf("dead letters by consumer = %v (%d messages), want one each from attribution and inference", got, len(dlq))
	}
}

// A new inference consumer starts at new messages, so its first start doesn't spend model
// capacity on the stream's 7-day history.
func TestInferenceStartsAtNewMessages(t *testing.T) {
	srv := newServer(t)
	for i := range 5 {
		srv.publish(t, fmt.Sprintf("CAold%d", i))
	}
	rec := &fakeRecorder{result: true}
	h := srv.run(t, Inference(4), rec)

	for i := range 3 {
		h.publish(t, fmt.Sprintf("CAnew%d", i))
	}
	h.waitSettled(t)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, id := range rec.calls {
		if strings.HasPrefix(id, "CAold") {
			t.Errorf("processed %s, published before the consumer existed", id)
		}
	}
	if len(rec.calls) != 3 {
		t.Errorf("processed %v, want the 3 new messages", rec.calls)
	}
}

// Attribution must record every lead, so it starts at the beginning of the stream.
func TestAttributionStartsAtAllMessages(t *testing.T) {
	srv := newServer(t)
	for i := range 5 {
		srv.publish(t, fmt.Sprintf("CAold%d", i))
	}
	rec := &fakeRecorder{result: true}
	srv.run(t, Attribution, rec).waitSettled(t)

	if n := rec.callCount(); n != 5 {
		t.Errorf("processed %d messages, want all 5", n)
	}
}

// JetStream can't change where an existing consumer starts. An inference consumer created
// before this change reads all history, so starting must fail with instructions, not a bare
// API error; after a reset it works.
func TestChangedStartPositionNeedsReset(t *testing.T) {
	srv := newServer(t)
	if _, err := srv.js.CreateOrUpdateConsumer(srv.ctx, stream.StreamName, jetstream.ConsumerConfig{
		Durable: "inference", AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("create old consumer: %v", err)
	}

	_, err := EnsureConsumer(srv.ctx, srv.js, Inference(4))
	if err == nil || !strings.Contains(err.Error(), "-reset-consumer") {
		t.Fatalf("err = %v, want one explaining -reset-consumer", err)
	}

	if err := ResetConsumer(srv.ctx, srv.js, "inference"); err != nil {
		t.Fatalf("ResetConsumer: %v", err)
	}
	if _, err := EnsureConsumer(srv.ctx, srv.js, Inference(4)); err != nil {
		t.Errorf("EnsureConsumer after reset: %v", err)
	}
}

// Contract rule: a consumer dead-letters a major version or a channel it doesn't support on
// the first delivery, with the reason, so an upgraded consumer can replay it. It never
// retries what can't succeed, and never guesses.
func TestUnsupportedVersionOrChannelIsDeadLetteredOnFirstDelivery(t *testing.T) {
	valid := `{"schema_version":1,"channel":"sms","provider":"twilio","provider_id":"SM1",` +
		`"from":"+14155550101","to":"+18005550002","received_at":"2026-10-27T12:00:01Z","params":{}}`
	cases := map[string]struct{ data, reason string }{
		"major version 2": {strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), "schema_version"},
		"unknown channel": {strings.Replace(valid, `"channel":"sms"`, `"channel":"fax"`, 1), "channel"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &fakeRecorder{result: true}
			h := start(t, rec)
			h.publishRaw(t, c.data)
			info := h.waitSettled(t)

			if rec.callCount() != 0 {
				t.Errorf("Record called %d times, want 0", rec.callCount())
			}
			if info.Delivered.Consumer != 1 {
				t.Errorf("deliveries = %d, want 1", info.Delivered.Consumer)
			}
			dlq := h.dlqMessages(t)
			if len(dlq) != 1 {
				t.Fatalf("dead-lettered %d messages, want 1", len(dlq))
			}
			if got := dlq[0].Header.Get(HeaderReason); !strings.Contains(got, c.reason) {
				t.Errorf("%s = %q, want it to name the %s", HeaderReason, got, c.reason)
			}
		})
	}
}

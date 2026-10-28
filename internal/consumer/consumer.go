// Package consumer runs a durable consumer on the LEADS stream (attribution or inference): it
// reads lead events, records them, and acks, retries or dead-letters each one.
package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
	"github.com/cferrante/decoupled-ingestion-layer/internal/stream"
)

const (
	// DLQStream holds messages that can never be processed, for a human to inspect.
	DLQStream  = "LEADS_DLQ"
	DLQSubject = "leads.dlq"

	// Headers added to each dead letter.
	HeaderReason   = "Dlq-Reason"   // why it was dead-lettered
	HeaderSubject  = "Dlq-Subject"  // the subject it was originally published on
	HeaderConsumer = "Dlq-Consumer" // the consumer that gave up on it

	// ackWait is how long JetStream waits for an ack before redelivering.
	ackWait = 30 * time.Second
	// recordTimeout keeps one attempt shorter than ackWait, so a stuck attempt fails and is
	// retried by us rather than redelivered while it is still running.
	recordTimeout = 20 * time.Second
	// maxDeliver is the most attempts a message gets; the last one dead-letters on failure.
	maxDeliver = 5
	// pullBatch is the most messages the client buffers. On top of those, each worker may be
	// processing one and the fetch loop may hold one waiting for a free worker. AckWait counts
	// down for all of them, so they must be processable well within it even when writes are
	// slow; for attribution's single worker: (10 + 1 + 1) × 2s = 24s < 30s. Inference is also
	// capped by MaxAckPending on the server.
	pullBatch = 10
)

// defaultRetryDelays grow with each attempt; the last one repeats.
var defaultRetryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// Config describes one durable consumer on the LEADS stream.
type Config struct {
	Name string // durable consumer name
	// MaxAckPending caps unfinished messages across all of this consumer's workers, enforced
	// by JetStream on the server. 0 means JetStream's default.
	MaxAckPending int
	// Workers is how many messages one process handles at once.
	Workers int
}

// Attribution is the attribution consumer: one message at a time.
var Attribution = Config{Name: "attribution", Workers: 1}

// Inference is the inference consumer with the given number of workers, which should match
// the model's capacity. MaxAckPending is set to the same number, so JetStream never hands out
// more unfinished messages than the workers (and the model) can take.
func Inference(workers int) Config {
	return Config{Name: "inference", Workers: workers, MaxAckPending: workers}
}

// Recorder stores an event, reporting false if it was already stored.
type Recorder interface {
	Record(ctx context.Context, env envelope.Envelope) (bool, error)
}

// EnsureConsumer creates or updates the dead-letter stream and the durable attribution
// consumer. It deliberately sets no BackOff: JetStream uses BackOff's first value in place of
// AckWait, so slow processing would be redelivered while still running. Retry delays come
// from NakWithDelay instead.
func EnsureConsumer(ctx context.Context, js jetstream.JetStream, cfg Config) (jetstream.Consumer, error) {
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     DLQStream,
		Subjects: []string{DLQSubject},
		Storage:  jetstream.FileStorage,
		MaxAge:   30 * 24 * time.Hour,
	}); err != nil {
		return nil, fmt.Errorf("ensure stream %s: %w", DLQStream, err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, stream.StreamName, jetstream.ConsumerConfig{
		Durable:       cfg.Name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxAckPending: cfg.MaxAckPending,
		// One spare delivery: if dead-lettering fails on the final attempt (or the worker
		// crashes during it), JetStream redelivers once more instead of stranding the message.
		MaxDeliver: maxDeliver + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure consumer %s: %w", cfg.Name, err)
	}
	return cons, nil
}

// Handler decides what happens to each message.
type Handler struct {
	consumer    string
	js          jetstream.JetStream
	rec         Recorder
	retryDelays []time.Duration
}

func NewHandler(consumer string, js jetstream.JetStream, rec Recorder) *Handler {
	return &Handler{consumer: consumer, js: js, rec: rec, retryDelays: defaultRetryDelays}
}

// Run processes messages with the given number of workers until ctx is cancelled, then
// finishes in-flight work and returns. Cancelling ctx stops fetching; messages already
// received are still processed, with a context the shutdown can't cancel (each attempt keeps
// its own time limit).
func Run(ctx context.Context, cons jetstream.Consumer, h *Handler, workers int) error {
	workCtx := context.WithoutCancel(ctx)
	// Unbuffered: the fetch loop hands a message over only when a worker is free, so the
	// client holds at most pullBatch waiting messages plus one per worker.
	work := make(chan jetstream.Msg)
	var wg sync.WaitGroup
	for range max(1, workers) {
		wg.Go(func() {
			for msg := range work {
				h.Handle(workCtx, msg)
			}
		})
	}
	defer func() {
		close(work) // safe: no more messages arrive once Consume has stopped
		wg.Wait()
	}()

	cc, err := cons.Consume(func(msg jetstream.Msg) { work <- msg },
		jetstream.PullMaxMessages(pullBatch))
	if err != nil {
		return fmt.Errorf("start consuming: %w", err)
	}
	<-ctx.Done()
	cc.Drain()
	<-cc.Closed()
	return nil
}

// Handle acks a recorded message, dead-letters one that can never succeed, and retries
// one that failed for a reason that may pass (e.g. the database is down).
func (h *Handler) Handle(ctx context.Context, msg jetstream.Msg) {
	env, err := envelope.Parse(msg.Data())
	if err != nil {
		h.deadLetter(ctx, msg, err)
		return
	}

	recordCtx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	if _, err := h.rec.Record(recordCtx, env); err != nil {
		// On the final attempt JetStream won't redeliver, so dead-letter it ourselves rather
		// than rely on its MaxDeliver advisory, which is lost if nobody is listening.
		if n := attempt(msg); n >= maxDeliver {
			h.deadLetter(ctx, msg, fmt.Errorf("gave up after %d attempts: %w", n, err))
			return
		}
		delay := h.retryDelay(msg)
		slog.Warn("record failed; will retry", "provider_id", env.ProviderID, "delay", delay, "err", err)
		if err := msg.NakWithDelay(delay); err != nil {
			slog.Error("nak failed", "provider_id", env.ProviderID, "err", err)
		}
		return
	}
	if err := msg.Ack(); err != nil {
		// Recording is idempotent, so the redelivery this causes is harmless.
		slog.Error("ack failed", "provider_id", env.ProviderID, "err", err)
	}
}

// deadLetter stores the message in the DLQ, then terminates it so it is never redelivered.
// If the DLQ write fails the message is retried instead, so it is never silently dropped.
func (h *Handler) deadLetter(ctx context.Context, msg jetstream.Msg, cause error) {
	out := nats.NewMsg(DLQSubject)
	out.Data = msg.Data()
	out.Header.Set(HeaderReason, cause.Error())
	out.Header.Set(HeaderSubject, msg.Subject())
	out.Header.Set(HeaderConsumer, h.consumer)
	if meta, err := msg.Metadata(); err == nil {
		// Dedupes the dead letter if we crash after storing it but before terminating. The
		// consumer is part of the ID: another consumer giving up on the same message is a
		// separate dead letter, not a duplicate.
		out.Header.Set(nats.MsgIdHdr,
			stream.StreamName+"-"+strconv.FormatUint(meta.Sequence.Stream, 10)+"-"+h.consumer)
	}

	if _, err := h.js.PublishMsg(ctx, out); err != nil {
		slog.Error("dead-letter failed; will retry", "err", err)
		_ = msg.NakWithDelay(h.retryDelay(msg))
		return
	}
	slog.Warn("dead-lettered message", "consumer", h.consumer, "subject", msg.Subject(), "reason", cause)
	if err := msg.Term(); err != nil {
		slog.Error("term failed", "err", err)
	}
}

// retryDelay picks the delay for this message's attempt number.
func (h *Handler) retryDelay(msg jetstream.Msg) time.Duration {
	return h.retryDelays[min(attempt(msg), len(h.retryDelays))-1]
}

// attempt is the message's delivery count: 1 on first delivery.
func attempt(msg jetstream.Msg) int {
	if meta, err := msg.Metadata(); err == nil {
		return int(meta.NumDelivered)
	}
	return 1
}

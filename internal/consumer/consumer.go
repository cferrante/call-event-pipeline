// Package consumer runs the attribution consumer: it reads lead events from the LEADS
// stream, records them, and acks, retries or dead-letters each one.
package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
	"github.com/cferrante/decoupled-ingestion-layer/internal/stream"
)

const (
	// ConsumerName is the durable JetStream consumer for attribution.
	ConsumerName = "attribution"

	// DLQStream holds messages that can never be processed, for a human to inspect.
	DLQStream  = "LEADS_DLQ"
	DLQSubject = "leads.dlq"

	// Headers added to each dead letter.
	HeaderReason  = "Dlq-Reason"  // why it was dead-lettered
	HeaderSubject = "Dlq-Subject" // the subject it was originally published on

	// ackWait is how long JetStream waits for an ack before redelivering.
	ackWait = 30 * time.Second
	// recordTimeout keeps one attempt shorter than ackWait, so a stuck attempt fails and is
	// retried by us rather than redelivered while it is still running.
	recordTimeout = 20 * time.Second
	// maxDeliver is the most attempts a message gets; the last one dead-letters on failure.
	maxDeliver = 5
	// pullBatch is the most messages the client buffers; one more may be in progress. AckWait
	// counts down for each while it waits in the buffer, so all of them must be processable well
	// within AckWait even when writes are slow: (10 + 1) × 2s = 22s < 30s.
	pullBatch = 10
)

// defaultRetryDelays grow with each attempt; the last one repeats.
var defaultRetryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// Recorder stores an event, reporting false if it was already stored.
type Recorder interface {
	Record(ctx context.Context, env envelope.Envelope) (bool, error)
}

// EnsureConsumer creates or updates the dead-letter stream and the durable attribution
// consumer. It deliberately sets no BackOff: JetStream uses BackOff's first value in place of
// AckWait, so slow processing would be redelivered while still running. Retry delays come
// from NakWithDelay instead.
func EnsureConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     DLQStream,
		Subjects: []string{DLQSubject},
		Storage:  jetstream.FileStorage,
		MaxAge:   30 * 24 * time.Hour,
	}); err != nil {
		return nil, fmt.Errorf("ensure stream %s: %w", DLQStream, err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, stream.StreamName, jetstream.ConsumerConfig{
		Durable:   ConsumerName,
		AckPolicy: jetstream.AckExplicitPolicy,
		AckWait:   ackWait,
		// One spare delivery: if dead-lettering fails on the final attempt (or the worker
		// crashes during it), JetStream redelivers once more instead of stranding the message.
		MaxDeliver: maxDeliver + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure consumer %s: %w", ConsumerName, err)
	}
	return cons, nil
}

// Handler decides what happens to each message.
type Handler struct {
	js          jetstream.JetStream
	rec         Recorder
	retryDelays []time.Duration
}

func NewHandler(js jetstream.JetStream, rec Recorder) *Handler {
	return &Handler{js: js, rec: rec, retryDelays: defaultRetryDelays}
}

// Run processes messages until ctx is cancelled, then finishes in-flight work and returns.
// Cancelling ctx stops fetching; messages already received are still processed, with a
// context the shutdown can't cancel (each attempt keeps its own time limit).
func Run(ctx context.Context, cons jetstream.Consumer, h *Handler) error {
	workCtx := context.WithoutCancel(ctx)
	cc, err := cons.Consume(func(msg jetstream.Msg) { h.Handle(workCtx, msg) },
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
	if meta, err := msg.Metadata(); err == nil {
		// Dedupes the dead letter if we crash after storing it but before terminating.
		out.Header.Set(nats.MsgIdHdr, stream.StreamName+"-"+strconv.FormatUint(meta.Sequence.Stream, 10))
	}

	if _, err := h.js.PublishMsg(ctx, out); err != nil {
		slog.Error("dead-letter failed; will retry", "err", err)
		_ = msg.NakWithDelay(h.retryDelay(msg))
		return
	}
	slog.Warn("dead-lettered message", "subject", msg.Subject(), "reason", cause)
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

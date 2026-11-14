package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/consumer"
	"github.com/cferrante/call-event-pipeline/internal/stream"
)

// writtenQuery counts each consumer's rows. count(*) is a scan, which is fine at demo scale
// (around 10 ms per 100,000 rows) every 5 seconds; at production scale this would read a
// counter instead.
var writtenQuery = map[string]string{
	"attribution": "SELECT count(*) FROM interactions",
	"inference":   "SELECT count(*) FROM insights",
}

// Sampler reads the pipeline's state from JetStream and Postgres. It only reads: it never
// creates the stream or the consumers it watches. Not safe for concurrent use.
type Sampler struct {
	js         jetstream.JetStream
	db         *pgxpool.Pool
	canaryFrom string

	// Dead letters are tallied as they arrive, by reading the DLQ once with the monitor's own
	// ordered consumer, rather than rescanning the whole DLQ on every sample.
	dlq      jetstream.Consumer
	deadByCo map[string]uint64
	// dlqSeq is the last DLQ stream sequence counted. An ordered consumer can hand a message
	// over again on a later fetch (seen in testing), so counting is keyed on the sequence.
	dlqSeq uint64
}

// NewSampler prepares a sampler. canaryFrom is the From the canary webhooks use.
func NewSampler(ctx context.Context, js jetstream.JetStream, db *pgxpool.Pool, canaryFrom string) (*Sampler, error) {
	return &Sampler{js: js, db: db, canaryFrom: canaryFrom, deadByCo: map[string]uint64{}}, nil
}

// Sample takes one reading. A consumer that doesn't exist yet is left out.
func (s *Sampler) Sample(ctx context.Context, now time.Time) (Sample, error) {
	out := Sample{At: now, Consumers: map[string]ConsumerSample{}}
	leads, err := s.js.Stream(ctx, stream.StreamName)
	if err != nil {
		return out, fmt.Errorf("stream %s: %w", stream.StreamName, err)
	}
	info, err := leads.Info(ctx)
	if err != nil {
		return out, fmt.Errorf("stream info: %w", err)
	}
	out.Arrivals = info.State.LastSeq

	var canary *time.Time
	if err := s.db.QueryRow(ctx, `SELECT max(ingested_at) FROM interactions WHERE from_number = $1`, s.canaryFrom).
		Scan(&canary); err != nil {
		return out, fmt.Errorf("canary: %w", err)
	}
	if canary != nil {
		out.CanarySeen = *canary
	}

	if err := s.tallyDeadLetters(ctx); err != nil {
		return out, err
	}
	for name, query := range writtenQuery {
		cons, err := leads.Consumer(ctx, name)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("consumer %s: %w", name, err)
		}
		ci, err := cons.Info(ctx)
		if err != nil {
			return out, fmt.Errorf("consumer %s info: %w", name, err)
		}
		// Acknowledged: everything delivered except what still awaits an ack. Not the ack
		// floor, which stalls behind one slow or retried message and then jumps past every
		// later one, so a window looked like thousands acknowledged and none written.
		acked := ci.Delivered.Stream
		if pending := uint64(ci.NumAckPending); pending <= acked {
			acked -= pending
		}
		cs := ConsumerSample{Acked: acked, DeadLettered: s.deadByCo[name]}
		if err := s.db.QueryRow(ctx, query).Scan(&cs.Written); err != nil {
			return out, fmt.Errorf("rows written by %s: %w", name, err)
		}
		if ci.NumPending+uint64(ci.NumAckPending) > 0 {
			// The oldest message it hasn't finished is the one just after its ack floor.
			if m, err := leads.GetMsg(ctx, ci.AckFloor.Stream+1); err == nil {
				cs.OldestPending = m.Time
			}
		}
		out.Consumers[name] = cs
	}
	return out, nil
}

// tallyDeadLetters reads any dead letters that arrived since the last sample and counts them
// by the consumer that gave up on each.
func (s *Sampler) tallyDeadLetters(ctx context.Context) error {
	if s.dlq == nil {
		c, err := s.js.OrderedConsumer(ctx, consumer.DLQStream, jetstream.OrderedConsumerConfig{})
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil // no consumer has run yet, so there are no dead letters
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", consumer.DLQStream, err)
		}
		s.dlq = c
	}
	for {
		batch, err := s.dlq.FetchNoWait(100)
		if err != nil {
			return fmt.Errorf("read %s: %w", consumer.DLQStream, err)
		}
		n := 0
		for m := range batch.Messages() {
			n++
			meta, err := m.Metadata()
			if err != nil || meta.Sequence.Stream <= s.dlqSeq {
				continue
			}
			s.dlqSeq = meta.Sequence.Stream
			s.deadByCo[m.Headers().Get(consumer.HeaderConsumer)]++
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			return fmt.Errorf("read %s: %w", consumer.DLQStream, err)
		}
		if n < 100 {
			return nil
		}
	}
}

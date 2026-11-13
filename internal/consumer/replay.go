package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/stream"
)

// replayRetryDelay is the wait after a failed write during replay, growing with each attempt.
var replayRetryDelay = time.Second

const (
	replayAttempts = 5
	replayBatch    = 100
)

// ReplayResult counts what a replay did.
type ReplayResult struct {
	Read            int // messages read from the stream
	Recorded        int // written now: these were missing
	AlreadyRecorded int // present already, so left unchanged
	Unparseable     int // skipped: they were dead-lettered when first delivered
}

// Replay re-records every message published at or after from, up to the stream's last message
// when the replay starts (docs/design.md, section 5.3). It reads through a temporary consumer
// that it deletes when done, so the durable consumer's position never moves; recording is
// idempotent, so messages already stored are left as they are. If a write keeps failing it
// stops with an error, and running it again is safe.
func Replay(ctx context.Context, js jetstream.JetStream, rec Recorder, from time.Time) (ReplayResult, error) {
	var res ReplayResult
	s, err := js.Stream(ctx, stream.StreamName)
	if err != nil {
		return res, fmt.Errorf("replay: stream %s: %w", stream.StreamName, err)
	}
	state := s.CachedInfo().State
	if state.Msgs == 0 || state.LastTime.Before(from) {
		return res, nil // nothing published since then
	}
	last := state.LastSeq

	name := fmt.Sprintf("replay-%d", time.Now().UnixNano())
	cons, err := js.CreateConsumer(ctx, stream.StreamName, jetstream.ConsumerConfig{
		Name:          name,
		DeliverPolicy: jetstream.DeliverByStartTimePolicy,
		OptStartTime:  &from,
		AckPolicy:     jetstream.AckNonePolicy,
		MemoryStorage: true,
		// Removed by the server even if this process dies before deleting it.
		InactiveThreshold: 5 * time.Minute,
	})
	if err != nil {
		return res, fmt.Errorf("replay: create consumer: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := js.DeleteConsumer(dctx, stream.StreamName, name); err != nil {
			slog.Error("replay: delete temporary consumer; the server removes it after 5 minutes idle",
				"consumer", name, "err", err)
		}
	}()
	slog.Info("replay started", "from", from.UTC().Format(time.RFC3339), "until_stream_sequence", last)

	for {
		batch, err := cons.Fetch(replayBatch, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			return res, fmt.Errorf("replay: fetch: %w", err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			meta, err := msg.Metadata()
			if err != nil {
				return res, fmt.Errorf("replay: message metadata: %w", err)
			}
			if meta.Sequence.Stream > last {
				return res, nil // published after the replay started; the durable consumer has it
			}
			if err := replayOne(ctx, rec, msg.Data(), &res); err != nil {
				return res, fmt.Errorf("replay stopped at stream sequence %d: %w", meta.Sequence.Stream, err)
			}
			if meta.Sequence.Stream == last {
				return res, nil
			}
			if res.Read%1000 == 0 {
				slog.Info("replay progress", "read", res.Read, "recorded", res.Recorded, "sequence", meta.Sequence.Stream)
			}
		}
		if err := batch.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return res, fmt.Errorf("replay: fetch: %w", err)
		}
		if got == 0 {
			// Nothing more to read: the remaining messages were removed by the stream's limits.
			info, err := cons.Info(ctx)
			if err != nil {
				return res, fmt.Errorf("replay: consumer info: %w", err)
			}
			if info.NumPending == 0 {
				return res, nil
			}
		}
	}
}

func replayOne(ctx context.Context, rec Recorder, data []byte, res *ReplayResult) error {
	res.Read++
	env, err := envelope.Parse(data)
	if err != nil {
		res.Unparseable++
		return nil
	}
	for attempt := 1; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, recordTimeout)
		recorded, err := rec.Record(rctx, env)
		cancel()
		if err == nil {
			if recorded {
				res.Recorded++
			} else {
				res.AlreadyRecorded++
			}
			return nil
		}
		if attempt == replayAttempts || ctx.Err() != nil {
			return fmt.Errorf("record %s: %w", env.ProviderID, err)
		}
		slog.Warn("replay: record failed; will retry", "provider_id", env.ProviderID, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * replayRetryDelay):
		}
	}
}

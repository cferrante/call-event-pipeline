//go:build integration

package consumer

import (
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/stream"
)

func init() { replayRetryDelay = 10 * time.Millisecond }

// consumerNames lists the LEADS stream's consumers.
func (h harness) consumerNames(t *testing.T) []string {
	t.Helper()
	s, err := h.js.Stream(h.ctx, stream.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range s.ConsumerNames(h.ctx).Name() {
		names = append(names, name)
	}
	return names
}

// Replay re-records every message from the given time on, skips (and counts) the ones that
// can't be parsed, as they were dead-lettered the first time, and leaves no consumer behind.
// The durable consumer is untouched: its position doesn't move.
func TestReplayFromTimeRecordsLaterMessagesOnly(t *testing.T) {
	h := start(t, &fakeRecorder{result: true})
	h.publish(t, "CAbefore1")
	h.publish(t, "CAbefore2")
	h.waitSettled(t)
	time.Sleep(20 * time.Millisecond)
	from := time.Now()
	time.Sleep(20 * time.Millisecond)
	h.publish(t, "CAafter1")
	h.publishRaw(t, "not an envelope")
	h.publish(t, "CAafter2")
	durableBefore := h.waitSettled(t)

	rec := &fakeRecorder{result: true}
	res, err := Replay(h.ctx, h.js, rec, from)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !slices.Equal(rec.calls, []string{"CAafter1", "CAafter2"}) {
		t.Errorf("replayed %v, want CAafter1 and CAafter2 only", rec.calls)
	}
	if res != (ReplayResult{Read: 3, Recorded: 2, Unparseable: 1}) {
		t.Errorf("result %+v", res)
	}
	if names := h.consumerNames(t); !slices.Equal(names, []string{Attribution.Name}) {
		t.Errorf("consumers after replay %v, want only the durable %q", names, Attribution.Name)
	}
	after, err := h.cons.Info(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.AckFloor.Stream != durableBefore.AckFloor.Stream || after.Delivered.Stream != durableBefore.Delivered.Stream {
		t.Errorf("durable consumer moved: ack floor %d -> %d, delivered %d -> %d", durableBefore.AckFloor.Stream,
			after.AckFloor.Stream, durableBefore.Delivered.Stream, after.Delivered.Stream)
	}
}

// Messages already recorded are counted as present, not recorded again: replaying a complete
// period changes nothing.
func TestReplayCountsAlreadyRecorded(t *testing.T) {
	h := newServer(t)
	from := time.Now()
	h.publish(t, "CA1")
	h.publish(t, "CA2")
	res, err := Replay(h.ctx, h.js, &fakeRecorder{result: false}, from)
	if err != nil || res != (ReplayResult{Read: 2, AlreadyRecorded: 2}) {
		t.Errorf("result %+v, err %v; want 2 read, 2 already recorded", res, err)
	}
}

// A store that fails briefly is retried; one that keeps failing stops the replay with an
// error (re-running it is safe, since recording is idempotent), and the consumer is still
// removed.
func TestReplayRetriesThenFails(t *testing.T) {
	h := newServer(t)
	from := time.Now()
	h.publish(t, "CA1")

	flaky := &fakeRecorder{result: true, failures: 2}
	if res, err := Replay(h.ctx, h.js, flaky, from); err != nil || res.Recorded != 1 {
		t.Errorf("brief failure: result %+v, err %v; want it recorded", res, err)
	}
	down := &fakeRecorder{failures: 1000}
	if _, err := Replay(h.ctx, h.js, down, from); err == nil {
		t.Error("a store that keeps failing didn't stop the replay")
	}
	if names := h.consumerNames(t); len(names) != 0 {
		t.Errorf("consumers left after a failed replay: %v", names)
	}
}

// Nothing published since the start time: the replay finishes at once with nothing to do,
// instead of waiting for messages that won't come.
func TestReplayWithNothingToDo(t *testing.T) {
	h := newServer(t)
	h.publish(t, "CA1")
	began := time.Now()
	res, err := Replay(h.ctx, h.js, &fakeRecorder{}, time.Now().Add(time.Minute))
	if err != nil || res != (ReplayResult{}) {
		t.Errorf("result %+v, err %v; want nothing done", res, err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("took %v with nothing to replay", d)
	}
	var _ jetstream.JetStream = h.js
}

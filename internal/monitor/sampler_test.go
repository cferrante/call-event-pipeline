//go:build integration

package monitor

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/consumer"
	"github.com/cferrante/call-event-pipeline/internal/dbtest"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/natstest"
	"github.com/cferrante/call-event-pipeline/internal/stream"
)

// The sampler reads real JetStream and Postgres state: arrivals, each consumer's acknowledgements,
// rows written, dead letters per consumer, the oldest pending message, and the canary.
func TestSamplerReadsThePipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	js, err := jetstream.New(natstest.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	db := dbtest.Migrated(t)
	if err := stream.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	attr, err := consumer.EnsureConsumer(ctx, js, consumer.Attribution)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewSampler(ctx, js, db, "canary")
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	first, err := s.Sample(ctx, time.Now())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if first.Arrivals != 0 || !first.CanarySeen.IsZero() {
		t.Errorf("empty pipeline sample = %+v", first)
	}
	if _, ok := first.Consumers["inference"]; ok {
		t.Error("inference consumer reported before it exists")
	}

	pub := stream.NewPublisher(js)
	publishedAt := map[int]time.Time{}
	for i := range 5 {
		env, err := envelope.FromWebhook(envelope.Voice, url.Values{
			"CallSid": {fmt.Sprintf("CA%d", i)}, "From": {"+14158675310"}, "To": {"+18005550001"},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		publishedAt[i] = time.Now()
		if err := pub.Publish(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	// Attribution handles the first 3: two written, one dead-lettered.
	batch, err := attr.Fetch(3, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for m := range batch.Messages() {
		if err := m.Ack(); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("fetched %d, want 3", n)
	}
	for i, from := range []string{"+14158675310", "canary"} {
		if _, err := db.Exec(ctx, `INSERT INTO interactions (provider_id, channel, from_number, to_e164, source, occurred_at, raw)
			VALUES ($1, 'voice', $2, '+18005550001', 'unknown', now(), '{}')`, fmt.Sprintf("CA%d", i), from); err != nil {
			t.Fatal(err)
		}
	}
	dl := nats.NewMsg(consumer.DLQSubject)
	dl.Data = []byte("x")
	dl.Header.Set(consumer.HeaderConsumer, "attribution")
	if _, err := js.PublishMsg(ctx, dl); err != nil {
		t.Fatal(err)
	}

	got, err := s.Sample(ctx, time.Now())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	a, ok := got.Consumers["attribution"]
	switch {
	case got.Arrivals != 5:
		t.Errorf("arrivals = %d, want 5", got.Arrivals)
	case !ok:
		t.Fatal("attribution missing")
	case a.Acked != 3 || a.Written != 2 || a.DeadLettered != 1:
		t.Errorf("attribution = %+v, want acked 3, written 2, dead-lettered 1", a)
	case a.OldestPending.Before(publishedAt[3].Add(-time.Second)) || a.OldestPending.After(publishedAt[3].Add(time.Second)):
		t.Errorf("oldest pending = %v, want about %v (message 4's publish time)", a.OldestPending, publishedAt[3])
	case got.CanarySeen.IsZero():
		t.Error("canary not seen")
	}

	// Dead letters are tallied once, not recounted on every sample.
	again, err := s.Sample(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := again.Consumers["attribution"].DeadLettered; d != 1 {
		t.Errorf("dead letters on the next sample = %d, want still 1", d)
	}
}

// A message still in progress (a slow or retried call) must not hide the acknowledgements after
// it. The ack floor stays behind it, then jumps past every later message at once, which the
// acked-but-not-written rule read as thousands acknowledged and none written: four false alerts
// in step 16's control run (docs/plan-m3.md).
func TestSamplerCountsAcksPastAMessageInProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	js, err := jetstream.New(natstest.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	db := dbtest.Migrated(t)
	if err := stream.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	attr, err := consumer.EnsureConsumer(ctx, js, consumer.Attribution)
	if err != nil {
		t.Fatal(err)
	}
	pub := stream.NewPublisher(js)
	for i := range 5 {
		env, err := envelope.FromWebhook(envelope.Voice, url.Values{
			"CallSid": {fmt.Sprintf("CA%d", i)}, "From": {"+14158675310"}, "To": {"+18005550001"},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := pub.Publish(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := attr.Fetch(5, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var first jetstream.Msg
	for m := range batch.Messages() {
		if first == nil {
			first = m // in progress: not acknowledged yet
			continue
		}
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewSampler(ctx, js, db, "canary")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Sample(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Consumers["attribution"].Acked; a != 4 {
		t.Errorf("acked = %d with message 1 in progress and 2–5 acknowledged, want 4", a)
	}

	if err := first.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.Sample(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Consumers["attribution"].Acked; a != 5 {
		t.Errorf("acked = %d after message 1 is acknowledged too, want 5", a)
	}
}

// A reading runs every 5 s for as long as the monitor does, so it asks JetStream only what it
// needs: the stream's info and each existing consumer's, once each (looking one up already
// returns its info, so asking again would double the requests), plus 2 for the dead-letter
// reader, an ordered consumer that nats.go recreates for every fetch. 8 before the fix.
func TestSamplerMakesOneRequestPerStreamAndConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	js, err := jetstream.New(natstest.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	db := dbtest.Migrated(t)
	if err := stream.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	for _, c := range []consumer.Config{consumer.Attribution, consumer.Inference(1)} {
		if _, err := consumer.EnsureConsumer(ctx, js, c); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewSampler(ctx, js, db, "canary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sample(ctx, time.Now()); err != nil { // the first reading sets up the DLQ reader
		t.Fatal(err)
	}
	apiTotal := func() uint64 {
		info, err := js.AccountInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return info.API.Total
	}
	before := apiTotal()
	if _, err := s.Sample(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The AccountInfo call itself is one request.
	got := apiTotal() - before - 1
	t.Logf("one reading: %d JetStream API requests", got)
	if got > 5 {
		t.Errorf("one reading made %d JetStream API requests, want at most 5 (the stream, two consumers, the dead-letter reader's 2)", got)
	}
}

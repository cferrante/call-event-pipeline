//go:build integration

package stream

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/natstest"
)

func setup(t *testing.T) (context.Context, jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	js, err := jetstream.New(natstest.Start(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return ctx, js
}

func leadsStream(ctx context.Context, t *testing.T, js jetstream.JetStream) jetstream.Stream {
	t.Helper()
	s, err := js.Stream(ctx, "LEADS")
	if err != nil {
		t.Fatalf("stream LEADS: %v", err)
	}
	return s
}

func sampleEnvelope(t *testing.T, ch envelope.Channel, id string) envelope.Envelope {
	t.Helper()
	idName := map[envelope.Channel]string{envelope.Voice: "CallSid", envelope.SMS: "MessageSid"}[ch]
	env, err := envelope.FromWebhook(ch, url.Values{
		idName: {id},
		"From": {"+14158675310"},
		"To":   {"+18005551212"},
	}, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromWebhook: %v", err)
	}
	return env
}

func TestEnsureStreamCreatesLeadsStream(t *testing.T) {
	ctx, js := setup(t)

	if err := EnsureStream(ctx, js); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	s, err := js.Stream(ctx, "LEADS")
	if err != nil {
		t.Fatalf("stream LEADS: %v", err)
	}
	cfg := s.CachedInfo().Config
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "leads.raw.>" {
		t.Errorf("subjects = %v, want [leads.raw.>]", cfg.Subjects)
	}
	if cfg.Storage != jetstream.FileStorage {
		t.Errorf("storage = %v, want file", cfg.Storage)
	}
	if cfg.MaxAge != 7*24*time.Hour {
		t.Errorf("max age = %v, want 168h", cfg.MaxAge)
	}
	if cfg.Duplicates != 10*time.Minute {
		t.Errorf("duplicate window = %v, want 10m", cfg.Duplicates)
	}
}

// Every service calls EnsureStream at startup, so an existing stream with an outdated
// config (e.g. from an earlier release) must be updated in place, not rejected.
func TestEnsureStreamUpdatesExistingStream(t *testing.T) {
	ctx, js := setup(t)
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:       "LEADS",
		Subjects:   []string{"leads.raw.>"},
		Storage:    jetstream.FileStorage,
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create old stream: %v", err)
	}

	if err := EnsureStream(ctx, js); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	info, err := leadsStream(ctx, t, js).Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.Duplicates != 10*time.Minute {
		t.Errorf("duplicate window = %v, want 10m", info.Config.Duplicates)
	}
}

func TestPublishStoresEventOnChannelSubject(t *testing.T) {
	ctx, js := setup(t)
	if err := EnsureStream(ctx, js); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	env := sampleEnvelope(t, envelope.SMS, "SM111")

	if err := NewPublisher(js).Publish(ctx, env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Publish returned, so the event must already be stored: no waiting or polling.
	s := leadsStream(ctx, t, js)
	msg, err := s.GetLastMsgForSubject(ctx, "leads.raw.sms")
	if err != nil {
		t.Fatalf("no message on leads.raw.sms: %v", err)
	}
	if got := msg.Header.Get(nats.MsgIdHdr); got != "SM111" {
		t.Errorf("Nats-Msg-Id = %q, want %q", got, "SM111")
	}
	got, err := envelope.Parse(msg.Data)
	if err != nil {
		t.Fatalf("stored payload does not parse: %v", err)
	}
	if got.ProviderID != "SM111" || got.Channel != envelope.SMS {
		t.Errorf("stored envelope = %+v", got)
	}
}

func TestPublishDeduplicatesByProviderID(t *testing.T) {
	ctx, js := setup(t)
	if err := EnsureStream(ctx, js); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	p := NewPublisher(js)
	env := sampleEnvelope(t, envelope.Voice, "CA222")

	for i := range 2 {
		if err := p.Publish(ctx, env); err != nil {
			t.Fatalf("Publish %d: %v", i+1, err)
		}
	}
	if err := p.Publish(ctx, sampleEnvelope(t, envelope.Voice, "CA333")); err != nil {
		t.Fatalf("Publish other: %v", err)
	}

	s := leadsStream(ctx, t, js)
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 2 {
		t.Errorf("stored messages = %d, want 2 (CA222 once, CA333 once)", info.State.Msgs)
	}
}

// With no stream to store it, a publish must fail so the ingest edge can return 503
// and the provider retries, rather than acknowledging an event that was never stored.
func TestPublishFailsWithoutStream(t *testing.T) {
	ctx, js := setup(t)

	err := NewPublisher(js).Publish(ctx, sampleEnvelope(t, envelope.Voice, "CA444"))
	if err == nil {
		t.Fatal("Publish succeeded with no stream, want error")
	}
	if !errors.Is(err, jetstream.ErrNoStreamResponse) {
		t.Errorf("err = %v, want it to wrap jetstream.ErrNoStreamResponse", err)
	}
}

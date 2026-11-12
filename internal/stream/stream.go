// Package stream stores lead events durably in the NATS JetStream stream LEADS.
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

const (
	// StreamName is the JetStream stream holding every raw lead event.
	StreamName = "LEADS"
	// subjectPrefix is followed by the channel, e.g. leads.raw.voice.
	subjectPrefix = "leads.raw."
)

// config is the LEADS stream's configuration; see docs/design.md, section 5.2.
var config = jetstream.StreamConfig{
	Name:     StreamName,
	Subjects: []string{subjectPrefix + ">"},
	Storage:  jetstream.FileStorage,
	// Keep a week of history so new consumers can replay it.
	MaxAge: 7 * 24 * time.Hour,
	// Drop a repeated provider ID seen within this window; the database catches older ones.
	Duplicates: 10 * time.Minute,
}

// EnsureStream creates the LEADS stream, or updates an existing one to the current config.
// Services call it at startup, so it is safe to run repeatedly.
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	if _, err := js.CreateOrUpdateStream(ctx, config); err != nil {
		return fmt.Errorf("ensure stream %s: %w", StreamName, err)
	}
	return nil
}

// HeaderSchemaVersion carries envelope.SchemaRelease on every published event.
const HeaderSchemaVersion = "Schema-Version"

// Subject returns the subject events for a channel are published on.
func Subject(ch envelope.Channel) string {
	return subjectPrefix + string(ch)
}

// Publisher publishes lead events to the LEADS stream.
type Publisher struct {
	js jetstream.JetStream
}

func NewPublisher(js jetstream.JetStream) *Publisher {
	return &Publisher{js: js}
}

// Publish stores env and returns only once JetStream has acknowledged it, so a nil error
// means the event is durable. The provider ID is the message ID, so a retried webhook
// within the duplicate window is stored once.
func (p *Publisher) Publish(ctx context.Context, env envelope.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	msg := nats.NewMsg(Subject(env.Channel))
	msg.Data = data
	msg.Header.Set(HeaderSchemaVersion, envelope.SchemaRelease)
	if _, err := p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(env.ProviderID)); err != nil {
		return fmt.Errorf("publish %s: %w", env.ProviderID, err)
	}
	return nil
}

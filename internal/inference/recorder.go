package inference

import (
	"context"
	"fmt"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// insightStore is the part of Store the recorder needs; tests replace it.
type insightStore interface {
	Exists(ctx context.Context, providerID string) (bool, error)
	Save(ctx context.Context, in Insight) (bool, error)
}

// Recorder classifies each lead event with the model and stores the result. It is what the
// inference consumer calls for every message.
type Recorder struct {
	model Model
	store insightStore
	now   func() time.Time // replaced in tests
}

func NewRecorder(m Model, s *Store) *Recorder {
	return &Recorder{model: m, store: s, now: time.Now}
}

// Record reports false, without calling the model, if env already has an insight: redeliveries
// mustn't spend model capacity. Any error means the message should be retried later.
func (r *Recorder) Record(ctx context.Context, env envelope.Envelope) (bool, error) {
	exists, err := r.store.Exists(ctx, env.ProviderID)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	start := r.now()
	result, err := r.model.Classify(ctx, Input{Channel: env.Channel, Body: env.Params.Get("Body")})
	latency := r.now().Sub(start) // model latency: the model call alone, not queue time
	if err != nil {
		return false, fmt.Errorf("classify %s: %w", env.ProviderID, err)
	}

	return r.store.Save(ctx, Insight{
		ProviderID:   env.ProviderID,
		ReceivedAt:   env.ReceivedAt,
		Result:       result,
		ModelLatency: latency,
	})
}

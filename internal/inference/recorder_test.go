package inference

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
)

type fakeModel struct {
	inputs []Input
	err    error
}

func (m *fakeModel) Classify(_ context.Context, in Input) (Result, error) {
	m.inputs = append(m.inputs, in)
	if m.err != nil {
		return Result{}, m.err
	}
	return Result{Intent: "sales", Score: 0.9, ModelVersion: "fake"}, nil
}

type fakeStore struct {
	existing  bool
	existsErr error
	saveErr   error
	saved     []Insight
}

func (s *fakeStore) Exists(context.Context, string) (bool, error) { return s.existing, s.existsErr }

func (s *fakeStore) Save(_ context.Context, in Insight) (bool, error) {
	if s.saveErr != nil {
		return false, s.saveErr
	}
	s.saved = append(s.saved, in)
	return true, nil
}

var receivedAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func smsEvent(t *testing.T) envelope.Envelope {
	t.Helper()
	env, err := envelope.FromWebhook(envelope.SMS, url.Values{
		"MessageSid": {"SM1"}, "From": {"+14158675310"}, "To": {"+18005550001"}, "Body": {"what's the price?"},
	}, receivedAt)
	if err != nil {
		t.Fatalf("FromWebhook: %v", err)
	}
	return env
}

// newTestRecorder's clock advances 150ms on every reading, so one model call measures 150ms.
func newTestRecorder(m Model, s insightStore) *Recorder {
	r := &Recorder{model: m, store: s}
	now := time.Date(2026, 10, 28, 13, 0, 0, 0, time.UTC)
	r.now = func() time.Time {
		now = now.Add(150 * time.Millisecond)
		return now
	}
	return r
}

func TestRecordClassifiesAndSaves(t *testing.T) {
	model, store := &fakeModel{}, &fakeStore{}

	saved, err := newTestRecorder(model, store).Record(context.Background(), smsEvent(t))
	if err != nil || !saved {
		t.Fatalf("Record = %v, %v; want true, nil", saved, err)
	}
	if len(model.inputs) != 1 || model.inputs[0] != (Input{Channel: envelope.SMS, Body: "what's the price?"}) {
		t.Errorf("model inputs = %+v, want the SMS channel and body", model.inputs)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d insights, want 1", len(store.saved))
	}
	got := store.saved[0]
	if got.ProviderID != "SM1" || !got.ReceivedAt.Equal(receivedAt) || got.Result.Intent != "sales" ||
		got.ModelLatency != 150*time.Millisecond {
		t.Errorf("saved %+v, want SM1 received at %v, sales, 150ms model latency", got, receivedAt)
	}
}

// A redelivered message must not spend model capacity.
func TestRecordSkipsExistingInsightWithoutCallingTheModel(t *testing.T) {
	model, store := &fakeModel{}, &fakeStore{existing: true}

	saved, err := newTestRecorder(model, store).Record(context.Background(), smsEvent(t))
	if err != nil || saved {
		t.Errorf("Record = %v, %v; want false, nil", saved, err)
	}
	if len(model.inputs) != 0 {
		t.Errorf("model called %d times, want 0", len(model.inputs))
	}
}

// Errors are returned so the consumer retries the message later.
func TestRecordReturnsErrors(t *testing.T) {
	dbDown := errors.New("database unavailable")
	tests := []struct {
		name       string
		model      *fakeModel
		store      *fakeStore
		want       error
		modelCalls int
	}{
		{"model overloaded", &fakeModel{err: ErrOverloaded}, &fakeStore{}, ErrOverloaded, 1},
		{"exists check fails", &fakeModel{}, &fakeStore{existsErr: dbDown}, dbDown, 0},
		{"save fails", &fakeModel{}, &fakeStore{saveErr: dbDown}, dbDown, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestRecorder(tt.model, tt.store).Record(context.Background(), smsEvent(t))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if len(tt.model.inputs) != tt.modelCalls {
				t.Errorf("model called %d times, want %d", len(tt.model.inputs), tt.modelCalls)
			}
			if len(tt.store.saved) != 0 {
				t.Errorf("saved %d insights, want 0", len(tt.store.saved))
			}
		})
	}
}

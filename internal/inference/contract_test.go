//go:build integration

package inference

import (
	"context"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/contract"
	"github.com/cferrante/call-event-pipeline/internal/dbtest"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// Consumer side of the contract: inference classifies and stores every published example
// of every minor version it supports, including events with fields it doesn't know.
func TestInferenceRecordsEveryV1Example(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := dbtest.Migrated(t)
	model := NewStub(StubConfig{Capacity: 4, BaseLatency: time.Millisecond, Timeout: time.Second})
	rec := NewRecorder(model, NewStore(db))

	for _, ex := range contract.Examples(t, "lead-event", 1) {
		env, err := envelope.Parse(ex.Data)
		if err != nil {
			t.Errorf("%s: parse: %v", ex.Path, err)
			continue
		}
		if _, err := rec.Record(ctx, env); err != nil {
			t.Errorf("%s: record: %v", ex.Path, err)
			continue
		}
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM insights WHERE provider_id = $1`, env.ProviderID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("%s: %d insights stored, want 1", ex.Path, n)
		}
	}
}

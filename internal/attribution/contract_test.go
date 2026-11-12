//go:build integration

package attribution

import (
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/contract"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// Consumer side of the contract: attribution records every published example of every
// minor version it supports, including events with fields it doesn't know.
func TestAttributionRecordsEveryV1Example(t *testing.T) {
	db, store := setup(t)
	for _, ex := range contract.Examples(t, "lead-event", 1) {
		env, err := envelope.Parse(ex.Data)
		if err != nil {
			t.Errorf("%s: parse: %v", ex.Path, err)
			continue
		}
		if _, err := store.Record(ctx(t), env); err != nil {
			t.Errorf("%s: record: %v", ex.Path, err)
			continue
		}
		var n int
		if err := db.QueryRow(ctx(t), `SELECT count(*) FROM interactions WHERE provider_id = $1`, env.ProviderID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("%s: %d interactions stored, want 1", ex.Path, n)
		}
	}
}

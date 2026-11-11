//go:build integration

package inference

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/call-event-pipeline/internal/dbtest"
)

var received = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func sample(id string) Insight {
	return Insight{
		ProviderID:   id,
		ReceivedAt:   received,
		Result:       Result{Intent: "sales", Score: 0.9, ModelVersion: "stub-v1"},
		ModelLatency: 213456 * time.Microsecond,
	}
}

type insightRow struct {
	Intent, ModelVersion string
	Score                float64
	ModelMS              float64
	ReceivedAt           time.Time
	Version              string // xmin: changes on every write
}

func row(t *testing.T, db *pgxpool.Pool, id string) insightRow {
	t.Helper()
	var r insightRow
	err := db.QueryRow(ctx(t), `
		SELECT intent, model_version, score, model_ms, received_at, xmin::text
		FROM insights WHERE provider_id = $1`, id).
		Scan(&r.Intent, &r.ModelVersion, &r.Score, &r.ModelMS, &r.ReceivedAt, &r.Version)
	if err != nil {
		t.Fatalf("insight %s: %v", id, err)
	}
	return r
}

func save(t *testing.T, s *Store, in Insight) bool {
	t.Helper()
	saved, err := s.Save(ctx(t), in)
	if err != nil {
		t.Fatalf("Save %s: %v", in.ProviderID, err)
	}
	return saved
}

// Model latency keeps sub-millisecond precision: at the load test's 10 ms, whole milliseconds
// would make 1 ms of rounding as large as the 10% "flat" tolerance.
func TestSaveStoresTheInsight(t *testing.T) {
	db := dbtest.Migrated(t)
	s := NewStore(db)

	if !save(t, s, sample("CA1")) {
		t.Fatal("Save = false for a new insight, want true")
	}

	r := row(t, db, "CA1")
	if r.Intent != "sales" || r.ModelVersion != "stub-v1" || r.Score < 0.89 || r.Score > 0.91 ||
		r.ModelMS < 213.455 || r.ModelMS > 213.457 || !r.ReceivedAt.Equal(received) {
		t.Errorf("row = %+v", r)
	}
}

// A redelivered message must not change the stored insight at all.
func TestSaveTwiceChangesNothing(t *testing.T) {
	db := dbtest.Migrated(t)
	s := NewStore(db)
	save(t, s, sample("CA1"))
	before := row(t, db, "CA1")

	again := sample("CA1")
	again.Result.Intent = "support"
	if save(t, s, again) {
		t.Error("Save = true for an existing insight, want false")
	}
	if after := row(t, db, "CA1"); after != before {
		t.Errorf("insight changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestConcurrentSavesApplyOnce(t *testing.T) {
	db := dbtest.Migrated(t)
	s := NewStore(db)

	const workers = 8
	var wg sync.WaitGroup
	saved := make([]bool, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() { saved[i], errs[i] = s.Save(ctx(t), sample("CA1")) })
	}
	wg.Wait()

	n := 0
	for i := range workers {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
		}
		if saved[i] {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d workers saved the insight, want exactly 1", n)
	}
}

// Workers check Exists before calling the model, so redeliveries don't spend model capacity.
func TestExists(t *testing.T) {
	db := dbtest.Migrated(t)
	s := NewStore(db)

	for _, tt := range []struct {
		when string
		want bool
	}{{"before saving", false}, {"after saving", true}} {
		if tt.want {
			save(t, s, sample("CA1"))
		}
		got, err := s.Exists(ctx(t), "CA1")
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if got != tt.want {
			t.Errorf("Exists %s = %v, want %v", tt.when, got, tt.want)
		}
	}
}

// The schema rejects values the model should never produce.
func TestSaveRejectsInvalidResults(t *testing.T) {
	db := dbtest.Migrated(t)
	s := NewStore(db)

	for name, mutate := range map[string]func(*Insight){
		"unknown intent":   func(in *Insight) { in.Result.Intent = "fax" },
		"score above 1":    func(in *Insight) { in.Result.Score = 1.5 },
		"negative score":   func(in *Insight) { in.Result.Score = -0.1 },
		"negative latency": func(in *Insight) { in.ModelLatency = -time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			in := sample("CA-" + name)
			mutate(&in)

			_, err := s.Save(ctx(t), in)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("err = %v, want check violation 23514", err)
			}
		})
	}
}

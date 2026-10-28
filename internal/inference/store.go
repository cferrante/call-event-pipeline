package inference

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Insight is one stored model result.
type Insight struct {
	ProviderID   string
	ReceivedAt   time.Time // when the webhook arrived, for time to insight
	Result       Result
	ModelLatency time.Duration
}

// Store writes insights to Postgres.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Save stores in, reporting false (and changing nothing) if an insight for its provider ID
// already exists, so a redelivered message is a no-op. See docs/design.md, section 5.5.
func (s *Store) Save(ctx context.Context, in Insight) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO insights (provider_id, intent, score, model_version, model_ms, received_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider_id) DO NOTHING`,
		in.ProviderID, in.Result.Intent, in.Result.Score, in.Result.ModelVersion,
		float64(in.ModelLatency)/float64(time.Millisecond), in.ReceivedAt)
	if err != nil {
		return false, fmt.Errorf("save insight %s: %w", in.ProviderID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Exists reports whether an insight is already stored for providerID. Workers check it before
// calling the model, so redeliveries don't spend model capacity; Save still guards the race.
func (s *Store) Exists(ctx context.Context, providerID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM insights WHERE provider_id = $1)", providerID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check insight %s: %w", providerID, err)
	}
	return exists, nil
}

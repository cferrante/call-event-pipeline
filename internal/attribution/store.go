// Package attribution records inbound interactions and credits them to campaigns.
package attribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// Store writes interactions and leads to Postgres.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// insertInteraction is the idempotency gate (docs/design.md, section 5.5): it inserts the
// interaction with the tracking number's current campaign, or returns no row if this
// provider_id was already recorded. Unknown and inactive numbers are credited to 'unknown'.
const insertInteraction = `
INSERT INTO interactions (provider_id, channel, from_number, to_e164, campaign_id, source, occurred_at, raw)
SELECT $1, $2, $3, $4, tn.campaign_id, coalesce(tn.source, 'unknown'), $5, $6
FROM (SELECT) AS one
LEFT JOIN tracking_numbers tn ON tn.phone_e164 = $4 AND tn.active
ON CONFLICT (provider_id) DO NOTHING
RETURNING id, campaign_id, source`

// upsertLead creates the caller's lead or moves its first/last touch by occurred_at, not
// by processing order, so out-of-order processing gives the same result. SET expressions
// see the row's old values, so each CASE compares against the existing touch.
const upsertLead = `
INSERT INTO leads AS l (phone_e164, first_campaign_id, first_source, first_seen_at,
                        last_campaign_id, last_source, last_seen_at)
VALUES ($1, $2, $3, $4, $2, $3, $4)
ON CONFLICT (phone_e164) DO UPDATE SET
    first_campaign_id = CASE WHEN excluded.first_seen_at < l.first_seen_at
                             THEN excluded.first_campaign_id ELSE l.first_campaign_id END,
    first_source      = CASE WHEN excluded.first_seen_at < l.first_seen_at
                             THEN excluded.first_source ELSE l.first_source END,
    first_seen_at     = least(l.first_seen_at, excluded.first_seen_at),
    last_campaign_id  = CASE WHEN excluded.last_seen_at > l.last_seen_at
                             THEN excluded.last_campaign_id ELSE l.last_campaign_id END,
    last_source       = CASE WHEN excluded.last_seen_at > l.last_seen_at
                             THEN excluded.last_source ELSE l.last_source END,
    last_seen_at      = greatest(l.last_seen_at, excluded.last_seen_at)
RETURNING id`

// Record stores env as an interaction and updates its caller's lead, in one transaction.
// It reports false, having changed nothing, if env was already recorded: the interaction
// insert runs first so a redelivered message is a complete no-op.
func (s *Store) Record(ctx context.Context, env envelope.Envelope) (bool, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return false, fmt.Errorf("encode envelope: %w", err)
	}

	recorded := false
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var (
			interactionID int64
			campaignID    *string
			source        string
		)
		err := tx.QueryRow(ctx, insertInteraction,
			env.ProviderID, env.Channel, env.From, env.To, env.ReceivedAt, raw,
		).Scan(&interactionID, &campaignID, &source)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded
		}
		if err != nil {
			return fmt.Errorf("insert interaction: %w", err)
		}
		recorded = true

		if env.AnonymousCaller() {
			return nil // no way to identify the lead; the campaign is still credited
		}
		var leadID int64
		if err := tx.QueryRow(ctx, upsertLead,
			env.From, campaignID, source, env.ReceivedAt,
		).Scan(&leadID); err != nil {
			return fmt.Errorf("upsert lead: %w", err)
		}
		if _, err := tx.Exec(ctx,
			"UPDATE interactions SET lead_id = $1 WHERE id = $2", leadID, interactionID,
		); err != nil {
			return fmt.Errorf("link interaction to lead: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record %s: %w", env.ProviderID, err)
	}
	return recorded, nil
}

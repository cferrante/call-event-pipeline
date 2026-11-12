//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/call-event-pipeline/internal/dbtest"
)

// Postgres error codes; see https://www.postgresql.org/docs/current/errcodes-appendix.html.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
	notNullViolation    = "23502"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func insertLead(t *testing.T, db *pgxpool.Pool, phone string) (int64, error) {
	t.Helper()
	var id int64
	err := db.QueryRow(ctx(t), `
		INSERT INTO leads (phone_e164, first_source, first_seen_at, last_source, last_seen_at)
		VALUES ($1, 'google', now(), 'google', now()) RETURNING id`, phone).Scan(&id)
	return id, err
}

func insertInteraction(t *testing.T, db *pgxpool.Pool, providerID string, leadID *int64, channel string) error {
	t.Helper()
	_, err := db.Exec(ctx(t), `
		INSERT INTO interactions (provider_id, lead_id, channel, from_number, to_e164, source, occurred_at, raw)
		VALUES ($1, $2, $3, '+14158675310', '+18005551212', 'unknown', now(), '{}')`,
		providerID, leadID, channel)
	return err
}

func insertEmail(t *testing.T, db *pgxpool.Pool, providerID, toAddress string) error {
	t.Helper()
	_, err := db.Exec(ctx(t), `
		INSERT INTO interactions (provider_id, channel, from_number, to_address, source, occurred_at, raw)
		VALUES ($1, 'email', 'jordan@example.net', $2, 'unknown', now(), '{}')`,
		providerID, toAddress)
	return err
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("err = %v, want Postgres error %s", err, code)
	}
}

func TestInteractionProviderIDIsUnique(t *testing.T) {
	db := dbtest.Migrated(t)
	if err := insertInteraction(t, db, "CA1", nil, "voice"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	wantCode(t, insertInteraction(t, db, "CA1", nil, "voice"), uniqueViolation)
}

// Anonymous callers have no lead, and unknown tracking numbers have no campaign.
func TestInteractionAllowsNoLeadAndNoCampaign(t *testing.T) {
	db := dbtest.Migrated(t)

	if err := insertInteraction(t, db, "CA1", nil, "voice"); err != nil {
		t.Errorf("insert with NULL lead_id and campaign_id: %v", err)
	}
}

func TestInteractionLeadMustExist(t *testing.T) {
	db := dbtest.Migrated(t)
	missing := int64(999)

	wantCode(t, insertInteraction(t, db, "CA1", &missing, "voice"), foreignKeyViolation)
}

func TestInteractionChannelIsVoiceSMSOrEmail(t *testing.T) {
	db := dbtest.Migrated(t)

	if err := insertInteraction(t, db, "SM1", nil, "sms"); err != nil {
		t.Errorf("sms: %v", err)
	}
	if err := insertEmail(t, db, "<m1@example.net>", "search-fall@leads.example.com"); err != nil {
		t.Errorf("email: %v", err)
	}
	wantCode(t, insertInteraction(t, db, "FX1", nil, "fax"), checkViolation)
}

// An interaction was sent to a tracking number or a tracking address, never both or neither.
func TestInteractionHasExactlyOneDestination(t *testing.T) {
	db := dbtest.Migrated(t)
	_, err := db.Exec(ctx(t), `
		INSERT INTO interactions (provider_id, channel, from_number, to_e164, to_address, source, occurred_at, raw)
		VALUES ('<m2@example.net>', 'email', 'a@example.net', '+18005551212', 'x@leads.example.com', 'unknown', now(), '{}')`)
	wantCode(t, err, checkViolation)
	_, err = db.Exec(ctx(t), `
		INSERT INTO interactions (provider_id, channel, from_number, source, occurred_at, raw)
		VALUES ('CA9', 'voice', '+14158675310', 'unknown', now(), '{}')`)
	wantCode(t, err, checkViolation)
}

// Addresses are stored lowercased so a lookup can't miss on case.
func TestTrackingAddressIsLowercase(t *testing.T) {
	db := dbtest.Migrated(t)
	ins := `INSERT INTO tracking_addresses (address, campaign_id, source) VALUES ($1, 'search-fall', 'google')`
	if _, err := db.Exec(ctx(t), ins, "search-fall@leads.example.com"); err != nil {
		t.Fatalf("lowercase address: %v", err)
	}
	_, err := db.Exec(ctx(t), ins, "Radio@Leads.Example.com")
	wantCode(t, err, checkViolation)
}

func TestLeadPhoneIsUnique(t *testing.T) {
	db := dbtest.Migrated(t)
	if _, err := insertLead(t, db, "+14158675310"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := insertLead(t, db, "+14158675310")
	wantCode(t, err, uniqueViolation)
}

func TestTrackingNumberRequiresCampaignAndSource(t *testing.T) {
	db := dbtest.Migrated(t)

	_, err := db.Exec(ctx(t), `INSERT INTO tracking_numbers (phone_e164, campaign_id, source)
		VALUES ('+18005551212', 'fall-promo', 'google')`)
	if err != nil {
		t.Errorf("valid tracking number: %v", err)
	}
	_, err = db.Exec(ctx(t), `INSERT INTO tracking_numbers (phone_e164, source) VALUES ('+18005550000', 'google')`)
	wantCode(t, err, notNullViolation)
}

// Postgres doesn't index foreign keys automatically; without this, finding a lead's
// interactions (or deleting a lead) scans the whole table.
func TestInteractionLeadIDIsIndexed(t *testing.T) {
	db := dbtest.Migrated(t)

	var n int
	err := db.QueryRow(ctx(t), `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'interactions' AND indexdef LIKE '%(lead_id)%'`).Scan(&n)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	if n == 0 {
		t.Error("no index on interactions(lead_id)")
	}
}

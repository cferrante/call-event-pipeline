//go:build integration

package migrations_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/decoupled-ingestion-layer/internal/dbtest"
)

const (
	caller   = "+14158675310"
	googleNo = "+18005550001"
	radioNo  = "+18005550002"
)

var day0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

var conversationSeq int

// message inserts an interaction directly; leadID nil means an anonymous caller.
func message(t *testing.T, db *pgxpool.Pool, from, to string, campaign *string, source string, leadID *int64, at time.Time) {
	t.Helper()
	conversationSeq++
	_, err := db.Exec(ctx(t), `
		INSERT INTO interactions (provider_id, lead_id, channel, from_number, to_e164, campaign_id, source, occurred_at, raw)
		VALUES ($1, $2, 'sms', $3, $4, $5, $6, $7, '{}')`,
		fmt.Sprintf("SM%06d", conversationSeq), leadID, from, to, campaign, source, at)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

type campaignRow struct {
	conversations, interactions int
}

// report reads campaign_conversations, keyed by source.
func report(t *testing.T, db *pgxpool.Pool) map[string]campaignRow {
	t.Helper()
	rows, err := db.Query(ctx(t), "SELECT source, conversations, interactions FROM campaign_conversations")
	if err != nil {
		t.Fatalf("query view: %v", err)
	}
	defer rows.Close()
	out := map[string]campaignRow{}
	for rows.Next() {
		var source string
		var r campaignRow
		if err := rows.Scan(&source, &r.conversations, &r.interactions); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[source] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func setupLead(t *testing.T) (*pgxpool.Pool, *int64, *string) {
	t.Helper()
	db := dbtest.Migrated(t)
	id, err := insertLead(t, db, caller)
	if err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	campaign := "search-fall"
	return db, &id, &campaign
}

func wantReport(t *testing.T, got map[string]campaignRow, source string, conversations, interactions int) {
	t.Helper()
	if r := got[source]; r.conversations != conversations || r.interactions != interactions {
		t.Errorf("%s: conversations, interactions = %d, %d; want %d, %d",
			source, r.conversations, r.interactions, conversations, interactions)
	}
}

func TestMessagesWithin24HoursAreOneConversation(t *testing.T) {
	db, lead, campaign := setupLead(t)
	for _, h := range []int{0, 1, 23} {
		message(t, db, caller, googleNo, campaign, "google", lead, day0.Add(time.Duration(h)*time.Hour))
	}

	wantReport(t, report(t, db), "google", 1, 3)
}

func TestGapOver24HoursStartsANewConversation(t *testing.T) {
	db, lead, campaign := setupLead(t)
	message(t, db, caller, googleNo, campaign, "google", lead, day0)
	message(t, db, caller, googleNo, campaign, "google", lead, day0.Add(25*time.Hour))

	wantReport(t, report(t, db), "google", 2, 2)
}

func TestExactly24HoursApartIsTheSameConversation(t *testing.T) {
	db, lead, campaign := setupLead(t)
	message(t, db, caller, googleNo, campaign, "google", lead, day0)
	message(t, db, caller, googleNo, campaign, "google", lead, day0.Add(24*time.Hour))

	wantReport(t, report(t, db), "google", 1, 2)
}

// The gap is measured in time order, whatever order the rows were inserted in.
func TestConversationsFollowTimeNotInsertOrder(t *testing.T) {
	db, lead, campaign := setupLead(t)
	for _, h := range []int{30, 0, 2} { // in time order: 0, 2 (same conversation), 30 (new one)
		message(t, db, caller, googleNo, campaign, "google", lead, day0.Add(time.Duration(h)*time.Hour))
	}

	wantReport(t, report(t, db), "google", 2, 3)
}

func TestEachTrackingNumberIsItsOwnConversation(t *testing.T) {
	db, lead, campaign := setupLead(t)
	radio := "morning-show"
	message(t, db, caller, googleNo, campaign, "google", lead, day0)
	message(t, db, caller, radioNo, &radio, "radio", lead, day0.Add(time.Hour))
	message(t, db, caller, googleNo, campaign, "google", lead, day0.Add(2*time.Hour))

	got := report(t, db)
	wantReport(t, got, "google", 1, 2)
	wantReport(t, got, "radio", 1, 1)
}

// A tracking number can move to another campaign mid-conversation. Each campaign then gets
// its own conversation, so no campaign shows interactions without a conversation.
func TestCampaignChangeMidConversationCountsForEachCampaign(t *testing.T) {
	db, lead, campaign := setupLead(t)
	spring := "spring-flyer"
	message(t, db, caller, googleNo, campaign, "google", lead, day0)
	message(t, db, caller, googleNo, &spring, "print", lead, day0.Add(time.Hour))

	got := report(t, db)
	wantReport(t, got, "google", 1, 1)
	wantReport(t, got, "print", 1, 1)
}

// Anonymous callers can't be told apart, so each of their messages counts on its own.
func TestAnonymousMessagesAreSeparateConversations(t *testing.T) {
	db, _, campaign := setupLead(t)
	for _, m := range []int{0, 5, 10} {
		message(t, db, "anonymous", googleNo, campaign, "google", nil, day0.Add(time.Duration(m)*time.Minute))
	}

	wantReport(t, report(t, db), "google", 3, 3)
}

func TestUnknownTrackingNumberIsReported(t *testing.T) {
	db, lead, _ := setupLead(t)
	message(t, db, caller, "+18005559999", nil, "unknown", lead, day0)

	wantReport(t, report(t, db), "unknown", 1, 1)
}

func TestConversationColumnsAreIndexed(t *testing.T) {
	db := dbtest.Migrated(t)

	var n int
	err := db.QueryRow(ctx(t), `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'interactions'
		  AND indexdef LIKE '%(from_number, to_e164, campaign_id, occurred_at)%'`).Scan(&n)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	if n == 0 {
		t.Error("no index on interactions(from_number, to_e164, campaign_id, occurred_at)")
	}
}

//go:build integration

package attribution

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/call-event-pipeline/internal/dbtest"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

const (
	caller    = "+14158675310"
	googleNum = "+18005550001"
	radioNum  = "+18005550002"
	tvNum     = "+18005550003"
	unusedNum = "+18005559999"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// setup returns a migrated database with three tracking numbers, one of them inactive.
func setup(t *testing.T) (*pgxpool.Pool, *Store) {
	t.Helper()
	db := dbtest.Migrated(t)
	_, err := db.Exec(ctx(t), `
		INSERT INTO tracking_numbers (phone_e164, campaign_id, source, active) VALUES
			($1, 'search-fall', 'google', true),
			($2, 'morning-show', 'radio', true),
			($3, 'old-tv-spot', 'tv', false)`,
		googleNum, radioNum, tvNum)
	if err != nil {
		t.Fatalf("seed tracking numbers: %v", err)
	}
	return db, NewStore(db)
}

func call(t *testing.T, id, from, to string, at time.Time) envelope.Envelope {
	t.Helper()
	env, err := envelope.FromWebhook(envelope.Voice, url.Values{
		"CallSid": {id}, "From": {from}, "To": {to},
	}, at)
	if err != nil {
		t.Fatalf("FromWebhook: %v", err)
	}
	return env
}

func record(t *testing.T, s *Store, env envelope.Envelope) bool {
	t.Helper()
	recorded, err := s.Record(ctx(t), env)
	if err != nil {
		t.Fatalf("Record %s: %v", env.ProviderID, err)
	}
	return recorded
}

type interactionRow struct {
	LeadID     *int64
	CampaignID *string
	Source     string
	RawID      string
}

func interaction(t *testing.T, db *pgxpool.Pool, providerID string) interactionRow {
	t.Helper()
	var r interactionRow
	err := db.QueryRow(ctx(t), `
		SELECT lead_id, campaign_id, source, raw->>'provider_id'
		FROM interactions WHERE provider_id = $1`, providerID).
		Scan(&r.LeadID, &r.CampaignID, &r.Source, &r.RawID)
	if err != nil {
		t.Fatalf("interaction %s: %v", providerID, err)
	}
	return r
}

type leadRow struct {
	ID                          int64
	FirstCampaign, LastCampaign string // "<NULL>" when NULL, so rows compare by value
	FirstSource, LastSource     string
	FirstSeen, LastSeen         time.Time
	Version                     string // xmin: changes on every write, even identical values
}

func lead(t *testing.T, db *pgxpool.Pool, phone string) leadRow {
	t.Helper()
	var (
		r           leadRow
		first, last *string
	)
	err := db.QueryRow(ctx(t), `
		SELECT id, first_campaign_id, first_source, first_seen_at,
		       last_campaign_id, last_source, last_seen_at, xmin::text
		FROM leads WHERE phone_e164 = $1`, phone).
		Scan(&r.ID, &first, &r.FirstSource, &r.FirstSeen,
			&last, &r.LastSource, &r.LastSeen, &r.Version)
	if err != nil {
		t.Fatalf("lead %s: %v", phone, err)
	}
	r.FirstCampaign, r.LastCampaign = str(first), str(last)
	return r
}

func count(t *testing.T, db *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctx(t), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func str(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func TestRecordAttributesKnownTrackingNumber(t *testing.T) {
	db, s := setup(t)

	if !record(t, s, call(t, "CA1", caller, googleNum, t0)) {
		t.Fatal("Record = false for a new interaction, want true")
	}

	i := interaction(t, db, "CA1")
	if str(i.CampaignID) != "search-fall" || i.Source != "google" {
		t.Errorf("interaction campaign, source = %s, %s; want search-fall, google", str(i.CampaignID), i.Source)
	}
	if i.RawID != "CA1" {
		t.Errorf("raw envelope provider_id = %q, want CA1", i.RawID)
	}
	l := lead(t, db, caller)
	if i.LeadID == nil || *i.LeadID != l.ID {
		t.Errorf("interaction lead_id = %v, want %d", i.LeadID, l.ID)
	}
	if l.FirstCampaign != "search-fall" || l.LastCampaign != "search-fall" ||
		!l.FirstSeen.Equal(t0) || !l.LastSeen.Equal(t0) {
		t.Errorf("lead = %+v, want first and last touch search-fall at %v", l, t0)
	}
}

func TestRecordUnknownAndInactiveNumbersAreUnknown(t *testing.T) {
	for name, to := range map[string]string{"unknown number": unusedNum, "inactive number": tvNum} {
		t.Run(name, func(t *testing.T) {
			db, s := setup(t)

			record(t, s, call(t, "CA1", caller, to, t0))

			i := interaction(t, db, "CA1")
			if i.CampaignID != nil || i.Source != "unknown" {
				t.Errorf("interaction campaign, source = %s, %s; want <NULL>, unknown", str(i.CampaignID), i.Source)
			}
			if l := lead(t, db, caller); l.FirstSource != "unknown" || l.FirstCampaign != "<NULL>" {
				t.Errorf("lead first touch = %s, %s; want <NULL>, unknown", l.FirstCampaign, l.FirstSource)
			}
		})
	}
}

// See docs/design.md, section 5.5: a redelivered message must change nothing at all.
func TestRecordRedeliveryChangesNothing(t *testing.T) {
	db, s := setup(t)
	env := call(t, "CA1", caller, googleNum, t0)
	record(t, s, env)
	before := lead(t, db, caller)

	if record(t, s, env) {
		t.Error("Record = true for a redelivery, want false")
	}

	if n := count(t, db, "interactions"); n != 1 {
		t.Errorf("interactions = %d, want 1", n)
	}
	if after := lead(t, db, caller); after != before {
		t.Errorf("lead changed on redelivery:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Processing order differs from call order; first and last touch must follow call order.
func TestRecordOutOfOrderKeepsFirstAndLastTouchByTime(t *testing.T) {
	db, s := setup(t)
	first := call(t, "CA1", caller, googleNum, t0)
	middle := call(t, "CA2", caller, unusedNum, t0.Add(time.Hour))
	last := call(t, "CA3", caller, radioNum, t0.Add(2*time.Hour))

	// The last one processed is neither the earliest nor the latest call, so any logic that
	// follows processing order gets first touch and last touch wrong.
	for _, env := range []envelope.Envelope{last, first, middle} {
		record(t, s, env)
	}

	l := lead(t, db, caller)
	if l.FirstCampaign != "search-fall" || l.FirstSource != "google" || !l.FirstSeen.Equal(first.ReceivedAt) {
		t.Errorf("first touch = %s/%s at %v, want search-fall/google at %v",
			l.FirstCampaign, l.FirstSource, l.FirstSeen, first.ReceivedAt)
	}
	if l.LastCampaign != "morning-show" || l.LastSource != "radio" || !l.LastSeen.Equal(last.ReceivedAt) {
		t.Errorf("last touch = %s/%s at %v, want morning-show/radio at %v",
			l.LastCampaign, l.LastSource, l.LastSeen, last.ReceivedAt)
	}
	if n := count(t, db, "leads"); n != 1 {
		t.Errorf("leads = %d, want 1", n)
	}
}

func TestRecordAnonymousCallerHasNoLead(t *testing.T) {
	db, s := setup(t)

	if !record(t, s, call(t, "CA1", "anonymous", googleNum, t0)) {
		t.Fatal("Record = false, want true")
	}

	i := interaction(t, db, "CA1")
	if i.LeadID != nil {
		t.Errorf("lead_id = %d, want NULL", *i.LeadID)
	}
	if str(i.CampaignID) != "search-fall" {
		t.Errorf("campaign = %s, want search-fall: anonymous calls still credit the campaign", str(i.CampaignID))
	}
	if n := count(t, db, "leads"); n != 0 {
		t.Errorf("leads = %d, want 0", n)
	}
}

// Slow processing can make JetStream redeliver while the first worker is still running.
func TestRecordConcurrentDuplicatesApplyOnce(t *testing.T) {
	db, s := setup(t)
	env := call(t, "CA1", caller, googleNum, t0)

	const workers = 8
	var wg sync.WaitGroup
	results := make([]bool, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() { results[i], errs[i] = s.Record(ctx(t), env) })
	}
	wg.Wait()

	recorded := 0
	for i := range workers {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
		}
		if results[i] {
			recorded++
		}
	}
	if recorded != 1 {
		t.Errorf("%d workers recorded the interaction, want exactly 1", recorded)
	}
	if n := count(t, db, "interactions"); n != 1 {
		t.Errorf("interactions = %d, want 1", n)
	}
}

func TestRecordStoresTheWholeEnvelope(t *testing.T) {
	db, s := setup(t)
	env := call(t, "CA1", caller, googleNum, t0)
	record(t, s, env)

	var raw []byte
	if err := db.QueryRow(ctx(t), "SELECT raw FROM interactions WHERE provider_id = 'CA1'").Scan(&raw); err != nil {
		t.Fatalf("select raw: %v", err)
	}
	got, err := envelope.Parse(raw)
	if err != nil {
		t.Fatalf("stored raw does not parse: %v", err)
	}
	want, _ := json.Marshal(env)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(want) {
		t.Errorf("stored envelope = %s, want %s", gotJSON, want)
	}
}

func email(t *testing.T, id, from, to string, at time.Time) envelope.Envelope {
	t.Helper()
	return envelope.Envelope{
		SchemaVersion: envelope.SchemaVersion, Channel: envelope.Email, Provider: "sendgrid",
		ProviderID: id, From: from, To: to, ReceivedAt: at, Subject: "Quote?",
		Params: url.Values{"subject": {"Quote?"}, "text": {"price please"}},
	}
}

// Email is credited through tracking addresses, matched regardless of case. Leads are keyed
// by phone number, so an email sender gets no lead (docs/design.md, section 5.3).
func TestRecordEmailAttributesTrackingAddress(t *testing.T) {
	db, store := setup(t)
	if _, err := db.Exec(ctx(t), `
		INSERT INTO tracking_addresses (address, campaign_id, source, active) VALUES
			('search-fall@leads.example.com', 'search-fall', 'google', true),
			('old-promo@leads.example.com', 'old-promo', 'email', false)`); err != nil {
		t.Fatalf("seed tracking addresses: %v", err)
	}

	record(t, store, email(t, "<m1@example.net>", "jordan@example.net", "Search-Fall@Leads.Example.com", t0))
	record(t, store, email(t, "<m2@example.net>", "jordan@example.net", "old-promo@leads.example.com", t0))
	record(t, store, email(t, "<m3@example.net>", "jordan@example.net", "nobody@leads.example.com", t0))

	got := interaction(t, db, "<m1@example.net>")
	if str(got.CampaignID) != "search-fall" || got.Source != "google" || got.LeadID != nil {
		t.Errorf("known address: campaign=%s source=%s lead=%v, want search-fall google no lead",
			str(got.CampaignID), got.Source, got.LeadID)
	}
	for _, id := range []string{"<m2@example.net>", "<m3@example.net>"} {
		if got := interaction(t, db, id); got.CampaignID != nil || got.Source != "unknown" {
			t.Errorf("%s: campaign=%s source=%s, want unknown", id, str(got.CampaignID), got.Source)
		}
	}
	var toAddr string
	var toNum *string
	if err := db.QueryRow(ctx(t), `SELECT to_address, to_e164 FROM interactions WHERE provider_id = '<m1@example.net>'`).
		Scan(&toAddr, &toNum); err != nil {
		t.Fatalf("destination: %v", err)
	}
	if toAddr != "search-fall@leads.example.com" || toNum != nil {
		t.Errorf("to_address=%q to_e164=%v, want the lowercased address and no number", toAddr, toNum)
	}
	if n := count(t, db, "leads"); n != 0 {
		t.Errorf("%d leads, want 0 for email senders", n)
	}
}

// A tracking number written like an address must not match an email, and vice versa.
func TestRecordEmailDoesNotMatchTrackingNumbers(t *testing.T) {
	db, store := setup(t)
	record(t, store, email(t, "<m4@example.net>", "jordan@example.net", googleNum, t0))
	if got := interaction(t, db, "<m4@example.net>"); got.CampaignID != nil {
		t.Errorf("email to %s credited to %s, want unknown", googleNum, str(got.CampaignID))
	}
}

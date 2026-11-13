//go:build integration

package e2e

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/stream"
)

// checksum summarises what attribution produced, independent of row ids and timestamps: each
// interaction's key fields with its lead's number, and each lead's first and last touch.
func (e env) checksum(t *testing.T) string {
	t.Helper()
	var sum string
	err := e.db.QueryRow(context.Background(), `
SELECT md5(
  (SELECT coalesce(string_agg(concat_ws('|', i.provider_id, i.channel, i.from_number, i.to_e164,
          i.campaign_id, i.source, l.phone_e164), ';' ORDER BY i.provider_id), '')
     FROM interactions i LEFT JOIN leads l ON l.id = i.lead_id)
  || '#' ||
  (SELECT coalesce(string_agg(concat_ws('|', phone_e164, first_campaign_id, first_source,
          last_campaign_id, last_source), ';' ORDER BY phone_e164), '') FROM leads))`).Scan(&sum)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	return sum
}

func (e env) seedTrackingNumbers(t *testing.T) {
	t.Helper()
	if _, err := e.db.Exec(context.Background(), `INSERT INTO tracking_numbers (phone_e164, campaign_id, source)
		VALUES ('+18005550101', 'search-fall', 'google'), ('+18005550102', 'social-fall', 'facebook')`); err != nil {
		t.Fatalf("seed tracking numbers: %v", err)
	}
}

// sendCalls sends calls [from, to) from five callers to two campaigns, alternating, so each
// lead's first and last touch depend on the order the calls happened in.
func (e env) sendCalls(t *testing.T, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		caller := fmt.Sprintf("+1415555010%d", i%5)
		dest := []string{"+18005550101", "+18005550102"}[(i/5)%2]
		if code := e.sendCall(t, fmt.Sprintf("CAreplay%04d", i), caller, dest); code != 200 {
			t.Fatalf("call %d: status %d", i, code)
		}
	}
}

// waitAcked waits until attribution has acknowledged everything in the stream.
func (e env) waitAcked(t *testing.T) {
	t.Helper()
	eventually(t, "attribution to acknowledge every message", func() bool {
		s, err := e.js.Stream(context.Background(), stream.StreamName)
		if err != nil {
			return false
		}
		c, err := e.js.Consumer(context.Background(), stream.StreamName, "attribution")
		if err != nil {
			return false
		}
		ci, err := c.Info(context.Background())
		return err == nil && ci.AckFloor.Stream == s.CachedInfo().State.LastSeq
	})
}

// replay runs attribution -replay-from and returns its output once it exits.
func (e env) replay(t *testing.T, from time.Time) string {
	t.Helper()
	p := startArgs(t, "replay", attributionBin, []string{"-replay-from", from.UTC().Format(time.RFC3339Nano)},
		"DATABASE_URL="+e.dbURL, "NATS_URL="+e.natsURL)
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("replay exited with %v:\n%s", err, p.out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("replay didn't finish:\n%s", p.out.String())
	}
	return p.out.String()
}

// The recovery drill: a silent write failure leaves a gap; replaying the stream from just
// before it began restores exactly what a clean run produces, and replaying again changes
// nothing.
func TestReplayRecoversAnInjectedGap(t *testing.T) {
	const n = 150

	clean := setup(t)
	cleanAttr := clean.startAttribution(t)
	clean.seedTrackingNumbers(t)
	cleanIngest := clean.startIngest(t)
	clean.sendCalls(t, 0, n)
	clean.waitAcked(t)
	want := clean.checksum(t)
	if got := clean.count(t, "SELECT count(*) FROM interactions"); got != n {
		t.Fatalf("clean run has %d interactions, want %d", got, n)
	}
	cleanIngest.stop(t)
	cleanAttr.stop(t)

	e := setup(t)
	attribution := e.startAttributionBin(t, attributionFaultBin)
	e.seedTrackingNumbers(t)
	ingest := e.startIngest(t)
	e.sendCalls(t, 0, 50)
	e.waitAcked(t)

	incident := time.Now()
	if err := attribution.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fault to be on", func() bool { return strings.Contains(attribution.out.String(), "FAULT INJECTION on") })
	e.sendCalls(t, 50, 100)
	e.waitAcked(t)
	if err := attribution.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fault to be off", func() bool { return strings.Contains(attribution.out.String(), "FAULT INJECTION off") })
	e.sendCalls(t, 100, n)
	e.waitAcked(t)
	if got := e.count(t, "SELECT count(*) FROM interactions"); got != n-50 {
		t.Fatalf("after the fault: %d interactions, want %d (a gap of 50)", got, n-50)
	}

	out := e.replay(t, incident.Add(-time.Second))
	if got := e.count(t, "SELECT count(*) FROM interactions"); got != n {
		t.Errorf("after replay: %d interactions, want %d\n%s", got, n, out)
	}
	if got := e.checksum(t); got != want {
		t.Errorf("after replay the checksum differs from the clean run's\n%s", out)
	}
	if !strings.Contains(out, "recorded=50") {
		t.Errorf("replay didn't report the 50 it restored:\n%s", out)
	}

	out = e.replay(t, incident.Add(-time.Second))
	if got := e.checksum(t); got != want || !strings.Contains(out, "recorded=0") {
		t.Errorf("a second replay changed something:\n%s", out)
	}
	ingest.stop(t)
	attribution.stop(t)
}

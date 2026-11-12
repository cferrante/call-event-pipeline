package monitor

import (
	"testing"
	"time"
)

func TestTrackerReportsEachTransitionOnce(t *testing.T) {
	var tr Tracker
	t0 := simStart
	stale := Finding{Rule: RuleStaleBacklog, Subject: "inference", Detail: "oldest 6m"}
	drop := Finding{Rule: RuleVolumeDrop, Detail: "2/s"}

	if ev := tr.Update(t0, nil); len(ev) != 0 {
		t.Fatalf("nothing firing: events %+v", ev)
	}
	ev := tr.Update(t0.Add(5*time.Second), []Finding{stale})
	if len(ev) != 1 || ev[0].State != Firing || ev[0].Rule != RuleStaleBacklog || ev[0].Subject != "inference" {
		t.Fatalf("first finding: events %+v, want one firing", ev)
	}
	stale.Detail = "oldest 7m" // still firing, new detail: no new event
	if ev := tr.Update(t0.Add(10*time.Second), []Finding{stale, drop}); len(ev) != 1 || ev[0].Rule != RuleVolumeDrop {
		t.Fatalf("second update: events %+v, want only volume drop firing", ev)
	}
	if got := tr.Active(); len(got) != 2 || got[0].Since != t0.Add(5*time.Second) {
		t.Errorf("active = %+v, want both, stale since its first firing", got)
	}
	ev = tr.Update(t0.Add(15*time.Second), []Finding{drop})
	if len(ev) != 1 || ev[0].State != Resolved || ev[0].Rule != RuleStaleBacklog || ev[0].Duration != 10*time.Second {
		t.Fatalf("stale cleared: events %+v, want it resolved after 10s", ev)
	}
}

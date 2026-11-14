package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSampler struct {
	next Sample
	err  error
}

func (f *fakeSampler) Sample(_ context.Context, now time.Time) (Sample, error) {
	s := f.next
	s.At = now
	return s, f.err
}

// Each tick samples, evaluates, reports alert changes once, and updates the status shown on
// the page.
func TestTickReportsAlertChangesAndStatus(t *testing.T) {
	cfg := DefaultConfig
	cfg.CanaryMaxAge = 10 * time.Second
	src := &fakeSampler{}
	var events []Event
	svc := NewService(src, cfg, func(e Event) { events = append(events, e) })

	now := simStart
	src.next = Sample{Arrivals: 100, CanarySeen: now, Consumers: map[string]ConsumerSample{
		"attribution": {Acked: 90, Written: 90}}}
	svc.Tick(context.Background(), now)
	src.next.Arrivals, src.next.Consumers["attribution"] = 150, ConsumerSample{Acked: 140, Written: 140,
		OldestPending: now}
	svc.Tick(context.Background(), now.Add(5*time.Second))

	st := svc.Status()
	if len(events) != 0 || len(st.Active) != 0 {
		t.Fatalf("healthy: events %+v, active %+v", events, st.Active)
	}
	if st.ArrivalsPerSec != 10 || st.Consumers["attribution"].Lag != 10 ||
		st.Consumers["attribution"].OldestPendingAge != 5*time.Second {
		t.Errorf("status = %+v, want 10/s arrivals, lag 10, oldest 5s", st)
	}

	// The canary stops: one firing event, then nothing while it stays down.
	for i := 2; i <= 5; i++ {
		svc.Tick(context.Background(), now.Add(time.Duration(i)*5*time.Second))
	}
	if len(events) != 1 || events[0].Rule != RuleArrivalsStopped || events[0].State != Firing {
		t.Fatalf("events = %+v, want one arrivals_stopped firing", events)
	}
	if st := svc.Status(); len(st.Active) != 1 || len(st.Recent) != 1 {
		t.Errorf("status active %d, recent %d; want 1 and 1", len(st.Active), len(st.Recent))
	}
}

// A failed reading is shown on the page and logged, but never treated as a reading of zero,
// which would look like an outage. Readings that keep failing are an alert of their own: a
// monitor that can't see the pipeline must not stay silent through an outage.
func TestTickAlertsWhenReadingsKeepFailing(t *testing.T) {
	cfg := DefaultConfig
	cfg.CanaryMaxAge = 10 * time.Second
	cfg.ReadingsFailAfter = 20 * time.Second
	src := &fakeSampler{next: Sample{CanarySeen: simStart, Consumers: map[string]ConsumerSample{}}}
	var events []Event
	svc := NewService(src, cfg, func(e Event) { events = append(events, e) })
	tick := func(i int) {
		at := simStart.Add(time.Duration(i) * 5 * time.Second)
		svc.Tick(context.Background(), at)
	}

	// Healthy, then the canary stops, so arrivals_stopped is firing before readings fail.
	for i := range 4 {
		tick(i)
	}
	if len(events) != 1 || events[0].Rule != RuleArrivalsStopped {
		t.Fatalf("before the failure: events %+v, want arrivals_stopped firing", events)
	}

	// Readings fail. Nothing for the first ReadingsFailAfter, then one readings_failing alert;
	// arrivals_stopped neither resolves nor fires again while the monitor can't see.
	src.err = errors.New("postgres down")
	for i := 4; i <= 9; i++ { // failing from 20 s to 45 s
		tick(i)
		if svc.Status().SampleError == "" {
			t.Fatal("status doesn't show the sampling error")
		}
	}
	if len(events) != 2 || events[1].Rule != RuleReadingsFailing || events[1].State != Firing {
		t.Fatalf("after 25 s of failed readings: events %+v, want readings_failing firing", events[1:])
	}
	if !strings.Contains(events[1].Detail, "postgres down") {
		t.Errorf("detail %q doesn't name the error", events[1].Detail)
	}

	// A good reading resolves readings_failing; the canary is back too.
	src.err = nil
	src.next.CanarySeen = simStart.Add(50 * time.Second)
	tick(10)
	got := events[2:]
	if len(got) != 2 {
		t.Fatalf("after recovery: events %+v, want both alerts resolved", got)
	}
	for _, e := range got {
		if e.State != Resolved {
			t.Errorf("after recovery: %+v, want resolved", e)
		}
	}
}

// Brief failures, each shorter than ReadingsFailAfter, raise nothing, even when together
// they last longer: a good reading in between starts the count again.
func TestTickToleratesBriefReadingFailures(t *testing.T) {
	src := &fakeSampler{next: Sample{CanarySeen: simStart, Consumers: map[string]ConsumerSample{}}}
	var events []Event
	svc := NewService(src, DefaultConfig, func(e Event) { events = append(events, e) })
	for i := range 11 { // failing 0–20 s and 30–50 s, one good reading at 25 s
		at := simStart.Add(time.Duration(i) * 5 * time.Second)
		src.err = errors.New("timeout")
		if i == 5 {
			src.err, src.next.CanarySeen = nil, at
		}
		svc.Tick(context.Background(), at)
	}
	if len(events) != 0 {
		t.Errorf("two 20 s failures produced events %+v", events)
	}
}

// History is trimmed to what the rules need, so a monitor running for weeks doesn't grow.
func TestHistoryIsBounded(t *testing.T) {
	src := &fakeSampler{next: Sample{Consumers: map[string]ConsumerSample{}}}
	svc := NewService(src, DefaultConfig, func(Event) {})
	for i := range 3 * 720 { // 3 hours of 5 s ticks
		src.next.CanarySeen = simStart.Add(time.Duration(i) * 5 * time.Second)
		svc.Tick(context.Background(), src.next.CanarySeen)
	}
	if n := svc.historyLen(); n > 900 {
		t.Errorf("history holds %d samples after 3 hours, want about 70 minutes' worth (≤ 900)", n)
	}
}

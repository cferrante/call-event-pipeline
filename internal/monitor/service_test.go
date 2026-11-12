package monitor

import (
	"context"
	"errors"
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
// which would look like an outage.
func TestTickSkipsAFailedSample(t *testing.T) {
	src := &fakeSampler{err: errors.New("postgres down")}
	var events []Event
	svc := NewService(src, DefaultConfig, func(e Event) { events = append(events, e) })
	for i := range 20 {
		svc.Tick(context.Background(), simStart.Add(time.Duration(i)*5*time.Second))
	}
	if len(events) != 0 {
		t.Errorf("failed samples produced events %+v", events)
	}
	if st := svc.Status(); st.SampleError == "" {
		t.Error("status doesn't show the sampling error")
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

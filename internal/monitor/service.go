package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Source takes readings; *Sampler is the real one.
type Source interface {
	Sample(ctx context.Context, now time.Time) (Sample, error)
}

// Status is what the status page shows.
type Status struct {
	At             time.Time
	ArrivalsPerSec float64
	Consumers      map[string]ConsumerStatus
	Active         []Alert
	Recent         []Event // newest first
	SampleError    string  // the last reading's error, if it failed
}

// ConsumerStatus is one consumer's line on the page.
type ConsumerStatus struct {
	Lag              uint64
	OldestPendingAge time.Duration
	WrittenPerSec    float64
}

const recentEvents = 20

// Service runs the monitor: each Tick takes a reading, evaluates the rules, reports alerts
// that start or end, and updates the status the page shows. Safe for concurrent use: Status
// is read by HTTP handlers while ticks run.
type Service struct {
	src   Source
	cfg   Config
	alert func(Event)

	mu      sync.Mutex
	history []Sample
	tracker Tracker
	status  Status
	// failingSince is when readings started failing; zero while they succeed.
	failingSince time.Time
}

// NewService returns a monitor that calls alert for every alert that starts or ends.
func NewService(src Source, cfg Config, alert func(Event)) *Service {
	return &Service{src: src, cfg: cfg, alert: alert}
}

// Tick takes one reading at now. A failed reading is reported and skipped: treating it as a
// reading of zero would look like an outage. Readings that keep failing for ReadingsFailAfter
// raise readings_failing, so a monitor that can't see the pipeline isn't silent through an
// outage; alerts already firing stay as they are until a reading succeeds.
func (s *Service) Tick(ctx context.Context, now time.Time) {
	sample, err := s.src.Sample(ctx, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		slog.Error("monitor reading failed", "err", err)
		s.status.SampleError = err.Error()
		if s.failingSince.IsZero() {
			s.failingSince = now
		}
		if age := now.Sub(s.failingSince); age >= s.cfg.ReadingsFailAfter {
			findings := []Finding{{Rule: RuleReadingsFailing,
				Detail: fmt.Sprintf("no reading for %v: %v", age.Round(time.Second), err)}}
			for _, a := range s.tracker.Active() {
				if a.Rule != RuleReadingsFailing {
					findings = append(findings, a.Finding)
				}
			}
			s.report(now, findings)
		}
		return
	}
	s.failingSince = time.Time{}
	s.status.SampleError = ""
	s.history = append(s.history, sample)
	s.trim(now)
	s.report(now, Evaluate(s.cfg, s.history))
	s.updateRates(sample)
}

// report records findings at now, sends each alert that starts or ends, and updates the page.
func (s *Service) report(now time.Time, findings []Finding) {
	for _, e := range s.tracker.Update(now, findings) {
		s.alert(e)
		s.status.Recent = slices.Insert(s.status.Recent, 0, e)
		if len(s.status.Recent) > recentEvents {
			s.status.Recent = s.status.Recent[:recentEvents]
		}
	}
	s.status.Active = s.tracker.Active()
}

// trim keeps only the history the rules look at: the volume rule's baseline plus its recent
// window, with a minute to spare.
func (s *Service) trim(now time.Time) {
	keep := now.Add(-(s.cfg.Baseline + s.cfg.Recent + 2*time.Minute))
	i := 0
	for i < len(s.history)-1 && s.history[i].At.Before(keep) {
		i++
	}
	s.history = s.history[i:]
}

func (s *Service) updateRates(now Sample) {
	s.status.At = now.At
	s.status.Consumers = map[string]ConsumerStatus{}
	var prev *Sample
	if n := len(s.history); n >= 2 {
		prev = &s.history[n-2]
	}
	if prev != nil {
		s.status.ArrivalsPerSec = float64(now.Arrivals-prev.Arrivals) / now.At.Sub(prev.At).Seconds()
	}
	for name, c := range now.Consumers {
		cs := ConsumerStatus{}
		if now.Arrivals > c.Acked {
			cs.Lag = now.Arrivals - c.Acked
		}
		if !c.OldestPending.IsZero() {
			cs.OldestPendingAge = now.At.Sub(c.OldestPending)
		}
		if prev != nil {
			if p, ok := prev.Consumers[name]; ok && c.Written >= p.Written {
				cs.WrittenPerSec = float64(c.Written-p.Written) / now.At.Sub(prev.At).Seconds()
			}
		}
		s.status.Consumers[name] = cs
	}
}

// Status returns what the page shows.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Active = slices.Clone(st.Active)
	st.Recent = slices.Clone(st.Recent)
	return st
}

func (s *Service) historyLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

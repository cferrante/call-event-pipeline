package monitor

import (
	"time"
)

// sim generates samples every 5 s from a simple model of the pipeline, so rules can be tested
// against realistic traffic shapes (including the recorded M2 spike) in microseconds.
type sim struct {
	at        time.Time
	arrivals  float64
	canary    time.Time
	history   []Sample
	arrivedAt []point // cumulative arrivals over time, to find the oldest pending message
	consumers map[string]*simConsumer
}

type point struct {
	at  time.Time
	cum float64
}

type simConsumer struct {
	capacity  float64 // messages per second it can process
	writeFrac float64 // share of processed messages that reach the database (1 normally)
	dlqFrac   float64 // share dead-lettered instead
	acked     float64
	written   float64
	dead      float64
}

var simStart = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func newSim() *sim {
	return &sim{at: simStart, consumers: map[string]*simConsumer{
		"attribution": {capacity: 700, writeFrac: 1},
		"inference":   {capacity: 300, writeFrac: 1},
	}}
}

// run advances the simulation by d at rate arrivals per second, with or without the canary.
func (s *sim) run(d time.Duration, rate float64, canary bool) {
	const step = 5 * time.Second
	for elapsed := time.Duration(0); elapsed < d; elapsed += step {
		s.at = s.at.Add(step)
		s.arrivals += rate * step.Seconds()
		if canary {
			s.canary = s.at.Add(-time.Duration(s.at.Unix()%10) * time.Second) // one every 10 s
		}
		s.arrivedAt = append(s.arrivedAt, point{s.at, s.arrivals})
		sample := Sample{At: s.at, Arrivals: uint64(s.arrivals), CanarySeen: s.canary,
			Consumers: map[string]ConsumerSample{}}
		for name, c := range s.consumers {
			processed := min(s.arrivals-c.acked, c.capacity*step.Seconds())
			c.acked += processed
			c.written += processed * c.writeFrac
			c.dead += processed * c.dlqFrac
			cs := ConsumerSample{Acked: uint64(c.acked), Written: uint64(c.written), DeadLettered: uint64(c.dead)}
			if c.acked < s.arrivals {
				cs.OldestPending = s.oldestPending(c.acked)
			}
			sample.Consumers[name] = cs
		}
		s.history = append(s.history, sample)
	}
}

// oldestPending is when the first message after acked arrived.
func (s *sim) oldestPending(acked float64) time.Time {
	for _, p := range s.arrivedAt {
		if p.cum > acked {
			return p.at
		}
	}
	return s.at
}

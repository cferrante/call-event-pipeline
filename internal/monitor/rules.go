// Package monitor watches the pipeline from outside for silent failures: components that stay
// up but stop doing their job (docs/design.md, section 8.1).
//
// Lag alone is never a failure. A spike that builds a backlog and drains it is the pipeline
// working as designed, and a watchdog that alerts on it gets ignored. Every rule here is
// tested against the recorded M2 spike shape and must stay quiet through it.
package monitor

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Rule names.
const (
	RuleArrivalsStopped = "arrivals_stopped"
	RuleVolumeDrop      = "volume_drop"
	RuleAckedNotWritten = "acked_not_written"
	RuleStaleBacklog    = "stale_backlog"
	RuleReadingsFailing = "readings_failing"
)

// Sample is one reading of the pipeline.
type Sample struct {
	At         time.Time
	Arrivals   uint64    // events accepted into the stream so far (its last sequence)
	CanarySeen time.Time // when the newest canary interaction was stored; zero if none yet
	Consumers  map[string]ConsumerSample
}

// ConsumerSample is one consumer's progress.
type ConsumerSample struct {
	Acked         uint64    // stream position delivered, less the messages still awaiting an ack
	Written       uint64    // rows it has written
	DeadLettered  uint64    // messages it has dead-lettered
	OldestPending time.Time // when its oldest unacknowledged message arrived; zero if none
}

// Config holds the rules' thresholds.
type Config struct {
	// Arrivals stopped: no canary stored for this long; 0 when the canary is off.
	CanaryMaxAge time.Duration
	// Volume drop: the recent rate is below DropRatio of the baseline, the median per-minute
	// rate over the Baseline before it, provided that baseline is at least MinRate per second.
	Recent, Baseline time.Duration
	DropRatio        float64
	MinRate          float64
	// Acked but not written: over WriteWindow, rows written plus dead letters are below
	// MinWrittenRatio of the messages acknowledged, with at least MinAcked acknowledged.
	WriteWindow     time.Duration
	MinWrittenRatio float64
	MinAcked        uint64
	// Stale backlog: a consumer's oldest pending message is older than this.
	StaleAfter time.Duration
	// Readings failing: the monitor itself has failed to read the pipeline for this long.
	ReadingsFailAfter time.Duration
}

// DefaultConfig is the spec's thresholds.
var DefaultConfig = Config{
	CanaryMaxAge: 30 * time.Second,
	Recent:       5 * time.Minute, Baseline: time.Hour, DropRatio: 0.2, MinRate: 1,
	WriteWindow: time.Minute, MinWrittenRatio: 0.9, MinAcked: 100,
	StaleAfter: 5 * time.Minute, ReadingsFailAfter: 30 * time.Second,
}

// Finding is a rule that is firing now.
type Finding struct {
	Rule    string
	Subject string // the consumer, for per-consumer rules
	Detail  string
}

// Evaluate checks every rule against history, oldest first, the newest sample being now.
func Evaluate(cfg Config, history []Sample) []Finding {
	if len(history) == 0 {
		return nil
	}
	var out []Finding
	out = append(out, arrivalsStopped(cfg, history)...)
	out = append(out, volumeDrop(cfg, history)...)
	now := history[len(history)-1]
	for _, name := range sortedNames(now.Consumers) {
		out = append(out, ackedNotWritten(cfg, history, name)...)
		out = append(out, staleBacklog(cfg, now, name)...)
	}
	return out
}

// arrivalsStopped fires when no canary has been stored for CanaryMaxAge. The canary is a
// synthetic webhook sent every 10 s, so its absence means the path from the provider to the
// database is broken somewhere, even when real traffic is naturally quiet.
func arrivalsStopped(cfg Config, h []Sample) []Finding {
	if cfg.CanaryMaxAge == 0 {
		return nil // no canary is being sent, so its absence means nothing
	}
	now := h[len(h)-1]
	last := now.CanarySeen
	if last.Before(h[0].At) {
		last = h[0].At // just started: give the canary a full window before alerting
	}
	if age := now.At.Sub(last); age > cfg.CanaryMaxAge {
		return []Finding{{Rule: RuleArrivalsStopped, Detail: fmt.Sprintf("no canary stored for %v", age.Round(time.Second))}}
	}
	return nil
}

// volumeDrop fires when the recent arrival rate falls well below normal. Normal is the
// median of the per-minute rates over the baseline window, not the mean: one minute of a
// 1,000/s spike would lift an hour's mean several-fold and make ordinary traffic afterwards
// look like an outage, while the median barely moves.
func volumeDrop(cfg Config, h []Sample) []Finding {
	now := h[len(h)-1]
	recentStart, ok := at(h, now.At.Add(-cfg.Recent))
	if !ok {
		return nil
	}
	var minuteRates []float64
	for end := recentStart.At; end.Sub(recentStart.At) > -cfg.Baseline; end = end.Add(-time.Minute) {
		a, ok1 := at(h, end.Add(-time.Minute))
		b, ok2 := at(h, end)
		if !ok1 || !ok2 {
			return nil // not a full baseline yet
		}
		minuteRates = append(minuteRates, float64(b.Arrivals-a.Arrivals)/60)
	}
	baseline := median(minuteRates)
	recent := float64(now.Arrivals-recentStart.Arrivals) / now.At.Sub(recentStart.At).Seconds()
	if baseline >= cfg.MinRate && recent < cfg.DropRatio*baseline {
		return []Finding{{Rule: RuleVolumeDrop,
			Detail: fmt.Sprintf("%.1f/s over the last %v, normally %.1f/s", recent, cfg.Recent, baseline)}}
	}
	return nil
}

// ackedNotWritten fires when a consumer acknowledges messages without writing them: the
// process is up, its lag is zero, and nothing reaches the database. Dead letters count as
// handled, since acknowledging them without a row is deliberate.
func ackedNotWritten(cfg Config, h []Sample, name string) []Finding {
	now := h[len(h)-1]
	start, ok := at(h, now.At.Add(-cfg.WriteWindow))
	if !ok {
		return nil
	}
	a, okA := start.Consumers[name]
	b, okB := now.Consumers[name]
	if !okA || !okB || b.Acked < a.Acked {
		// Not a full window of readings (the consumer is new, or was reset): an ack floor is a
		// stream position and a row count starts at zero, so they're only comparable as deltas.
		return nil
	}
	acked := b.Acked - a.Acked
	handled := (b.Written - a.Written) + (b.DeadLettered - a.DeadLettered)
	if acked >= cfg.MinAcked && float64(handled) < cfg.MinWrittenRatio*float64(acked) {
		return []Finding{{Rule: RuleAckedNotWritten, Subject: name,
			Detail: fmt.Sprintf("%d acknowledged but %d written or dead-lettered in the last %v", acked, handled, cfg.WriteWindow)}}
	}
	return nil
}

// staleBacklog fires when a consumer's oldest pending message has waited too long. A spike's
// backlog drains within a few minutes; one that doesn't means the consumer is stuck.
func staleBacklog(cfg Config, now Sample, name string) []Finding {
	oldest := now.Consumers[name].OldestPending
	if oldest.IsZero() {
		return nil
	}
	if age := now.At.Sub(oldest); age > cfg.StaleAfter {
		return []Finding{{Rule: RuleStaleBacklog, Subject: name,
			Detail: fmt.Sprintf("oldest pending message is %v old", age.Round(time.Second))}}
	}
	return nil
}

// at returns the newest sample taken at or before t, if history reaches back that far.
func at(h []Sample, t time.Time) (Sample, bool) {
	if len(h) == 0 || h[0].At.After(t) {
		return Sample{}, false
	}
	i := sort.Search(len(h), func(i int) bool { return h[i].At.After(t) })
	return h[i-1], true
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func sortedNames(m map[string]ConsumerSample) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

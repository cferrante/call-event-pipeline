package monitor

import (
	"cmp"
	"slices"
	"time"
)

// State is whether an alert has started or ended.
type State string

const (
	Firing   State = "firing"
	Resolved State = "resolved"
)

// Event is an alert starting or ending.
type Event struct {
	State    State
	Rule     string
	Subject  string
	Detail   string
	At       time.Time
	Duration time.Duration // for Resolved: how long it fired
}

// Alert is one that is firing now.
type Alert struct {
	Finding
	Since time.Time
}

// Tracker turns each evaluation's findings into events: one when an alert starts and one when
// it ends, never one per evaluation while it keeps firing. Not safe for concurrent use.
type Tracker struct {
	active map[string]Alert
}

func key(f Finding) string { return f.Rule + "/" + f.Subject }

// Update records the findings at now and returns what changed.
func (t *Tracker) Update(now time.Time, findings []Finding) []Event {
	if t.active == nil {
		t.active = map[string]Alert{}
	}
	var events []Event
	seen := map[string]bool{}
	for _, f := range findings {
		k := key(f)
		seen[k] = true
		if a, ok := t.active[k]; ok {
			a.Detail = f.Detail // still firing; keep the latest detail for the status page
			t.active[k] = a
			continue
		}
		t.active[k] = Alert{Finding: f, Since: now}
		events = append(events, Event{State: Firing, Rule: f.Rule, Subject: f.Subject, Detail: f.Detail, At: now})
	}
	for k, a := range t.active {
		if !seen[k] {
			delete(t.active, k)
			events = append(events, Event{State: Resolved, Rule: a.Rule, Subject: a.Subject, Detail: a.Detail,
				At: now, Duration: now.Sub(a.Since)})
		}
	}
	return events
}

// Active returns the alerts firing now, oldest first.
func (t *Tracker) Active() []Alert {
	out := make([]Alert, 0, len(t.active))
	for _, a := range t.active {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Alert) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return cmp.Compare(key(a.Finding), key(b.Finding))
	})
	return out
}

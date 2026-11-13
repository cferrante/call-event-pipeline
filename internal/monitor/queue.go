package monitor

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Alerter makes one attempt to deliver an alert event. An error that has a Retryable() bool
// method saying false is permanent (the request itself is wrong); any other error may succeed
// later. An error with a RetryAt() time.Time method says when to try again, as a rate limit's
// reset time does.
type Alerter interface {
	Alert(ctx context.Context, e Event) error
}

// DeliveryState is where one alert email is on its way out.
type DeliveryState string

const (
	DeliveryQueued   DeliveryState = "queued"
	DeliveryRetrying DeliveryState = "retrying"
	DeliverySent     DeliveryState = "sent"
	DeliveryFailed   DeliveryState = "failed"
	DeliveryDropped  DeliveryState = "dropped" // the queue was full
)

// Delivery is one alert's line in the outbox.
type Delivery struct {
	Event     Event
	State     DeliveryState
	Attempts  int
	LastError string
	Updated   time.Time
}

// QueueOptions tune a Queue; DefaultQueueOptions suits alert emails.
type QueueOptions struct {
	Size                   int           // events waiting before new ones are dropped
	MinBackoff, MaxBackoff time.Duration // wait after the first failure, doubling up to the max
	MaxAttempts            int
	AttemptTimeout         time.Duration // bound on one attempt, so a hung API can't hold the queue forever
}

// DefaultQueueOptions retry for about three minutes: 1, 2, 4 … 60 s.
var DefaultQueueOptions = QueueOptions{Size: 100, MinBackoff: time.Second, MaxBackoff: time.Minute,
	MaxAttempts: 8, AttemptTimeout: 10 * time.Second}

const outboxSize = 20

// Queue delivers alerts in the background, one at a time and in order, with retry and
// backoff. Enqueue never blocks, so a slow or failing alerter can't delay rule evaluation.
type Queue struct {
	alerter Alerter
	opts    QueueOptions
	ch      chan *Delivery

	mu     sync.Mutex
	outbox []*Delivery // newest first
}

// NewQueue returns a queue that delivers through a; call Run to start it.
func NewQueue(a Alerter, opts QueueOptions) *Queue {
	return &Queue{alerter: a, opts: opts, ch: make(chan *Delivery, opts.Size)}
}

// Enqueue adds an alert without waiting. If the queue is full the alert is dropped, logged and
// shown in the outbox: losing an email is better than a monitor that stops watching.
func (q *Queue) Enqueue(e Event) {
	d := &Delivery{Event: e, State: DeliveryQueued, Updated: time.Now()}
	select {
	case q.ch <- d:
	default:
		d.State = DeliveryDropped
		slog.Error("alert queue full; alert email dropped", "rule", e.Rule, "subject", e.Subject, "state", e.State)
	}
	q.mu.Lock()
	q.outbox = slices.Insert(q.outbox, 0, d)
	if len(q.outbox) > outboxSize {
		q.outbox = q.outbox[:outboxSize]
	}
	q.mu.Unlock()
}

// Run delivers queued alerts until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-q.ch:
			q.deliver(ctx, d)
		}
	}
}

func (q *Queue) deliver(ctx context.Context, d *Delivery) {
	backoff := q.opts.MinBackoff
	for {
		actx, cancel := context.WithTimeout(ctx, q.opts.AttemptTimeout)
		err := q.alerter.Alert(actx, d.Event)
		cancel()
		attempts := q.update(d, func() { d.Attempts++ })
		if err == nil {
			q.update(d, func() { d.State, d.LastError = DeliverySent, "" })
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !retryable(err) || attempts >= q.opts.MaxAttempts {
			q.update(d, func() { d.State, d.LastError = DeliveryFailed, err.Error() })
			slog.Error("alert email failed", "rule", d.Event.Rule, "attempts", attempts, "err", err)
			return
		}
		q.update(d, func() { d.State, d.LastError = DeliveryRetrying, err.Error() })
		wait := backoff
		var ra interface{ RetryAt() time.Time }
		if errors.As(err, &ra) {
			if until := time.Until(ra.RetryAt()); until > wait {
				wait = min(until, q.opts.MaxBackoff)
			}
		}
		slog.Warn("alert email not delivered; will retry", "rule", d.Event.Rule, "attempt", attempts, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(2*backoff, q.opts.MaxBackoff)
	}
}

func retryable(err error) bool {
	var r interface{ Retryable() bool }
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true // unknown: assume the problem may pass
}

// update changes d under the lock and returns its attempt count.
func (q *Queue) update(d *Delivery, f func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	f()
	d.Updated = time.Now()
	return d.Attempts
}

// Outbox returns the most recent alerts and how their delivery went, newest first.
func (q *Queue) Outbox() []Delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Delivery, len(q.outbox))
	for i, d := range q.outbox {
		out[i] = *d
	}
	return out
}

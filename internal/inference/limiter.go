package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrDailyBudget means the limiter's tokens for the last 24 hours are used up. Waiting
// wouldn't help for hours, so the caller fails instead.
var ErrDailyBudget = errors.New("daily token budget used up")

// Limiter keeps calls to a hosted model within a tokens-per-minute rate limit and a daily
// token budget, so the provider's limit is respected rather than answered with 429s. It
// tracks every reservation in a sliding window: no 60 seconds ever hold more than perMinute
// tokens, provided each reservation's estimate is an upper bound of what the call uses.
type Limiter struct {
	perMinute, perDay int
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error

	mu  sync.Mutex
	log []*spent // oldest first; nothing older than a day
}

type spent struct {
	at     time.Time
	tokens int
}

// Reservation is tokens set aside for one call. Settle it with what the call actually used.
type Reservation struct {
	l *Limiter
	s *spent
}

func NewLimiter(perMinute, perDay int) *Limiter {
	return &Limiter{perMinute: perMinute, perDay: perDay, now: time.Now, sleep: sleepContext}
}

// Reserve waits until n tokens fit in the minute's budget and sets them aside. It fails at
// once if the daily budget can't take them, and returns ctx's error if cancelled while waiting.
func (l *Limiter) Reserve(ctx context.Context, n int) (*Reservation, error) {
	if n > l.perMinute {
		return nil, fmt.Errorf("a call estimated at %d tokens can never fit a budget of %d a minute", n, l.perMinute)
	}
	for {
		l.mu.Lock()
		now := l.now()
		l.forgetBefore(now.Add(-24 * time.Hour))
		if day := l.sum(time.Time{}); day+n > l.perDay {
			l.mu.Unlock()
			return nil, fmt.Errorf("%w: %d of %d tokens used in the last 24 hours", ErrDailyBudget, day, l.perDay)
		}
		wait := l.waitFor(now, n)
		if wait <= 0 {
			s := &spent{at: now, tokens: n}
			l.log = append(l.log, s)
			l.mu.Unlock()
			return &Reservation{l: l, s: s}, nil
		}
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// Settle replaces the reservation's estimate with the tokens actually used.
func (r *Reservation) Settle(actual int) {
	if r == nil {
		return
	}
	r.l.mu.Lock()
	r.s.tokens = actual
	r.l.mu.Unlock()
}

// UsedToday is the tokens spent in the last 24 hours.
func (l *Limiter) UsedToday() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sum(l.now().Add(-24 * time.Hour))
}

// waitFor is how long until n more tokens fit in the 60 seconds ending now: until enough of
// the oldest spends in the window have left it. Called with mu held.
func (l *Limiter) waitFor(now time.Time, n int) time.Duration {
	windowStart := now.Add(-time.Minute)
	over := l.sum(windowStart) + n - l.perMinute
	if over <= 0 {
		return 0
	}
	for _, s := range l.log {
		if !s.at.After(windowStart) {
			continue
		}
		over -= s.tokens
		if over <= 0 {
			return s.at.Add(time.Minute).Sub(now) + time.Millisecond
		}
	}
	return time.Minute // unreachable while n <= perMinute
}

// sum is the tokens spent after since. Called with mu held.
func (l *Limiter) sum(since time.Time) int {
	total := 0
	for _, s := range l.log {
		if s.at.After(since) {
			total += s.tokens
		}
	}
	return total
}

func (l *Limiter) forgetBefore(t time.Time) {
	i := 0
	for i < len(l.log) && !l.log[i].at.After(t) {
		i++
	}
	l.log = l.log[i:]
}

package inference

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// virtualClock is a clock whose sleep advances time instantly, so a limiter can be driven
// through hours of traffic in microseconds.
type virtualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newVirtualClock() *virtualClock {
	return &virtualClock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *virtualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *virtualClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

func newTestLimiter(c *virtualClock, perMinute, perDay int) *Limiter {
	l := NewLimiter(perMinute, perDay)
	l.now, l.sleep = c.now, c.sleep
	return l
}

type spend struct {
	at     time.Time
	tokens int
}

// The budget holds in every 60-second window, however reservations and settlements fall.
func TestLimiterNeverExceedsTheMinuteBudget(t *testing.T) {
	c := newVirtualClock()
	l := newTestLimiter(c, 8000, 10_000_000)
	r := rand.New(rand.NewPCG(1, 2))
	var spent []spend
	for i := 0; i < 2000; i++ {
		estimate := 100 + r.IntN(900)
		res, err := l.Reserve(context.Background(), estimate)
		if err != nil {
			t.Fatal(err)
		}
		actual := estimate - r.IntN(estimate/2) // the estimate is an upper bound
		res.Settle(actual)
		spent = append(spent, spend{c.now(), actual})
		_ = c.sleep(context.Background(), time.Duration(r.IntN(3000))*time.Millisecond)
	}
	for i := range spent {
		sum := 0
		for j := i; j < len(spent) && spent[j].at.Sub(spent[i].at) < time.Minute; j++ {
			sum += spent[j].tokens
		}
		if sum > 8000 {
			t.Fatalf("%d tokens in the minute from %s, budget 8000", sum, spent[i].at.Format(time.TimeOnly))
		}
	}
	// And it isn't trivially safe by refusing work: 2000 calls of ~400 tokens need ~100 minutes.
	if elapsed := c.now().Sub(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)); elapsed > 3*time.Hour {
		t.Fatalf("took %s of fake time; the limiter is far too slow", elapsed)
	}
}

// Settling for less than the estimate frees the difference at once.
func TestLimiterSettleReturnsUnusedTokens(t *testing.T) {
	c := newVirtualClock()
	l := newTestLimiter(c, 1000, 1_000_000)
	start := c.now()
	res, _ := l.Reserve(context.Background(), 900)
	res.Settle(100)
	if _, err := l.Reserve(context.Background(), 900); err != nil {
		t.Fatal(err)
	}
	if waited := c.now().Sub(start); waited != 0 {
		t.Fatalf("waited %s; the settled reservation should have left room", waited)
	}
}

func TestLimiterDailyBudget(t *testing.T) {
	c := newVirtualClock()
	l := newTestLimiter(c, 1000, 2500)
	for i := 0; i < 2; i++ {
		res, err := l.Reserve(context.Background(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		res.Settle(1000)
	}
	if _, err := l.Reserve(context.Background(), 1000); !errors.Is(err, ErrDailyBudget) {
		t.Fatalf("third reservation: got %v, want ErrDailyBudget", err)
	}
	if got := l.UsedToday(); got != 2000 {
		t.Fatalf("UsedToday = %d, want 2000", got)
	}
	// A day later the budget is back.
	_ = c.sleep(context.Background(), 24*time.Hour)
	if _, err := l.Reserve(context.Background(), 1000); err != nil {
		t.Fatalf("a day later: %v", err)
	}
}

func TestLimiterRejectsAReservationLargerThanAMinute(t *testing.T) {
	l := newTestLimiter(newVirtualClock(), 1000, 1_000_000)
	if _, err := l.Reserve(context.Background(), 1001); err == nil {
		t.Fatal("want an error: it could never fit")
	}
}

func TestLimiterWaitStopsWhenCancelled(t *testing.T) {
	c := newVirtualClock()
	l := newTestLimiter(c, 1000, 1_000_000)
	res, _ := l.Reserve(context.Background(), 1000)
	res.Settle(1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Reserve(ctx, 500); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// Command calibrate measures a real hosted model through Groq, at a low rate and with synthetic
// texts only: its accuracy on a labelled set (and the stub's, for comparison), its latency, and
// the request rate at which Groq starts answering 429. The numbers replace the stub's guessed
// profile (docs/design.md, section 5.4).
//
//	GROQ_API_KEY=… go run ./cmd/calibrate -model openai/gpt-oss-20b
//
// The key is read from the environment; fill it from SSM at run time (see docs/benchmarks.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/inference"
)

type options struct {
	model, effort, phases string
	latencyCalls          int
	latencyPerMin         int
	ramp                  []int
	rampFor               time.Duration
	budget                int64
}

func main() {
	var o options
	var ramp string
	flag.StringVar(&o.model, "model", "openai/gpt-oss-20b", "Groq model")
	flag.StringVar(&o.effort, "reasoning-effort", "", `reasoning_effort; default "low" for gpt-oss, none otherwise`)
	flag.StringVar(&o.phases, "phases", "accuracy,latency,ramp", "which measurements to run")
	flag.IntVar(&o.latencyCalls, "latency-calls", 100, "calls in the latency run")
	flag.IntVar(&o.latencyPerMin, "latency-rate", 20, "calls a minute in the latency run, well under the limit")
	flag.StringVar(&ramp, "ramp", "20,30,40,50", "calls a minute at each ramp step, without the limiter")
	flag.DurationVar(&o.rampFor, "ramp-step", time.Minute, "length of each ramp step")
	flag.Int64Var(&o.budget, "budget", 120_000, "stop before this many tokens are spent in total")
	flag.Parse()
	for _, f := range strings.Split(ramp, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			fail(fmt.Errorf("-ramp: %q is not a rate", f))
		}
		o.ramp = append(o.ramp, n)
	}
	if o.effort == "" && strings.HasPrefix(o.model, "openai/gpt-oss") {
		o.effort = "low"
	}
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		fail(errors.New("GROQ_API_KEY is not set; fill it from SSM (see docs/benchmarks.md)"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, o, key); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "calibrate:", err)
	os.Exit(1)
}

// calibration shares one token count across phases, so -budget caps the whole run.
type calibration struct {
	o     options
	key   string
	set   []sample
	spent int64 // tokens, from every adapter used so far
}

func (c *calibration) groq(limited bool, retries int) *inference.Groq {
	var lim *inference.Limiter
	if limited {
		lim = inference.NewLimiter(7200, int(c.o.budget))
	}
	return inference.NewGroq(inference.GroqConfig{
		BaseURL: "https://api.groq.com", APIKey: c.key, Model: c.o.model, ReasoningEffort: c.o.effort,
		MaxCompletionTokens: 256, MaxRetries: retries, Timeout: 10 * time.Second,
	}, lim)
}

// call classifies one sample and times it.
func call(ctx context.Context, g *inference.Groq, s sample) (inference.Result, time.Duration, error) {
	start := time.Now()
	r, err := g.Classify(ctx, inference.Input{Channel: envelope.Channel(s.Channel), Body: s.Text})
	return r, time.Since(start), err
}

func run(ctx context.Context, o options, key string) error {
	set, err := loadSet()
	if err != nil {
		return err
	}
	c := &calibration{o: o, key: key, set: set}
	fmt.Printf("# Calibration of %s (reasoning_effort %q), %s\n\n", o.model, o.effort, time.Now().UTC().Format("2006-01-02 15:04 MST"))
	for _, phase := range strings.Split(o.phases, ",") {
		var err error
		switch phase {
		case "accuracy":
			err = c.accuracy(ctx)
		case "latency":
			err = c.latency(ctx)
		case "ramp":
			err = c.rampUp(ctx)
		default:
			err = fmt.Errorf("unknown phase %q", phase)
		}
		if err != nil {
			return err
		}
	}
	fmt.Printf("Tokens spent in total: %d\n", c.spent)
	return nil
}

func (c *calibration) accuracy(ctx context.Context) error {
	g := c.groq(true, 2)
	stub := inference.NewStub(inference.StubConfig{Capacity: 1, BaseLatency: time.Nanosecond, Timeout: time.Second})
	var want, real, rules []string
	var lat []time.Duration
	var wrong []string
	for _, s := range c.set {
		r, d, err := call(ctx, g, s)
		if err != nil {
			return fmt.Errorf("accuracy: %w", err)
		}
		sr, _ := stub.Classify(ctx, inference.Input{Channel: envelope.Channel(s.Channel), Body: s.Text})
		want, real, rules = append(want, s.Intent), append(real, r.Intent), append(rules, sr.Intent)
		lat = append(lat, d)
		if r.Intent != s.Intent {
			wrong = append(wrong, fmt.Sprintf("- labelled %s, model said %s: %q", s.Intent, r.Intent, oneLine(s.Text)))
		}
	}
	c.spent += g.TokensUsed()
	fmt.Printf("## Accuracy on %d labelled synthetic texts\n\n", len(c.set))
	fmt.Println("| Classifier | Accuracy | sales | support | unknown |")
	fmt.Println("|---|---|---|---|---|")
	for _, row := range []struct {
		name string
		got  []string
	}{{c.o.model, real}, {"stub keyword rules", rules}} {
		fmt.Printf("| %s | %.0f%% (%d/%d) | %s | %s | %s |\n", row.name, 100*accuracy(want, row.got),
			matches(want, row.got, ""), len(want), perIntent(want, row.got, "sales"),
			perIntent(want, row.got, "support"), perIntent(want, row.got, "unknown"))
	}
	fmt.Printf("\nTokens per call: %.0f on average. Latency of these calls: p50 %s, p95 %s.\n",
		float64(g.TokensUsed())/float64(len(c.set)), ms(percentile(lat, 50)), ms(percentile(lat, 95)))
	if len(wrong) > 0 {
		fmt.Println("\nThe model's misses:")
		fmt.Println(strings.Join(wrong, "\n"))
	}
	fmt.Println()
	return nil
}

func (c *calibration) latency(ctx context.Context) error {
	g := c.groq(true, 2)
	every := time.Minute / time.Duration(c.o.latencyPerMin)
	var lat []time.Duration
	errs := 0
	t := time.NewTicker(every)
	defer t.Stop()
	for i := 0; i < c.o.latencyCalls; i++ {
		if c.spent+g.TokensUsed()+1000 > c.o.budget {
			fmt.Printf("(stopped after %d calls: token budget)\n", i)
			break
		}
		_, d, err := call(ctx, g, c.set[i%len(c.set)])
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs++
		} else {
			lat = append(lat, d)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.spent += g.TokensUsed()
	fmt.Printf("## Latency at %d calls a minute (one at a time, through the limiter)\n\n", c.o.latencyPerMin)
	fmt.Println("| Calls | Errors | 429s | min | p50 | p95 | p99 | max |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	fmt.Printf("| %d | %d | %d | %s | %s | %s | %s | %s |\n\n", len(lat)+errs, errs, g.RateLimited(),
		ms(percentile(lat, 0.0001)), ms(percentile(lat, 50)), ms(percentile(lat, 95)), ms(percentile(lat, 99)), ms(percentile(lat, 100)))
	return nil
}

// rampUp sends calls at rising rates with no limiter and no retries, to find where Groq starts
// refusing. It first waits a minute so earlier phases' tokens leave Groq's window.
func (c *calibration) rampUp(ctx context.Context) error {
	fmt.Println("## Ramp without the limiter (each call sent on schedule, not waiting for the last)")
	fmt.Println()
	if err := sleep(ctx, time.Minute); err != nil {
		return err
	}
	fmt.Println("| Calls a minute | Sent | OK | 429s | Other errors | Tokens | First 429 after |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, rate := range c.o.ramp {
		g := c.groq(false, 0)
		var mu sync.Mutex
		var wg sync.WaitGroup
		ok, other, sent := 0, 0, 0
		var first time.Duration
		start := time.Now()
		every := time.Minute / time.Duration(rate)
		for i := 0; time.Since(start) < c.o.rampFor; i++ {
			if c.spent+g.TokensUsed()+1000 > c.o.budget {
				fmt.Println("(stopped: token budget)")
				break
			}
			sent++
			wg.Add(1)
			go func(s sample) {
				defer wg.Done()
				_, _, err := call(ctx, g, s)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
				case errors.Is(err, inference.ErrOverloaded):
					if first == 0 {
						first = time.Since(start)
					}
				default:
					other++
				}
			}(c.set[i%len(c.set)])
			if err := sleep(ctx, every); err != nil {
				return err
			}
		}
		wg.Wait()
		c.spent += g.TokensUsed()
		firstText := "–"
		if first > 0 {
			firstText = first.Round(time.Second).String()
		}
		fmt.Printf("| %d | %d | %d | %d | %d | %d | %s |\n", rate, sent, ok, g.RateLimited(), other, g.TokensUsed(), firstText)
		// Let the window empty before the next step, so each step starts from zero.
		if err := sleep(ctx, time.Minute); err != nil {
			return err
		}
	}
	fmt.Println()
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func matches(want, got []string, intent string) int {
	n := 0
	for i := range want {
		if got[i] == want[i] && (intent == "" || want[i] == intent) {
			n++
		}
	}
	return n
}

// perIntent is "right/total" for the samples labelled intent.
func perIntent(want, got []string, intent string) string {
	total := 0
	for _, w := range want {
		if w == intent {
			total++
		}
	}
	return fmt.Sprintf("%d/%d", matches(want, got, intent), total)
}

func ms(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

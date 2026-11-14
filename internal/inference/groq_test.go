package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

const testKey = "gsk_test-key-not-real"

// fakeGroq stands in for Groq's chat completions API. It checks the key, answers with
// reply(), charges tokensPerCall per answer, and, when perMinute > 0, enforces a
// tokens-per-minute limit on clock with 429 and retry-after, as Groq does.
type fakeGroq struct {
	t             *testing.T
	clock         *virtualClock
	perMinute     int
	tokensPerCall int
	reply         func(text string) (status int, body string)

	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	spent    []spend
	limited  int
}

func (f *fakeGroq) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/openai/v1/chat/completions" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		http.Error(w, `{"error":{"message":"Invalid API Key"}}`, http.StatusUnauthorized)
		return
	}
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())

	if f.perMinute > 0 {
		now, used := f.clock.now(), 0
		for _, s := range f.spent {
			if now.Sub(s.at) < time.Minute {
				used += s.tokens
			}
		}
		if used+f.tokensPerCall > f.perMinute {
			f.limited++
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":{"message":"Rate limit reached: tokens per minute (TPM)"}}`, http.StatusTooManyRequests)
			return
		}
		f.spent = append(f.spent, spend{now, f.tokensPerCall})
	}

	status, content := http.StatusOK, `{"intent":"unknown","score":0.5}`
	if f.reply != nil {
		status, content = f.reply(userText(req))
	}
	if status != http.StatusOK {
		http.Error(w, content, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"total_tokens": f.tokensPerCall},
	})
}

// userText is the content of the last user message in a request.
func userText(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

func newTestGroq(t *testing.T, f *fakeGroq, lim *Limiter) *Groq {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	g := NewGroq(GroqConfig{
		BaseURL: srv.URL, APIKey: testKey, Model: "openai/gpt-oss-20b", ReasoningEffort: "low",
		MaxCompletionTokens: 256, Timeout: 2 * time.Second, MaxRetries: 2,
	}, lim)
	g.sleep = f.clock.sleep
	return g
}

func TestGroqRequestShape(t *testing.T) {
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	g := newTestGroq(t, f, nil)
	_, err := g.Classify(context.Background(), Input{Channel: envelope.SMS, Body: "Quote for 3 units? Text me at +14158675310 or jo@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.requests[0]
	if req["model"] != "openai/gpt-oss-20b" || req["reasoning_effort"] != "low" {
		t.Errorf("model/reasoning_effort = %v/%v", req["model"], req["reasoning_effort"])
	}
	if req["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v, want 256", req["max_completion_tokens"])
	}
	if rf, _ := req["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object", req["response_format"])
	}
	text := userText(req)
	if strings.Contains(text, "4158675310") || strings.Contains(text, "jo@example.com") {
		t.Errorf("personal data reached the model: %q", text)
	}
	if !strings.Contains(text, "Quote for 3 units?") || !strings.Contains(text, "sms") {
		t.Errorf("the text or channel is missing: %q", text)
	}
	if got := f.headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestGroqResult(t *testing.T) {
	tests := []struct {
		content string
		want    Result
	}{
		{`{"intent":"sales","score":0.82}`, Result{Intent: "sales", Score: 0.82}},
		{`{"intent":"Support","score":1.4}`, Result{Intent: "support", Score: 1}},
		{`{"intent":"billing","score":0.9}`, Result{Intent: "unknown", Score: 0.9}},
		{`{"intent":"sales","score":-1}`, Result{Intent: "sales", Score: 0}},
	}
	for _, tt := range tests {
		f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200,
			reply: func(string) (int, string) { return http.StatusOK, tt.content }}
		got, err := newTestGroq(t, f, nil).Classify(context.Background(), Input{Channel: envelope.SMS, Body: "hi"})
		if err != nil {
			t.Fatalf("%s: %v", tt.content, err)
		}
		tt.want.ModelVersion = "groq/openai/gpt-oss-20b"
		if got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.content, got, tt.want)
		}
	}
}

func TestGroqRejectsAnUnreadableReply(t *testing.T) {
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200,
		reply: func(string) (int, string) { return http.StatusOK, "I think this is sales" }}
	if _, err := newTestGroq(t, f, nil).Classify(context.Background(), Input{Channel: envelope.SMS, Body: "hi"}); err == nil {
		t.Fatal("want an error for a reply that isn't the JSON asked for")
	}
}

func TestGroqRetriesA429AfterRetryAfter(t *testing.T) {
	calls := 0
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	f.reply = func(string) (int, string) {
		calls++
		if calls <= 2 {
			return http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`
		}
		return http.StatusOK, `{"intent":"sales","score":0.9}`
	}
	g := newTestGroq(t, f, nil)
	start := f.clock.now()
	got, err := g.Classify(context.Background(), Input{Channel: envelope.SMS, Body: "price?"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != "sales" || g.RateLimited() != 2 {
		t.Fatalf("intent %q after %d rate limits; want sales after 2", got.Intent, g.RateLimited())
	}
	// No Retry-After header on these replies: the adapter falls back to 1 s, then 2 s.
	if waited := f.clock.now().Sub(start); waited < 3*time.Second {
		t.Fatalf("waited %s before retrying, want at least 3s", waited)
	}
}

func TestGroqGivesUpAsOverloadedAfterRetries(t *testing.T) {
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200,
		reply: func(string) (int, string) { return http.StatusTooManyRequests, `{}` }}
	_, err := newTestGroq(t, f, nil).Classify(context.Background(), Input{Channel: envelope.SMS, Body: "hi"})
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("got %v, want ErrOverloaded so the message is retried later", err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("%d requests, want 3 (the first and 2 retries)", len(f.requests))
	}
}

func TestGroqErrorsNeverContainTheKey(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest} {
		f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200,
			reply: func(string) (int, string) { return status, `{"error":{"message":"failed"}}` }}
		_, err := newTestGroq(t, f, nil).Classify(context.Background(), Input{Channel: envelope.SMS, Body: "hi"})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatalf("status %d: got %v, want an error naming the status", status, err)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("the API key leaked into an error: %v", err)
		}
	}
}

func TestGroqTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	g := NewGroq(GroqConfig{BaseURL: srv.URL, APIKey: testKey, Model: "m", MaxCompletionTokens: 64,
		Timeout: 50 * time.Millisecond}, nil)
	start := time.Now()
	if _, err := g.Classify(context.Background(), Input{Channel: envelope.SMS, Body: "hi"}); err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s; the 50ms timeout wasn't applied", elapsed)
	}
}

// The limiter keeps a hosted model's rate limit from ever answering 429; without it, the same
// traffic is rate limited. (Removing the limiter is this test's deliberate break.)
func TestGroqLimiterPreventsRateLimits(t *testing.T) {
	for _, withLimiter := range []bool{true, false} {
		c := newVirtualClock()
		f := &fakeGroq{t: t, clock: c, perMinute: 8000, tokensPerCall: 200}
		var lim *Limiter
		if withLimiter {
			lim = newTestLimiter(c, 8000, 1_000_000)
		}
		g := newTestGroq(t, f, lim)
		g.cfg.MaxRetries = 100 // so every call eventually succeeds either way
		for i := 0; i < 100; i++ {
			if _, err := g.Classify(context.Background(), Input{Channel: envelope.SMS, Body: "price?"}); err != nil {
				t.Fatal(err)
			}
		}
		if withLimiter && f.limited != 0 {
			t.Errorf("with the limiter: %d 429s, want 0", f.limited)
		}
		if !withLimiter && f.limited == 0 {
			t.Errorf("without the limiter: no 429s; the fake server isn't enforcing its limit")
		}
		if g.TokensUsed() != 100*200 {
			t.Errorf("TokensUsed = %d, want 20000 (with limiter: %v)", g.TokensUsed(), withLimiter)
		}
		if withLimiter && lim.UsedToday() != 100*200 {
			t.Errorf("limiter charged %d tokens, want the 20000 actually used", lim.UsedToday())
		}
	}
}

// A call has no text yet, and neither has the monitor's canary, sent every 10 s. Asking the
// model about nothing would spend the daily token budget on them (8,640 canaries a day at a
// few hundred tokens each is several times GROQ_TOKENS_PER_DAY), so it isn't asked.
func TestGroqSkipsTheModelWithoutText(t *testing.T) {
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	lim := NewLimiter(7200, 100_000)
	g := newTestGroq(t, f, lim)
	for _, body := range []string{"", " \n\t "} {
		got, err := g.Classify(context.Background(), Input{Channel: envelope.Voice, Body: body})
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if got != (Result{Intent: "unknown", Score: 1, ModelVersion: NoTextVersion}) {
			t.Errorf("body %q: %+v, want unknown from %s", body, got, NoTextVersion)
		}
	}
	if len(f.requests) != 0 || lim.UsedToday() != 0 {
		t.Errorf("%d requests sent and %d tokens charged for messages with no text", len(f.requests), lim.UsedToday())
	}
}

// An email's text is kept up to 64 KB, more than a minute's token budget. A message that
// could never fit would be retried until it was dead-lettered, unclassified; instead only its
// start is sent, which is where an email says what it wants.
func TestGroqSendsOnlyTheStartOfALongMessage(t *testing.T) {
	f := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	lim := NewLimiter(7200, 100_000)
	g := newTestGroq(t, f, lim)
	body := "Can I get a quote for a new roof? " + strings.Repeat("Some more detail about the house. ", 2000) + "THE END"
	if _, err := g.Classify(context.Background(), Input{Channel: envelope.Email, Body: body}); err != nil {
		t.Fatalf("a %d KB message: %v", len(body)>>10, err)
	}
	text := userText(f.requests[0])
	if !strings.Contains(text, "Can I get a quote") || strings.Contains(text, "THE END") {
		t.Errorf("sent %d bytes; want the start of the message only", len(text))
	}
	if len(text) > maxModelTextBytes+100 {
		t.Errorf("sent %d bytes, want at most about %d", len(text), maxModelTextBytes)
	}

	// A smaller budget a minute shrinks what is sent, so the call still fits it.
	f2 := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	g2 := newTestGroq(t, f2, NewLimiter(1500, 100_000))
	if _, err := g2.Classify(context.Background(), Input{Channel: envelope.Email, Body: body}); err != nil {
		t.Fatalf("with 1,500 tokens a minute: %v", err)
	}

	// The cut falls between characters, never inside one.
	f3 := &fakeGroq{t: t, clock: newVirtualClock(), tokensPerCall: 200}
	if _, err := newTestGroq(t, f3, nil).Classify(context.Background(),
		Input{Channel: envelope.Email, Body: "x" + strings.Repeat("é", maxModelTextBytes)}); err != nil {
		t.Fatal(err)
	}
	if text := userText(f3.requests[0]); strings.ContainsRune(text, utf8.RuneError) {
		t.Errorf("the cut split a character: ...%q", text[len(text)-8:])
	}
}

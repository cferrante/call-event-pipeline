package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// GroqConfig sets up the Groq adapter. The API key comes from the environment, which is filled
// from SSM Parameter Store at start; it is never written to disk or logged.
type GroqConfig struct {
	BaseURL             string // https://api.groq.com
	APIKey              string
	Model               string // e.g. openai/gpt-oss-20b
	ReasoningEffort     string // "low" for gpt-oss; empty to leave the model's default
	MaxCompletionTokens int    // caps the reply, so a call's token estimate is an upper bound
	MaxRetries          int    // 429s retried after retry-after before giving up as overloaded
	Timeout             time.Duration
}

// Groq classifies with a hosted model through Groq's OpenAI-compatible API, using only the
// standard library. Text is redacted before it leaves the process.
type Groq struct {
	cfg         GroqConfig
	lim         *Limiter // nil: no limit (calibration's ramp uses this to find the 429 point)
	client      *http.Client
	sleep       func(context.Context, time.Duration) error // replaced in tests
	rateLimited atomic.Int64
	tokensUsed  atomic.Int64
}

func NewGroq(cfg GroqConfig, lim *Limiter) *Groq {
	return &Groq{cfg: cfg, lim: lim, client: &http.Client{Timeout: cfg.Timeout}, sleep: sleepContext}
}

// TokensUsed is the tokens Groq reported for successful calls, limiter or not.
func (g *Groq) TokensUsed() int64 { return g.tokensUsed.Load() }

// RateLimited counts the 429s Groq has answered, retried or not.
func (g *Groq) RateLimited() int64 { return g.rateLimited.Load() }

const groqSystemPrompt = `You classify messages that people send to a business after seeing its marketing.
Answer with JSON only: {"intent": "sales" | "support" | "unknown", "score": <confidence from 0 to 1>}.
sales: wants to buy or book, or asks about prices, quotes, availability or options.
support: an existing customer with a problem, a question about an order, a refund or a cancellation.
unknown: anything else, including empty, unclear or unrelated messages.`

type groqResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

func (g *Groq) Classify(ctx context.Context, in Input) (Result, error) {
	user := fmt.Sprintf("Channel: %s\nMessage:\n%s", in.Channel, Redact(in.Body))
	req := map[string]any{
		"model": g.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": groqSystemPrompt},
			{"role": "user", "content": user},
		},
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": g.cfg.MaxCompletionTokens,
		"temperature":           0,
	}
	if g.cfg.ReasoningEffort != "" {
		req["reasoning_effort"] = g.cfg.ReasoningEffort
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	// An upper bound: under 3 characters a token for English, a margin for the chat template
	// and the provider's own system text, and the reply's cap.
	estimate := (len(groqSystemPrompt)+len(user))/3 + 100 + g.cfg.MaxCompletionTokens

	for attempt := 0; ; attempt++ {
		var res *Reservation
		if g.lim != nil {
			if res, err = g.lim.Reserve(ctx, estimate); err != nil {
				return Result{}, fmt.Errorf("groq: %w", err)
			}
		}
		status, header, respBody, err := g.post(ctx, body)
		if err != nil {
			res.Settle(estimate) // unknown use: charge the estimate
			return Result{}, fmt.Errorf("groq: %w", err)
		}
		if status == http.StatusTooManyRequests {
			res.Settle(0) // a refused request isn't charged
			g.rateLimited.Add(1)
			if attempt >= g.cfg.MaxRetries {
				return Result{}, fmt.Errorf("%w: groq rate limit after %d attempts", ErrOverloaded, attempt+1)
			}
			if err := g.sleep(ctx, retryAfter(header, attempt)); err != nil {
				return Result{}, err
			}
			continue
		}
		if status != http.StatusOK {
			res.Settle(estimate)
			return Result{}, fmt.Errorf("groq: status %d: %s", status, snippet(respBody))
		}
		var resp groqResponse
		if err := json.Unmarshal(respBody, &resp); err != nil || len(resp.Choices) == 0 {
			res.Settle(estimate)
			return Result{}, fmt.Errorf("groq: unreadable response: %s", snippet(respBody))
		}
		res.Settle(resp.Usage.TotalTokens)
		g.tokensUsed.Add(int64(resp.Usage.TotalTokens))
		return g.result(resp.Choices[0].Message.Content)
	}
}

func (g *Groq) post(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(g.cfg.BaseURL, "/")+"/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

// result reads the model's JSON answer; an intent outside the three becomes unknown.
func (g *Groq) result(content string) (Result, error) {
	var answer struct {
		Intent string  `json:"intent"`
		Score  float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		return Result{}, fmt.Errorf("groq: the model didn't answer in JSON: %s", snippet([]byte(content)))
	}
	intent := strings.ToLower(strings.TrimSpace(answer.Intent))
	if intent != "sales" && intent != "support" {
		intent = "unknown"
	}
	return Result{Intent: intent, Score: math.Min(1, math.Max(0, answer.Score)), ModelVersion: "groq/" + g.cfg.Model}, nil
}

// retryAfter is Groq's Retry-After in seconds, or 1 s, 2 s, 4 s… if it sent none; at most a minute.
func retryAfter(h http.Header, attempt int) time.Duration {
	if s, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && s >= 0 {
		return min(time.Duration(s*float64(time.Second)), time.Minute)
	}
	return min(time.Second<<attempt, time.Minute)
}

// snippet shortens a response body for an error message.
func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

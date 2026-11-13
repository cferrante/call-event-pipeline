package sendgrid

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

var testMail = Mail{From: "monitor@example.com", To: "oncall@example.com",
	Subject: "[FIRING] arrivals_stopped", Text: "No canary for 30s"}

// The request must match SendGrid's documented Mail Send v3 contract: POST /v3/mail/send,
// a bearer token, and personalizations/from/subject/content in JSON. The expected body is
// written out by hand from the docs, not produced by our own types, so a wrong tag or a
// missing field fails here.
func TestSendMatchesMailSendContract(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotType, gotAccept string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotType, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "SG.test-key", HTTP: srv.Client()}
	if err := c.Send(context.Background(), testMail); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v3/mail/send" {
		t.Errorf("request %s %s, want POST /v3/mail/send", gotMethod, gotPath)
	}
	if gotAuth != "Bearer SG.test-key" || gotType != "application/json" || gotAccept != "application/json" {
		t.Errorf("headers: Authorization %q, Content-Type %q, Accept %q", gotAuth, gotType, gotAccept)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(`{
		"personalizations": [{"to": [{"email": "oncall@example.com"}]}],
		"from": {"email": "monitor@example.com"},
		"subject": "[FIRING] arrivals_stopped",
		"content": [{"type": "text/plain", "value": "No canary for 30s"}]
	}`), &want)
	if g, w := mustJSON(gotBody), mustJSON(want); g != w {
		t.Errorf("body\n got %s\nwant %s", g, w)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Errors carry the status, SendGrid's messages, and whether a retry can help: 429 and 5xx
// can, a 4xx such as a bad key or a malformed body can't.
func TestSendErrors(t *testing.T) {
	reset := time.Now().Add(7 * time.Second).Truncate(time.Second)
	for _, tc := range []struct {
		status    int
		body      string
		retryable bool
		wait      bool // a reset time is reported
	}{
		{401, `{"errors":[{"field":null,"message":"authorization required"}]}`, false, false},
		{400, `{"errors":[{"field":"from.email","message":"does not contain a valid address"}]}`, false, false},
		{413, ``, false, false},
		{429, `{"errors":[{"field":null,"message":"too many requests"}]}`, true, true},
		{500, ``, true, false},
		{503, `not json`, true, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if tc.status == 429 {
				w.Header().Set("X-RateLimit-Limit", "600")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		c := &Client{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
		err := c.Send(context.Background(), testMail)
		srv.Close()

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%d: error %v is not an *APIError", tc.status, err)
			continue
		}
		if apiErr.Status != tc.status || apiErr.Retryable() != tc.retryable {
			t.Errorf("%d: status %d, retryable %v; want retryable %v", tc.status, apiErr.Status, apiErr.Retryable(), tc.retryable)
		}
		if tc.wait != !apiErr.Reset.IsZero() || (tc.wait && !apiErr.Reset.Equal(reset)) {
			t.Errorf("%d: reset %v, want %v", tc.status, apiErr.Reset, tc.wait)
		}
		if tc.body != "" && tc.body != "not json" && len(apiErr.Messages) != 1 {
			t.Errorf("%d: messages %q, want SendGrid's one message", tc.status, apiErr.Messages)
		}
	}
}

// A network failure (nothing listening) is retryable too: it isn't an API answer at all.
func TestSendNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, APIKey: "k", HTTP: &http.Client{Timeout: time.Second}}
	err := c.Send(context.Background(), testMail)
	if err == nil || !IsRetryable(err) {
		t.Fatalf("err = %v, retryable %v; want a retryable error", err, IsRetryable(err))
	}
}

// The monitor's alert queue decides retries through these methods, without importing this
// package's types: a 429's reset time, and a network error counting as retryable.
func TestErrorsTellTheQueueWhenToRetry(t *testing.T) {
	reset := time.Unix(1_800_000_000, 0)
	var withReset interface{ RetryAt() time.Time } = &APIError{Status: 429, Reset: reset}
	if !withReset.RetryAt().Equal(reset) {
		t.Errorf("RetryAt = %v", withReset.RetryAt())
	}
	var r interface{ Retryable() bool }
	if !errors.As(error(&netError{errors.New("refused")}), &r) || !r.Retryable() {
		t.Errorf("a network error doesn't say it is retryable")
	}
}

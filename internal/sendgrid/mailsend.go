package sendgrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Mail is a plain-text email to one recipient: all an alert needs.
type Mail struct {
	From, To, Subject, Text string
}

// MailRequest is the subset of SendGrid's Mail Send v3 request body that we use. It is
// exported so cmd/mailsink can decode the same documented shape.
type MailRequest struct {
	Personalizations []Personalization `json:"personalizations"`
	From             Address           `json:"from"`
	Subject          string            `json:"subject,omitempty"`
	Content          []Content         `json:"content,omitempty"`
}

// Personalization is one set of recipients.
type Personalization struct {
	To []Address `json:"to"`
}

// Address is an email address with an optional display name.
type Address struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Content is one MIME part of the body.
type Content struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Request returns m in Mail Send's request shape.
func (m Mail) Request() MailRequest {
	return MailRequest{
		Personalizations: []Personalization{{To: []Address{{Email: m.To}}}},
		From:             Address{Email: m.From},
		Subject:          m.Subject,
		Content:          []Content{{Type: "text/plain", Value: m.Text}},
	}
}

// ErrorBody is SendGrid's documented error response: {"errors":[{"message", "field"}]}.
type ErrorBody struct {
	Errors []ErrorItem `json:"errors"`
}

// ErrorItem is one problem with a request; Field is null when it isn't about one field.
type ErrorItem struct {
	Message string  `json:"message"`
	Field   *string `json:"field"`
}

// Client sends mail through SendGrid's Mail Send v3 API, or anything that implements it such
// as cmd/mailsink. A real account is a configuration change: BaseURL and APIKey.
type Client struct {
	BaseURL string // e.g. https://api.sendgrid.com, without the /v3 path
	APIKey  string
	HTTP    *http.Client
}

// Send makes one attempt. It doesn't retry: the caller decides, using IsRetryable and, for a
// rate limit, the error's Reset time.
func (c *Client) Send(ctx context.Context, m Mail) error {
	body, err := json.Marshal(m.Request())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.BaseURL, "/")+"/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &netError{err} // no answer at all, so trying again may work
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	apiErr := &APIError{Status: resp.StatusCode}
	var eb ErrorBody
	if raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); json.Unmarshal(raw, &eb) == nil {
		for _, e := range eb.Errors {
			apiErr.Messages = append(apiErr.Messages, e.Message)
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// The docs show epoch seconds in their example without stating the unit.
		if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && s > 0 {
			apiErr.Reset = time.Unix(s, 0)
		}
	}
	return apiErr
}

// APIError is a response other than 202 Accepted.
type APIError struct {
	Status   int
	Messages []string  // SendGrid's error messages, if the body had any
	Reset    time.Time // for 429: when the rate limit resets, if the response said
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("mail send: HTTP %d", e.Status)
	if len(e.Messages) > 0 {
		msg += ": " + strings.Join(e.Messages, "; ")
	}
	return msg
}

// Retryable reports whether trying again later can succeed: a rate limit or a server error.
// Any other 4xx means the request itself is wrong, and repeating it can't help.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// RetryAt is when a rate limit resets, or zero if the response didn't say.
func (e *APIError) RetryAt() time.Time { return e.Reset }

// netError is a request that got no answer: refused, timed out or cut off.
type netError struct{ err error }

func (e *netError) Error() string   { return "mail send: " + e.err.Error() }
func (e *netError) Unwrap() error   { return e.err }
func (e *netError) Retryable() bool { return true }

// IsRetryable reports whether err, from Send, may succeed on another attempt.
func IsRetryable(err error) bool {
	var r interface{ Retryable() bool }
	return errors.As(err, &r) && r.Retryable()
}

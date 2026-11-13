package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

const key = "SG.sink-test-key"

const validBody = `{
	"personalizations": [{"to": [{"email": "oncall@example.com"}]}],
	"from": {"email": "monitor@example.com"},
	"subject": "[FIRING] arrivals_stopped",
	"content": [{"type": "text/plain", "value": "No canary for 30s"}]
}`

func post(t *testing.T, h http.Handler, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v3/mail/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A valid request is accepted with 202, as SendGrid answers, and kept for the outbox.
func TestAcceptsValidMail(t *testing.T) {
	s := newSink(key)
	rec := post(t, s.handler(), "Bearer "+key, validBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202; body %s", rec.Code, rec.Body)
	}
	msgs := s.messages()
	if len(msgs) != 1 || msgs[0].To != "oncall@example.com" || msgs[0].Subject != "[FIRING] arrivals_stopped" ||
		msgs[0].Text != "No canary for 30s" || msgs[0].From != "monitor@example.com" {
		t.Errorf("stored %+v", msgs)
	}
}

// A missing or wrong key gets SendGrid's 401 and is not stored.
func TestRejectsMissingOrWrongKey(t *testing.T) {
	s := newSink(key)
	for _, auth := range []string{"", "Bearer wrong", key, "Basic " + key} {
		rec := post(t, s.handler(), auth, validBody)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, rec.Code)
		}
		assertErrorBody(t, rec)
	}
	if n := len(s.messages()); n != 0 {
		t.Errorf("stored %d messages from unauthorised requests", n)
	}
}

// A body that breaks the documented shape gets 400 with SendGrid's error JSON, naming the
// field, and is not stored.
func TestRejectsMalformedBody(t *testing.T) {
	s := newSink(key)
	for name, body := range map[string]string{
		"not json":            `{"personalizations": [`,
		"no personalizations": strings.Replace(validBody, `"personalizations": [{"to": [{"email": "oncall@example.com"}]}],`, ``, 1),
		"empty to":            strings.Replace(validBody, `[{"email": "oncall@example.com"}]`, `[]`, 1),
		"bad to address":      strings.Replace(validBody, `oncall@example.com`, `oncall`, 1),
		"no from":             strings.Replace(validBody, `"from": {"email": "monitor@example.com"},`, ``, 1),
		"no subject":          strings.Replace(validBody, `"subject": "[FIRING] arrivals_stopped",`, ``, 1),
		"no content":          strings.Replace(validBody, `"content": [{"type": "text/plain", "value": "No canary for 30s"}]`, `"content": []`, 1),
		"empty content value": strings.Replace(validBody, `"No canary for 30s"`, `""`, 1),
	} {
		rec := post(t, s.handler(), "Bearer "+key, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
			continue
		}
		assertErrorBody(t, rec)
	}
	if n := len(s.messages()); n != 0 {
		t.Errorf("stored %d malformed messages", n)
	}
}

func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var eb sendgrid.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil || len(eb.Errors) == 0 || eb.Errors[0].Message == "" {
		t.Errorf("error body %q is not SendGrid's {\"errors\":[{\"message\"}]} shape", rec.Body)
	}
}

// Our client and the sink agree end to end, and the outbox lists messages newest first, keeping
// only the most recent.
func TestClientToSinkAndOutbox(t *testing.T) {
	s := newSink(key)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	c := &sendgrid.Client{BaseURL: srv.URL, APIKey: key, HTTP: srv.Client()}
	for i := range keepMessages + 5 {
		m := sendgrid.Mail{From: "monitor@example.com", To: "oncall@example.com", Subject: "alert " + string(rune('a'+i%26)), Text: "x"}
		if err := c.Send(context.Background(), m); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []message
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	last := string(rune('a' + (keepMessages+4)%26))
	if len(got) != keepMessages || got[0].Subject != "alert "+last || got[0].Received.IsZero() {
		t.Errorf("outbox has %d, first %+v; want %d, newest first", len(got), got[0], keepMessages)
	}
}

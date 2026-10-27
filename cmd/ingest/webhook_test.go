package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cferrante/decoupled-ingestion-layer/internal/envelope"
	"github.com/cferrante/decoupled-ingestion-layer/internal/twilio"
)

const (
	testToken     = "test-auth-token"
	testPublicURL = "https://hooks.example.com"
	testResponse  = "<Response/>"
)

var testNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type fakePublisher struct {
	published   []envelope.Envelope
	hadDeadline bool
	err         error
}

func (f *fakePublisher) Publish(ctx context.Context, env envelope.Envelope) error {
	_, f.hadDeadline = ctx.Deadline()
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, env)
	return nil
}

func newTestWebhook(ch envelope.Channel, pub publisher) *webhook {
	return &webhook{
		channel:   ch,
		authToken: testToken,
		publicURL: testPublicURL,
		pub:       pub,
		now:       func() time.Time { return testNow },
		response:  testResponse,
	}
}

func voiceForm() url.Values {
	return url.Values{
		"CallSid": {"CA111"},
		"From":    {"+14158675310"},
		"To":      {"+18005551212"},
	}
}

// signedRequest builds a POST signed the way Twilio would sign it for the public URL.
func signedRequest(target string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilio.Signature(testToken, testPublicURL+target, form))
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookPublishesSignedVoiceEvent(t *testing.T) {
	pub := &fakePublisher{}

	rec := serve(newTestWebhook(envelope.Voice, pub), signedRequest("/webhooks/voice", voiceForm()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/xml" {
		t.Errorf("Content-Type = %q, want text/xml", ct)
	}
	if rec.Body.String() != testResponse {
		t.Errorf("body = %q, want %q", rec.Body.String(), testResponse)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.published))
	}
	env := pub.published[0]
	if env.Channel != envelope.Voice || env.ProviderID != "CA111" || !env.ReceivedAt.Equal(testNow) {
		t.Errorf("published envelope = %+v", env)
	}
}

func TestWebhookPublishesSignedSMSEvent(t *testing.T) {
	pub := &fakePublisher{}
	form := url.Values{"MessageSid": {"SM111"}, "From": {"+14158675310"}, "To": {"+18005551212"}, "Body": {"hi"}}

	rec := serve(newTestWebhook(envelope.SMS, pub), signedRequest("/webhooks/sms", form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if len(pub.published) != 1 || pub.published[0].Channel != envelope.SMS || pub.published[0].ProviderID != "SM111" {
		t.Errorf("published = %+v, want one SMS event SM111", pub.published)
	}
}

// Twilio signs the full URL including any query string, but only POST fields are
// parameters: the query must not leak into the event's params.
func TestWebhookSignsURLWithQueryButStoresOnlyPostParams(t *testing.T) {
	pub := &fakePublisher{}

	rec := serve(newTestWebhook(envelope.Voice, pub), signedRequest("/webhooks/voice?campaign=fall", voiceForm()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.published))
	}
	if _, ok := pub.published[0].Params["campaign"]; ok {
		t.Errorf("query parameter leaked into params: %v", pub.published[0].Params)
	}
}

func TestWebhookRejectsBadRequests(t *testing.T) {
	badSignature := signedRequest("/webhooks/voice", voiceForm())
	badSignature.Header.Set("X-Twilio-Signature", "bm90LWEtc2lnbmF0dXJl")

	noSignature := signedRequest("/webhooks/voice", voiceForm())
	noSignature.Header.Del("X-Twilio-Signature")

	tamperedForm := voiceForm()
	tampered := signedRequest("/webhooks/voice", voiceForm())
	tamperedForm.Set("To", "+19999999999")
	tampered.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tamperedForm.Encode())).Body

	tooLargeForm := voiceForm()
	tooLargeForm.Set("Padding", strings.Repeat("x", 70*1024))
	tooLarge := signedRequest("/webhooks/voice", tooLargeForm)

	missingSid := voiceForm()
	missingSid.Del("CallSid")
	unprocessable := signedRequest("/webhooks/voice", missingSid)

	tests := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"bad signature", badSignature, http.StatusForbidden},
		{"missing signature", noSignature, http.StatusForbidden},
		{"tampered body", tampered, http.StatusForbidden},
		{"body over 64 KB", tooLarge, http.StatusRequestEntityTooLarge},
		{"signed but missing CallSid", unprocessable, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := &fakePublisher{}

			rec := serve(newTestWebhook(envelope.Voice, pub), tt.req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if len(pub.published) != 0 {
				t.Errorf("published %d events, want none", len(pub.published))
			}
		})
	}
}

// If the event can't be stored, Twilio must not get a 200: a 503 makes it retry, and
// stream deduplication makes the retry safe.
func TestWebhookReturns503WhenPublishFails(t *testing.T) {
	pub := &fakePublisher{err: errors.New("no stream response")}

	rec := serve(newTestWebhook(envelope.Voice, pub), signedRequest("/webhooks/voice", voiceForm()))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<Response") {
		t.Errorf("body = %q, want no TwiML on failure", rec.Body.String())
	}
}

// A stalled stream must not hold webhook requests open indefinitely.
func TestWebhookPublishesWithDeadline(t *testing.T) {
	pub := &fakePublisher{}

	serve(newTestWebhook(envelope.Voice, pub), signedRequest("/webhooks/voice", voiceForm()))

	if !pub.hadDeadline {
		t.Error("Publish context has no deadline")
	}
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

type part struct {
	name, filename string
	data           []byte
}

func emailParts(messageID string) []part {
	return []part{
		{name: "from", data: []byte("Jordan Lee <jordan@example.net>")},
		{name: "to", data: []byte("search-fall@leads.example.com")},
		{name: "subject", data: []byte("Quote for a new roof?")},
		{name: "text", data: []byte("What's the price?")},
		{name: "headers", data: []byte("Message-ID: " + messageID + "\nSubject: Quote for a new roof?\n")},
		{name: "envelope", data: []byte(`{"to":["search-fall@leads.example.com"],"from":"jordan@example.net"}`)},
	}
}

// multipartBody encodes parts the way Inbound Parse posts them.
func multipartBody(t *testing.T, parts []part) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var (
			pw  io.Writer
			err error
		)
		if p.filename != "" {
			pw, err = w.CreateFormFile(p.name, p.filename)
		} else {
			pw, err = w.CreateFormField(p.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func newTestEmailHook(t *testing.T, pub publisher) (*emailWebhook, *sendgrid.Signer) {
	t.Helper()
	signer, err := sendgrid.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	key, err := sendgrid.ParsePublicKey(signer.PublicKeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	return &emailWebhook{key: key, pub: pub, now: func() time.Time { return testNow }, maxBytes: maxEmailBytes}, signer
}

func signedEmail(t *testing.T, signer *sendgrid.Signer, body []byte, contentType string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(sendgrid.HeaderTimestamp, "1761566400")
	req.Header.Set(sendgrid.HeaderSignature, signer.Sign("1761566400", body))
	return req
}

func TestEmailIsVerifiedAndPublished(t *testing.T) {
	pub := &fakePublisher{}
	h, signer := newTestEmailHook(t, pub)
	parts := append(emailParts("<m1@example.net>"),
		part{name: "attachment1", filename: "roof.jpg", data: []byte("jpeg bytes")},
		part{name: "attachment2", filename: "plan.pdf", data: []byte("pdf bytes")})
	body, ct := multipartBody(t, parts)

	rec := serve(h, signedEmail(t, signer, body, ct))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.published))
	}
	env := pub.published[0]
	if env.Channel != envelope.Email || env.ProviderID != "<m1@example.net>" ||
		env.To != "search-fall@leads.example.com" || env.AttachmentCount != 2 || !pub.hadDeadline {
		t.Errorf("published %+v (deadline %v)", env, pub.hadDeadline)
	}
	if strings.Contains(fmt.Sprint(env.Params), "jpeg bytes") {
		t.Error("attachment content reached the event")
	}
}

func TestEmailWithBadSignatureIsRejected(t *testing.T) {
	pub := &fakePublisher{}
	h, signer := newTestEmailHook(t, pub)
	body, ct := multipartBody(t, emailParts("<m1@example.net>"))
	req := signedEmail(t, signer, body, ct)
	tampered := bytes.Replace(body, []byte("roof"), []byte("ROOF"), 1)
	req.Body = io.NopCloser(bytes.NewReader(tampered))

	rec := serve(h, req)
	if rec.Code != http.StatusForbidden || len(pub.published) != 0 {
		t.Errorf("status = %d, published %d; want 403 and nothing published", rec.Code, len(pub.published))
	}

	req = signedEmail(t, signer, body, ct)
	req.Header.Del(sendgrid.HeaderSignature)
	if rec := serve(h, req); rec.Code != http.StatusForbidden {
		t.Errorf("missing signature: status = %d, want 403", rec.Code)
	}
}

func TestUnusableEmailIsRejected(t *testing.T) {
	h, signer := newTestEmailHook(t, &fakePublisher{})
	parts := emailParts("<m1@example.net>")
	parts[4].data = []byte("Subject: no id here\n")
	body, ct := multipartBody(t, parts)
	if rec := serve(h, signedEmail(t, signer, body, ct)); rec.Code != http.StatusBadRequest {
		t.Errorf("no Message-ID: status = %d, want 400", rec.Code)
	}
	if rec := serve(h, signedEmail(t, signer, []byte("a=b"), "application/x-www-form-urlencoded")); rec.Code != http.StatusBadRequest {
		t.Errorf("not multipart: status = %d, want 400", rec.Code)
	}
}

func TestEmailOverTheSizeLimitIsRejected(t *testing.T) {
	if maxEmailBytes != 30<<20 {
		t.Errorf("maxEmailBytes = %d, want SendGrid's 30 MB limit", maxEmailBytes)
	}
	pub := &fakePublisher{}
	h, signer := newTestEmailHook(t, pub)
	h.maxBytes = 4 << 10
	parts := append(emailParts("<m1@example.net>"), part{name: "attachment1", filename: "big.bin", data: make([]byte, 8<<10)})
	body, ct := multipartBody(t, parts)
	if rec := serve(h, signedEmail(t, signer, body, ct)); rec.Code != http.StatusRequestEntityTooLarge || len(pub.published) != 0 {
		t.Errorf("status = %d, published %d; want 413 and nothing published", rec.Code, len(pub.published))
	}
}

// A large attachment is hashed and discarded as it streams, never held in memory.
func TestLargeAttachmentIsNotBuffered(t *testing.T) {
	pub := &fakePublisher{}
	h, signer := newTestEmailHook(t, pub)
	const size = 16 << 20
	parts := append(emailParts("<m1@example.net>"), part{name: "attachment1", filename: "scan.pdf", data: make([]byte, size)})
	body, ct := multipartBody(t, parts)
	req := signedEmail(t, signer, body, ct)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec := serve(h, req)
	runtime.ReadMemStats(&after)

	if rec.Code != http.StatusOK || len(pub.published) != 1 {
		t.Fatalf("status = %d, published %d; want 200 and 1", rec.Code, len(pub.published))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Errorf("handling a %d MB attachment allocated %d KB, want it streamed (< 2 MB)", size>>20, allocated>>10)
	}
}

func TestLongTextFieldIsTruncated(t *testing.T) {
	pub := &fakePublisher{}
	h, signer := newTestEmailHook(t, pub)
	parts := emailParts("<m1@example.net>")
	parts[3].data = bytes.Repeat([]byte("a"), maxEmailFieldBytes+100)
	body, ct := multipartBody(t, parts)
	if rec := serve(h, signedEmail(t, signer, body, ct)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := len(pub.published[0].Params.Get("text")); got != maxEmailFieldBytes {
		t.Errorf("text kept %d bytes, want %d", got, maxEmailFieldBytes)
	}
}

func TestEmailPublishFailureIsRetryable(t *testing.T) {
	h, signer := newTestEmailHook(t, &fakePublisher{err: errors.New("stream down")})
	body, ct := multipartBody(t, emailParts("<m1@example.net>"))
	if rec := serve(h, signedEmail(t, signer, body, ct)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so SendGrid retries", rec.Code)
	}
}

// Without SENDGRID_PUBLIC_KEY the email route doesn't exist: no key, no unverified email.
func TestEmailRouteNeedsAKey(t *testing.T) {
	cfg := config{authToken: testToken, publicURL: testPublicURL}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/inbound", strings.NewReader(""))
	if rec := serve(routes(cfg, &fakePublisher{}), req); rec.Code != http.StatusNotFound {
		t.Errorf("without a key: status = %d, want 404", rec.Code)
	}
	signer, _ := sendgrid.NewSigner()
	cfg.sendgridKey, _ = sendgrid.ParsePublicKey(signer.PublicKeyBase64())
	req = httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/inbound", strings.NewReader(""))
	if rec := serve(routes(cfg, &fakePublisher{}), req); rec.Code == http.StatusNotFound {
		t.Error("with a key: route missing")
	}
}

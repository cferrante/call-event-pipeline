package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

func emailOptions(t *testing.T) (options, *sendgrid.Signer) {
	t.Helper()
	s, err := sendgrid.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	o := baseOptions()
	o.channel, o.from, o.to, o.body = "email", "jordan@example.net", "search-fall@leads.example.com", "What's the price?"
	o.subject, o.signer = "Quote?", s
	return o, s
}

// fieldsOf reads a multipart body back: text fields, and the number of file parts.
func fieldsOf(t *testing.T, contentType string, body []byte) (map[string]string, int) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	fields, files := map[string]string{}, 0
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return fields, files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			files++
		} else {
			fields[p.FormName()] = string(b)
		}
	}
}

func TestBuildEmailRequestIsSignedLikeInboundParse(t *testing.T) {
	o, signer := emailOptions(t)
	o.attachBytes = 1024
	req, err := buildRequest(o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.URL.String(); got != "http://127.0.0.1:8081/webhooks/sendgrid/inbound" {
		t.Errorf("URL = %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	key, _ := sendgrid.ParsePublicKey(signer.PublicKeyBase64())
	if err := sendgrid.Verify(key, req.Header.Get(sendgrid.HeaderSignature), req.Header.Get(sendgrid.HeaderTimestamp), body); err != nil {
		t.Errorf("signature: %v", err)
	}
	fields, files := fieldsOf(t, req.Header.Get("Content-Type"), body)
	if fields["subject"] != "Quote?" || fields["text"] != "What's the price?" ||
		!strings.Contains(fields["headers"], "Message-ID: <") ||
		!strings.Contains(fields["envelope"], `"to":["search-fall@leads.example.com"]`) || files != 1 {
		t.Errorf("fields = %v, files = %d", fields, files)
	}
}

func TestBuildEmailRequestWithBadSignatureFailsVerification(t *testing.T) {
	o, signer := emailOptions(t)
	o.badSignature = true
	req, err := buildRequest(o)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	key, _ := sendgrid.ParsePublicKey(signer.PublicKeyBase64())
	if err := sendgrid.Verify(key, req.Header.Get(sendgrid.HeaderSignature), req.Header.Get(sendgrid.HeaderTimestamp), body); err == nil {
		t.Error("bad-signature request verified")
	}
}

func TestBuildEmailRequestReusesAnID(t *testing.T) {
	o, _ := emailOptions(t)
	o.id = "<fixed@example.net>"
	req, err := buildRequest(o)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	fields, _ := fieldsOf(t, req.Header.Get("Content-Type"), body)
	if !strings.Contains(fields["headers"], "Message-ID: <fixed@example.net>") {
		t.Errorf("headers = %q, want the given Message-ID", fields["headers"])
	}
}

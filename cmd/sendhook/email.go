package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

// buildEmailRequest creates a SendGrid Inbound Parse POST (default, non-raw format) and signs
// it as SendGrid's security policy does, with the simulator's private key.
func buildEmailRequest(o options) (*http.Request, error) {
	if o.signer == nil {
		return nil, fmt.Errorf("SENDGRID_SIMULATOR_PRIVATE_KEY must be set; run: set -a; source .env; set +a")
	}
	id := o.id
	if id == "" {
		id = "<" + randomHex(16) + "@sendhook.example>"
	}
	fields := [][2]string{
		{"from", o.from},
		{"to", o.to},
		{"subject", o.subject},
		{"text", o.body},
		{"headers", "Message-ID: " + id + "\nFrom: " + o.from + "\nTo: " + o.to + "\nSubject: " + o.subject + "\n"},
		{"envelope", fmt.Sprintf(`{"to":[%q],"from":%q}`, o.to, o.from)},
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	if o.attachBytes > 0 {
		fw, err := w.CreateFormFile("attachment1", "attachment.bin")
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(bytes.Repeat([]byte{'x'}, o.attachBytes)); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	signer := o.signer
	if o.badSignature {
		var err error
		if signer, err = sendgrid.NewSigner(); err != nil { // a key ingest doesn't trust
			return nil, err
		}
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := buf.Bytes()
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(o.target, "/")+"/webhooks/sendgrid/inbound", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set(sendgrid.HeaderTimestamp, ts)
	req.Header.Set(sendgrid.HeaderSignature, signer.Sign(ts, body))
	return req, nil
}

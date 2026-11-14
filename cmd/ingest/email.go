package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

const (
	// maxEmailBytes is SendGrid's own limit for an inbound email with attachments; anything
	// smaller would reject, and so lose, a lead who attached a file.
	maxEmailBytes = 30 << 20
	// maxEmailFieldBytes caps each text field kept in the event.
	maxEmailFieldBytes = 64 << 10
	// emailTimeout replaces the server's 10-second read and 15-second write limits for this
	// route only: 30 MB needs longer on a slow link, while calls and SMS keep the tight limits.
	emailTimeout = 60 * time.Second
)

// emailWebhook is the ingest edge for SendGrid Inbound Parse (docs/design.md, section 5.1).
// It streams the body: every byte feeds the signature check, text fields are kept up to a
// cap, and attachments are counted and discarded without being buffered.
type emailWebhook struct {
	key      *ecdsa.PublicKey
	pub      publisher
	now      func() time.Time
	maxBytes int64
	// timeout replaces the server's read and write limits for this route; 0 keeps them.
	timeout time.Duration
}

func (h *emailWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Before reading anything: no signature means no point reading 30 MB.
	sig, ts := r.Header.Get(sendgrid.HeaderSignature), r.Header.Get(sendgrid.HeaderTimestamp)
	if sig == "" || ts == "" {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		http.Error(w, "want multipart/form-data", http.StatusBadRequest)
		return
	}
	// Both deadlines: the server's write deadline runs from the request headers, so moving
	// only the read deadline would let a slow upload be published and then lose its 200.
	// Errors here only mean the server doesn't support deadlines (e.g. a test recorder).
	if h.timeout > 0 {
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(h.timeout))
		_ = rc.SetWriteDeadline(time.Now().Add(h.timeout))
	}

	verifier := sendgrid.NewStreamVerifier(h.key, ts)
	body := io.TeeReader(http.MaxBytesReader(w, r.Body, h.maxBytes), verifier)
	fields, attachments, err := readParts(multipart.NewReader(body, params["boundary"]))
	if err == nil {
		// Whatever follows the closing boundary is signed too.
		_, err = io.Copy(io.Discard, body)
	}
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid multipart body", http.StatusBadRequest)
		return
	}
	// Only now has the whole body been hashed; nothing above trusted its content.
	if err := verifier.Verify(sig); err != nil {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	env, err := envelope.FromInboundEmail(fields, attachments, h.now())
	if err != nil {
		// Signed by SendGrid but unusable; a retry would fail the same way.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), publishTimeout)
	defer cancel()
	if err := h.pub.Publish(ctx, env); err != nil {
		slog.Error("publish failed", "channel", env.Channel, "err", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// readParts reads every part: text fields up to maxEmailFieldBytes each (the rest is
// discarded), and file parts counted and discarded. Discarded bytes are still read, so the
// signature hash sees the whole body.
func readParts(mr *multipart.Reader) (url.Values, int, error) {
	fields := url.Values{}
	attachments := 0
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			return fields, attachments, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if p.FileName() != "" {
			attachments++
		} else if name := p.FormName(); name != "" {
			v, err := io.ReadAll(io.LimitReader(p, maxEmailFieldBytes))
			if err != nil {
				return nil, 0, err
			}
			fields.Add(name, string(v))
		}
		if _, err := io.Copy(io.Discard, p); err != nil {
			return nil, 0, err
		}
	}
}

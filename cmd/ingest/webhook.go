package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/twilio"
)

const (
	// maxBodyBytes caps webhook bodies; real Twilio webhooks are a few KB.
	maxBodyBytes = 64 << 10
	// publishTimeout bounds how long a webhook waits for the stream to store its event,
	// well inside Twilio's 15-second webhook timeout.
	publishTimeout = 5 * time.Second
)

// publisher stores an event durably; a nil error means it is safe to acknowledge.
type publisher interface {
	Publish(ctx context.Context, env envelope.Envelope) error
}

// webhook is the ingest edge for one channel: verify, store, acknowledge. It never
// waits on attribution or AI, so its latency depends only on the stream.
type webhook struct {
	channel   envelope.Channel
	authToken string
	// publicURL is the scheme and host Twilio calls (e.g. https://hooks.example.com).
	// Signatures cover that URL, which a proxy or tunnel hides from the request itself.
	publicURL string
	pub       publisher
	now       func() time.Time
	// response is the TwiML returned once the event is stored.
	response string
}

func (h *webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid form body", http.StatusBadRequest)
		return
	}

	// Twilio signs the full public URL (including any query string) plus the POST fields.
	signedURL := h.publicURL + r.URL.RequestURI()
	if !twilio.Valid(h.authToken, signedURL, r.PostForm, r.Header.Get("X-Twilio-Signature")) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	env, err := envelope.FromWebhook(h.channel, r.PostForm, h.now())
	if err != nil {
		// Signed by Twilio but unusable; a retry would fail the same way.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), publishTimeout)
	defer cancel()
	if err := h.pub.Publish(ctx, env); err != nil {
		slog.Error("publish failed", "channel", env.Channel, "provider_id", env.ProviderID, "err", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(h.response))
}

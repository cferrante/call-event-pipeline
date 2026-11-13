package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"sync"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

// keepMessages is how many recent messages the outbox holds; older ones are dropped.
const keepMessages = 100

// maxBody caps a request. SendGrid allows 30 MB with attachments; alerts are plain text.
const maxBody = 1 << 20

// message is one accepted email, as the outbox lists it.
type message struct {
	From, To, Subject, Text string
	Received                time.Time
}

// sink implements the part of SendGrid's Mail Send v3 API that the monitor uses, and keeps
// what it accepts instead of delivering it.
type sink struct {
	key string

	mu   sync.Mutex
	msgs []message // newest first
}

func newSink(key string) *sink { return &sink{key: key} }

func (s *sink) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/mail/send", s.send)
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.messages())
	})
	return mux
}

func (s *sink) send(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	want := "Bearer " + s.key
	if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
		writeError(w, http.StatusUnauthorized, "", "authorization required")
		return
	}
	var req sendgrid.MailRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "", "content too large")
			return
		}
		writeError(w, http.StatusBadRequest, "", "the request body is not valid JSON")
		return
	}
	if field, msg := validate(req); msg != "" {
		writeError(w, http.StatusBadRequest, field, msg)
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	for _, p := range req.Personalizations {
		for _, to := range p.To {
			s.msgs = slices.Insert(s.msgs, 0, message{From: req.From.Email, To: to.Email,
				Subject: req.Subject, Text: req.Content[0].Value, Received: now})
		}
	}
	if len(s.msgs) > keepMessages {
		s.msgs = s.msgs[:keepMessages]
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

// validate checks the documented rules for the fields we use. Templates aren't supported, so
// a subject and at least one content part are required.
func validate(req sendgrid.MailRequest) (field, msg string) {
	if n := len(req.Personalizations); n == 0 || n > 1000 {
		return "personalizations", "personalizations must have between 1 and 1000 items"
	}
	for i, p := range req.Personalizations {
		if len(p.To) == 0 {
			return fmt.Sprintf("personalizations.%d.to", i), "to must have at least one recipient"
		}
		for j, to := range p.To {
			if !validEmail(to.Email) {
				return fmt.Sprintf("personalizations.%d.to.%d.email", i, j), "does not contain a valid address"
			}
		}
	}
	if !validEmail(req.From.Email) {
		return "from.email", "the from address does not contain a valid address"
	}
	if req.Subject == "" {
		return "subject", "the subject is required"
	}
	if len(req.Content) == 0 {
		return "content", "at least one content part is required"
	}
	for i, c := range req.Content {
		if c.Type == "" || c.Value == "" {
			return fmt.Sprintf("content.%d", i), "content type and value must not be empty"
		}
	}
	return "", ""
}

func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// writeError answers in SendGrid's documented error shape.
func writeError(w http.ResponseWriter, status int, field, msg string) {
	item := sendgrid.ErrorItem{Message: msg}
	if field != "" {
		item.Field = &field
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(sendgrid.ErrorBody{Errors: []sendgrid.ErrorItem{item}})
}

func (s *sink) messages() []message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

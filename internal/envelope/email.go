package envelope

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrUnusableEmail means a signed inbound email can't become an event; retrying won't help.
var ErrUnusableEmail = errors.New("unusable inbound email")

// keptEmailFields are the Inbound Parse fields stored in params for replay, in the order they
// draw on maxKeptEmailBytes: the small fields first, then text, then headers, the largest and
// least needed. html is left out: it repeats text at several times the size. Attachments are
// never kept.
var keptEmailFields = []string{"from", "to", "cc", "subject", "envelope", "charsets", "SPF", "dkim",
	"spam_score", "text", "headers"}

const (
	// maxKeptEmailBytes is the total kept in params. JSON can encode a byte as up to 6 ('<' is
	// \u003c), so with the subject this keeps an event under about 600 KB, inside NATS's 1 MB limit.
	maxKeptEmailBytes = 96 << 10
	// maxSubjectBytes is the longest line an email may have (RFC 5322); the subject is stored
	// twice, in params and on its own.
	maxSubjectBytes = 998
	// maxSmallFieldBytes caps each kept field other than subject, text and headers, so that
	// however large they arrive they leave text most of the budget.
	maxSmallFieldBytes = 4 << 10
)

// FromInboundEmail builds an envelope from SendGrid Inbound Parse fields (the default,
// non-raw format), plus how many attachment parts the request had.
//
//   - provider_id is the Message-ID from the raw headers field: Inbound Parse has no
//     dedicated field, and it's what makes a retried delivery a duplicate.
//   - to is the SMTP envelope recipient: the tracking address the mail was delivered to,
//     which the To header may not even contain (Bcc, lists).
//   - from is the From header's address, the person who wrote; the envelope sender, often a
//     bounce address, is the fallback.
//
// Addresses are lowercased so lookups and lead matching don't depend on case.
func FromInboundEmail(fields url.Values, attachments int, receivedAt time.Time) (Envelope, error) {
	id, err := messageID(fields.Get("headers"))
	if err != nil {
		return Envelope{}, err
	}
	var smtp struct {
		To   []string `json:"to"`
		From string   `json:"from"`
	}
	if err := json.Unmarshal([]byte(fields.Get("envelope")), &smtp); err != nil {
		return Envelope{}, fmt.Errorf("%w: envelope field: %v", ErrUnusableEmail, err)
	}
	if len(smtp.To) == 0 || strings.TrimSpace(smtp.To[0]) == "" {
		return Envelope{}, fmt.Errorf("%w: no recipient in envelope", ErrUnusableEmail)
	}
	from := strings.ToLower(strings.TrimSpace(smtp.From))
	if a, err := mail.ParseAddress(fields.Get("from")); err == nil {
		from = strings.ToLower(a.Address)
	}

	// One value per field, within a total budget, so the event always fits a stream message.
	params, budget := url.Values{}, maxKeptEmailBytes
	for _, k := range keptEmailFields {
		if v, ok := fields[k]; ok && len(v) > 0 {
			limit := budget
			switch k {
			case "subject":
				limit = min(limit, maxSubjectBytes)
			case "text", "headers":
			default:
				limit = min(limit, maxSmallFieldBytes)
			}
			kept := cut(v[0], limit)
			budget -= len(kept)
			params.Set(k, kept)
		}
	}
	return Envelope{
		SchemaVersion:   SchemaVersion,
		Channel:         Email,
		Provider:        "sendgrid",
		ProviderID:      id,
		From:            from,
		To:              strings.ToLower(strings.TrimSpace(smtp.To[0])),
		ReceivedAt:      receivedAt.UTC(),
		Params:          params,
		Subject:         cut(fields.Get("subject"), maxSubjectBytes),
		AttachmentCount: attachments,
	}, nil
}

// cut returns at most n bytes of s, cut between characters.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(n, 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// messageID reads the Message-ID from raw header text.
func messageID(rawHeaders string) (string, error) {
	r := textproto.NewReader(bufio.NewReader(strings.NewReader(
		strings.TrimRight(rawHeaders, "\r\n") + "\r\n\r\n")))
	h, err := r.ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return "", fmt.Errorf("%w: headers field: %v", ErrUnusableEmail, err)
	}
	id := strings.TrimSpace(h.Get("Message-Id"))
	if id == "" {
		return "", fmt.Errorf("%w: no Message-ID header, so it couldn't be deduplicated", ErrUnusableEmail)
	}
	return id, nil
}

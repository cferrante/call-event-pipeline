package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

// MailSender sends one email; *sendgrid.Client is the real one.
type MailSender interface {
	Send(ctx context.Context, m sendgrid.Mail) error
}

// EmailAlerter sends each alert event as a plain-text email.
type EmailAlerter struct {
	Sender    MailSender
	From, To  string
	StatusURL string // the status page, for the reader to look further
}

// Alert makes one attempt; the Queue retries.
func (a EmailAlerter) Alert(ctx context.Context, e Event) error {
	name := e.Rule
	if e.Subject != "" {
		name += " (" + e.Subject + ")"
	}
	var b strings.Builder
	if e.State == Firing {
		fmt.Fprintf(&b, "Alert firing: %s\n\nSeen: %s\nSince: %s\n", name, e.Detail, e.At.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "Alert resolved: %s\n\nLast seen: %s\nResolved: %s, after %s\n", name, e.Detail,
			e.At.UTC().Format(time.RFC3339), e.Duration.Round(time.Second))
	}
	if a.StatusURL != "" {
		fmt.Fprintf(&b, "\nStatus page: %s\n", a.StatusURL)
	}
	return a.Sender.Send(ctx, sendgrid.Mail{From: a.From, To: a.To,
		Subject: "[" + strings.ToUpper(string(e.State)) + "] " + name, Text: b.String()})
}

package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

type captureSender struct{ got []sendgrid.Mail }

func (c *captureSender) Send(_ context.Context, m sendgrid.Mail) error {
	c.got = append(c.got, m)
	return nil
}

// An alert email says in its subject whether the alert started or ended and which rule, and
// in its body what was seen, when, and where to look.
func TestEmailAlerterFormatsMail(t *testing.T) {
	s := &captureSender{}
	a := EmailAlerter{Sender: s, From: "monitor@example.com", To: "oncall@example.com", StatusURL: "http://127.0.0.1:8090"}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	_ = a.Alert(context.Background(), Event{State: Firing, Rule: RuleAckedNotWritten, Subject: "attribution",
		Detail: "12 of 400 acknowledged written", At: at})
	_ = a.Alert(context.Background(), Event{State: Resolved, Rule: RuleAckedNotWritten, Subject: "attribution",
		Detail: "12 of 400 acknowledged written", At: at.Add(3 * time.Minute), Duration: 3 * time.Minute})

	if len(s.got) != 2 {
		t.Fatalf("sent %d, want 2", len(s.got))
	}
	f, r := s.got[0], s.got[1]
	if f.From != "monitor@example.com" || f.To != "oncall@example.com" {
		t.Errorf("addresses %q -> %q", f.From, f.To)
	}
	if f.Subject != "[FIRING] "+RuleAckedNotWritten+" (attribution)" || r.Subject != "[RESOLVED] "+RuleAckedNotWritten+" (attribution)" {
		t.Errorf("subjects %q, %q", f.Subject, r.Subject)
	}
	for _, want := range []string{"12 of 400 acknowledged written", "2026-11-13T09:30:00Z", "http://127.0.0.1:8090"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("firing body lacks %q:\n%s", want, f.Text)
		}
	}
	if !strings.Contains(r.Text, "3m0s") {
		t.Errorf("resolved body doesn't say how long it fired:\n%s", r.Text)
	}
}

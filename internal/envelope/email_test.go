package envelope

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"
)

var emailAt = time.Date(2026, 10, 27, 12, 0, 3, 0, time.FixedZone("PDT", -7*3600))

func inboundFields() url.Values {
	return url.Values{
		"from":     {"Jordan Lee <Jordan@Example.net>"},
		"to":       {"Sales <search-fall@leads.example.com>, someone-else@example.org"},
		"subject":  {"Quote for a new roof?"},
		"text":     {"What's the price?"},
		"html":     {"<p>What's the price?</p>"},
		"headers":  {"Received: from mx.example.net\nMessage-ID: <CABQ123@mail.example.net>\nSubject: Quote for a new roof?\n"},
		"envelope": {`{"to":["Search-Fall@leads.example.com"],"from":"bounce+123@example.net"}`},
	}
}

func TestFromInboundEmail(t *testing.T) {
	env, err := FromInboundEmail(inboundFields(), 2, emailAt)
	if err != nil {
		t.Fatalf("FromInboundEmail: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"schema_version", env.SchemaVersion, SchemaVersion},
		{"channel", env.Channel, Email},
		{"provider", env.Provider, "sendgrid"},
		{"provider_id", env.ProviderID, "<CABQ123@mail.example.net>"},
		// The person who wrote, from the From header (lowercased), not the bounce address.
		{"from", env.From, "jordan@example.net"},
		// The SMTP recipient: the tracking address the mail was actually delivered to.
		{"to", env.To, "search-fall@leads.example.com"},
		{"received_at", env.ReceivedAt, emailAt.UTC()},
		{"subject", env.Subject, "Quote for a new roof?"},
		{"attachment_count", env.AttachmentCount, 2},
		{"text kept", env.Params.Get("text"), "What's the price?"},
		// html repeats text at several times the size, so it isn't kept.
		{"html dropped", env.Params.Has("html"), false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestFromInboundEmailFallsBackToTheEnvelopeSender(t *testing.T) {
	f := inboundFields()
	f.Set("from", "not an address")
	env, err := FromInboundEmail(f, 0, emailAt)
	if err != nil {
		t.Fatalf("FromInboundEmail: %v", err)
	}
	if env.From != "bounce+123@example.net" {
		t.Errorf("From = %q, want the SMTP envelope sender", env.From)
	}
}

func TestFromInboundEmailRejectsUnusableInput(t *testing.T) {
	cases := map[string]func(url.Values){
		"no Message-ID":     func(f url.Values) { f.Set("headers", "Subject: hi\n") },
		"no headers":        func(f url.Values) { f.Del("headers") },
		"no recipient":      func(f url.Values) { f.Set("envelope", `{"to":[],"from":"a@example.net"}`) },
		"envelope not JSON": func(f url.Values) { f.Set("envelope", "nope") },
	}
	for name, change := range cases {
		f := inboundFields()
		change(f)
		if _, err := FromInboundEmail(f, 0, emailAt); !errors.Is(err, ErrUnusableEmail) {
			t.Errorf("%s: err = %v, want ErrUnusableEmail", name, err)
		}
	}
}

func TestInboundEmailRoundTripsThroughParse(t *testing.T) {
	env, err := FromInboundEmail(inboundFields(), 1, emailAt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Subject != env.Subject || got.AttachmentCount != 1 || got.Channel != Email {
		t.Errorf("round trip = %+v", got)
	}
}

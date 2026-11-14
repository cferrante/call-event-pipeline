package envelope

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// The event must fit a NATS message (1 MB by default), or publishing it fails and SendGrid
// retries the same email for three days until the lead is lost. The edge caps each field at
// 64 KB, but a field may be repeated, and JSON encodes '<', '>' and '&' as 6 bytes each, so
// the kept fields have a total budget too. Small fields come first, then text, then headers.
func TestInboundEmailEventFitsAStreamMessage(t *testing.T) {
	const natsMaxPayload = 1 << 20
	quoted := strings.Repeat("<>&", 64<<10/3) // 64 KB that JSON encodes at 6 bytes a byte
	f := inboundFields()
	f.Set("subject", quoted)
	f["text"] = []string{"Can I get a quote? " + quoted, quoted, quoted}
	f["headers"] = []string{f.Get("headers") + "X-Quote: " + quoted, quoted}
	for _, k := range []string{"cc", "charsets", "SPF", "dkim", "spam_score"} {
		f[k] = []string{quoted, quoted}
	}
	env, err := FromInboundEmail(f, 0, emailAt)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > natsMaxPayload*3/4 {
		t.Errorf("event is %d KB, want under %d KB (three quarters of NATS's limit)", len(data)>>10, natsMaxPayload*3/4>>10)
	}
	for k, v := range env.Params {
		if len(v) != 1 {
			t.Errorf("params[%s] keeps %d values, want 1", k, len(v))
		}
	}
	if !strings.HasPrefix(env.Params.Get("text"), "Can I get a quote?") || env.Params.Get("envelope") != f.Get("envelope") {
		t.Errorf("the start of text or the whole envelope field was lost: text %.30q, envelope %q",
			env.Params.Get("text"), env.Params.Get("envelope"))
	}
	if !utf8.ValidString(env.Subject) || len(env.Subject) > 1000 {
		t.Errorf("subject is %d bytes (valid UTF-8: %v), want at most 1,000", len(env.Subject), utf8.ValidString(env.Subject))
	}
}

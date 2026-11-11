package main

import (
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/twilio"
)

func baseOptions() options {
	return options{
		channel:   "voice",
		from:      "+14158675310",
		to:        "+18005550001",
		target:    "http://127.0.0.1:8081",
		publicURL: "https://hooks.example.com",
		token:     "secret-token",
	}
}

// formOf reads the request body back as form values.
func formOf(t *testing.T, body io.Reader) url.Values {
	t.Helper()
	if body == nil {
		t.Fatal("request has no body")
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	return form
}

// The request goes to the local target, but is signed for the public URL, as Twilio would.
func TestBuildRequestIsSignedForThePublicURL(t *testing.T) {
	req, err := buildRequest(baseOptions())
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	if got := req.URL.String(); got != "http://127.0.0.1:8081/webhooks/voice" {
		t.Errorf("URL = %q, want the local target", got)
	}
	form := formOf(t, req.Body)
	sig := req.Header.Get("X-Twilio-Signature")
	if !twilio.Valid("secret-token", "https://hooks.example.com/webhooks/voice", form, sig) {
		t.Error("signature does not verify against the public URL")
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestBuildRequestFillsChannelFields(t *testing.T) {
	voice, err := buildRequest(baseOptions())
	if err != nil {
		t.Fatalf("voice: %v", err)
	}
	vf := formOf(t, voice.Body)
	if !strings.HasPrefix(vf.Get("CallSid"), "CA") || vf.Get("From") != "+14158675310" || vf.Get("To") != "+18005550001" {
		t.Errorf("voice form = %v", vf)
	}

	o := baseOptions()
	o.channel, o.body = "sms", "Hi there"
	sms, err := buildRequest(o)
	if err != nil {
		t.Fatalf("sms: %v", err)
	}
	sf := formOf(t, sms.Body)
	if !strings.HasPrefix(sf.Get("MessageSid"), "SM") || sf.Get("Body") != "Hi there" {
		t.Errorf("sms form = %v", sf)
	}
	if sms.URL.Path != "/webhooks/sms" {
		t.Errorf("sms path = %q", sms.URL.Path)
	}
}

// Without -id, each run gets a fresh ID so repeated sends aren't deduplicated; with -id,
// the same ID can be sent twice on purpose to demonstrate deduplication.
func TestBuildRequestIDs(t *testing.T) {
	a, _ := buildRequest(baseOptions())
	b, _ := buildRequest(baseOptions())
	if formOf(t, a.Body).Get("CallSid") == formOf(t, b.Body).Get("CallSid") {
		t.Error("two runs without -id produced the same CallSid")
	}

	o := baseOptions()
	o.id = "CAfixed"
	req, _ := buildRequest(o)
	if got := formOf(t, req.Body).Get("CallSid"); got != "CAfixed" {
		t.Errorf("CallSid = %q, want CAfixed", got)
	}
}

func TestBuildRequestBadSignature(t *testing.T) {
	o := baseOptions()
	o.badSignature = true
	req, err := buildRequest(o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	form := formOf(t, req.Body)
	if twilio.Valid("secret-token", "https://hooks.example.com/webhooks/voice", form, req.Header.Get("X-Twilio-Signature")) {
		t.Error("-bad-signature produced a valid signature")
	}
}

func TestBuildRequestRejectsUnknownChannel(t *testing.T) {
	o := baseOptions()
	o.channel = "fax"
	if _, err := buildRequest(o); err == nil {
		t.Error("want error for channel fax")
	}
}

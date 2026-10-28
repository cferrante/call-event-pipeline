package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"testing"
)

// The worked example from https://www.twilio.com/docs/usage/security,
// independently confirmed with: printf '%s' "<string to sign>" | openssl dgst -sha1 -hmac 12345 -binary | base64
const (
	exampleToken     = "12345"
	exampleURL       = "https://example.com/myapp.php?foo=1&bar=2"
	exampleSignature = "L/OH5YylLD5NRKLltdqwSvS0BnU="
)

func exampleParams() url.Values {
	return url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Caller":  {"+14158675310"},
		"Digits":  {"1234"},
		"From":    {"+14158675310"},
		"To":      {"+18005551212"},
	}
}

func TestSignatureMatchesTwilioExample(t *testing.T) {
	if got := Signature(exampleToken, exampleURL, exampleParams()); got != exampleSignature {
		t.Errorf("Signature = %q, want %q", got, exampleSignature)
	}
}

// Twilio sorts parameter names with case-sensitive byte ordering, so "B" comes before "a".
func TestSignatureSortsParamsCaseSensitively(t *testing.T) {
	params := url.Values{"b": {"2"}, "a": {"3"}, "B": {"1"}}

	mac := hmac.New(sha1.New, []byte("token"))
	mac.Write([]byte("https://example.com/hookB1a3b2"))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got := Signature("token", "https://example.com/hook", params); got != want {
		t.Errorf("Signature = %q, want %q", got, want)
	}
}

func TestValidAcceptsTwilioExample(t *testing.T) {
	if !Valid(exampleToken, exampleURL, exampleParams(), exampleSignature) {
		t.Error("Valid = false, want true")
	}
}

func TestValidRejectsTampering(t *testing.T) {
	tampered := exampleParams()
	tampered.Set("To", "+18005559999")

	extra := exampleParams()
	extra.Set("Body", "injected")

	tests := []struct {
		name      string
		token     string
		url       string
		params    url.Values
		signature string
	}{
		{"tampered param", exampleToken, exampleURL, tampered, exampleSignature},
		{"extra param", exampleToken, exampleURL, extra, exampleSignature},
		{"wrong token", "54321", exampleURL, exampleParams(), exampleSignature},
		{"missing signature", exampleToken, exampleURL, exampleParams(), ""},
		{"different scheme", exampleToken, "http://example.com/myapp.php?foo=1&bar=2", exampleParams(), exampleSignature},
		{"different query", exampleToken, "https://example.com/myapp.php?foo=1&bar=3", exampleParams(), exampleSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if Valid(tt.token, tt.url, tt.params, tt.signature) {
				t.Error("Valid = true, want false")
			}
		})
	}
}

// Twilio signs voice-over-HTTPS URLs without the port but keeps it for SMS, so the
// request URL we rebuild may differ from the signed one only by an explicit default port.
func TestValidToleratesExplicitDefaultPort(t *testing.T) {
	tests := []struct {
		name, signedURL, requestURL string
	}{
		{"signed without port, request has :443", "https://example.com/hook", "https://example.com:443/hook"},
		{"signed with :443, request has none", "https://example.com:443/hook", "https://example.com/hook"},
		{"signed with :80, request has none", "http://example.com:80/hook", "http://example.com/hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := Signature(exampleToken, tt.signedURL, exampleParams())
			if !Valid(exampleToken, tt.requestURL, exampleParams(), sig) {
				t.Error("Valid = false, want true")
			}
		})
	}
}

func TestValidRejectsDifferentNonDefaultPort(t *testing.T) {
	sig := Signature(exampleToken, "https://example.com:8443/hook", exampleParams())
	if Valid(exampleToken, "https://example.com:9443/hook", exampleParams(), sig) {
		t.Error("Valid = true, want false")
	}
}

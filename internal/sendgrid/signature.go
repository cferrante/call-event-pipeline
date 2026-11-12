// Package sendgrid implements the parts of SendGrid's contracts this pipeline uses: verifying
// Inbound Parse webhook signatures, and (from step 9) sending mail through the Mail Send API.
package sendgrid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
)

// Header names SendGrid uses for signed webhooks (Event Webhook and Inbound Parse).
const (
	HeaderSignature = "X-Twilio-Email-Event-Webhook-Signature"
	HeaderTimestamp = "X-Twilio-Email-Event-Webhook-Timestamp"
)

// ErrBadSignature means a request wasn't signed by the holder of SendGrid's private key.
var ErrBadSignature = errors.New("sendgrid: bad signature")

// ParsePublicKey decodes the verification key SendGrid returns for a security policy.
// It is base64 of a DER-encoded SubjectPublicKeyInfo, and must be a P-256 key: SendGrid's
// docs don't name the curve, but their example key's encoding identifies P-256.
func ParsePublicKey(b64 string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("sendgrid public key: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("sendgrid public key: %w", err)
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("sendgrid public key: not an ECDSA P-256 key")
	}
	return key, nil
}

// Verify checks a signature over timestamp and body: an ECDSA signature (base64 of ASN.1)
// over SHA-256 of the timestamp header's bytes followed by the raw body, with no separator.
// Verify before parsing or modifying the body in any way.
//
// There is deliberately no check on the timestamp's age. SendGrid documents no tolerance
// and doesn't say whether a retried delivery is re-signed, so a freshness window could
// reject a legitimate retry and lose a lead. A replayed request can only resend an email
// that was already accepted, and Message-ID deduplication makes that a no-op.
func Verify(key *ecdsa.PublicKey, signature, timestamp string, body []byte) error {
	v := NewStreamVerifier(key, timestamp)
	v.Write(body) // a hash.Hash never returns an error
	return v.Verify(signature)
}

// StreamVerifier verifies a body that arrives in pieces, so a large request (Inbound Parse
// allows 30 MB with attachments) never has to be held in memory. Write the raw body to it,
// exactly as received, then call Verify.
type StreamVerifier struct {
	key       *ecdsa.PublicKey
	timestamp string
	h         hash.Hash
}

// NewStreamVerifier starts verifying a body signed with timestamp.
func NewStreamVerifier(key *ecdsa.PublicKey, timestamp string) *StreamVerifier {
	h := sha256.New()
	h.Write([]byte(timestamp))
	return &StreamVerifier{key: key, timestamp: timestamp, h: h}
}

// Write adds raw body bytes. It never fails.
func (v *StreamVerifier) Write(p []byte) (int, error) { return v.h.Write(p) }

// Verify checks the signature over the timestamp and everything written.
func (v *StreamVerifier) Verify(signature string) error {
	if signature == "" || v.timestamp == "" {
		return fmt.Errorf("%w: missing signature or timestamp", ErrBadSignature)
	}
	if !allDigits(v.timestamp) {
		// The signed bytes are timestamp+body with no separator; digits only means no body
		// bytes can be moved into the timestamp.
		return fmt.Errorf("%w: timestamp is not a decimal number", ErrBadSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64", ErrBadSignature)
	}
	if !ecdsa.VerifyASN1(v.key, v.h.Sum(nil), sig) {
		return ErrBadSignature
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

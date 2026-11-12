package sendgrid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
)

// Signer plays SendGrid's side of a signed webhook, for tests and for sendhook while
// SendGrid is simulated: it holds a P-256 private key and signs as SendGrid documents.
type Signer struct {
	priv *ecdsa.PrivateKey
}

// NewSigner generates a fresh key pair.
func NewSigner() (*Signer, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Signer{priv: priv}, nil
}

// PublicKeyBase64 returns the verification key in the form SendGrid publishes it: base64 of
// the DER-encoded SubjectPublicKeyInfo.
func (s *Signer) PublicKeyBase64() string {
	der, err := x509.MarshalPKIXPublicKey(&s.priv.PublicKey)
	if err != nil {
		panic(err) // can't fail for a P-256 key
	}
	return base64.StdEncoding.EncodeToString(der)
}

// Sign returns the base64 ASN.1 signature of SHA-256(timestamp + body).
func (s *Signer) Sign(timestamp string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write(body)
	sig, err := ecdsa.SignASN1(rand.Reader, s.priv, h.Sum(nil))
	if err != nil {
		panic(err) // only fails if the random source does
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// ParsePrivateKey loads a P-256 private key from base64 PKCS#8 DER, as kept in .env for the
// simulator.
func ParsePrivateKey(b64 string) (*Signer, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("sendgrid private key: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("sendgrid private key: %w", err)
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, errors.New("sendgrid private key: not an ECDSA P-256 key")
	}
	return &Signer{priv: priv}, nil
}

package sendgrid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"testing"
)

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func publicKey(t *testing.T, s *Signer) *ecdsa.PublicKey {
	t.Helper()
	key, err := ParsePublicKey(s.PublicKeyBase64())
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return key
}

func TestValidSignatureIsAccepted(t *testing.T) {
	s := newSigner(t)
	body := []byte("--x\r\nContent-Disposition: form-data; name=\"subject\"\r\n\r\nQuote please\r\n--x--\r\n")
	sig := s.Sign("1761566400", body)
	if err := Verify(publicKey(t, s), sig, "1761566400", body); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestInvalidSignaturesAreRejected(t *testing.T) {
	s, other := newSigner(t), newSigner(t)
	key := publicKey(t, s)
	body := []byte("subject=Quote please")
	sig := s.Sign("1761566400", body)

	cases := map[string]struct {
		key            *ecdsa.PublicKey
		sig, timestamp string
		body           []byte
	}{
		"tampered body":      {key, sig, "1761566400", []byte("subject=Quote pleasE")},
		"tampered timestamp": {key, sig, "1761566401", body},
		"signed by another":  {key, other.Sign("1761566400", body), "1761566400", body},
		"missing signature":  {key, "", "1761566400", body},
		"not base64":         {key, "%%%", "1761566400", body},
		"not ASN.1":          {key, base64.StdEncoding.EncodeToString([]byte("nope")), "1761566400", body},
		"missing timestamp":  {key, s.Sign("", body), "", body},
	}
	for name, c := range cases {
		if err := Verify(c.key, c.sig, c.timestamp, c.body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// SendGrid signs the timestamp followed by the body with no separator, so the same bytes
// split differently verify alike: "17615664001"+"23" equals "1761566400"+"123". Requiring a
// timestamp of digits only means body bytes can't be moved into it (a multipart body starts
// with "--"), and anything else in the header is refused.
func TestTimestampMustBeDigits(t *testing.T) {
	s := newSigner(t)
	key := publicKey(t, s)
	sig := s.Sign("17615664001", []byte("23"))
	if err := Verify(key, sig, "1761566400", []byte("123")); err != nil {
		t.Fatalf("documents the construction: same concatenated bytes should verify: %v", err)
	}
	for _, ts := range []string{"1761566400 ", "1761566400\r\n--x", "-1761566400", "17615664e5"} {
		body := []byte("--x--")
		if err := Verify(key, s.Sign(ts, body), ts, body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("timestamp %q: err = %v, want ErrBadSignature", ts, err)
		}
	}
}

func TestStreamingVerificationMatchesOneShot(t *testing.T) {
	s := newSigner(t)
	body := []byte("a body sent in several writes")
	sig := s.Sign("1761566400", body)
	v := NewStreamVerifier(publicKey(t, s), "1761566400")
	for _, part := range [][]byte{body[:5], body[5:17], body[17:]} {
		if _, err := v.Write(part); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := v.Verify(sig); err != nil {
		t.Fatalf("streamed Verify: %v", err)
	}
	v2 := NewStreamVerifier(publicKey(t, s), "1761566400")
	_, _ = v2.Write(body[:10])
	if err := v2.Verify(sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("truncated stream verified: %v", err)
	}
}

// SendGrid's documented example key begins with the DER prefix of a P-256
// SubjectPublicKeyInfo; keys on other curves are refused rather than half-trusted.
func TestParsePublicKey(t *testing.T) {
	const docsPrefix = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE"
	s := newSigner(t)
	if got := s.PublicKeyBase64()[:len(docsPrefix)]; got != docsPrefix {
		t.Errorf("P-256 key encodes as %q..., want SendGrid's documented prefix %q", got, docsPrefix)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	for name, in := range map[string]string{
		"P-384 key":  base64.StdEncoding.EncodeToString(der),
		"not base64": "%%%",
		"not a key":  base64.StdEncoding.EncodeToString([]byte("nope")),
		"empty":      "",
	} {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

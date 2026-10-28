// Package twilio verifies that webhook requests were signed by Twilio.
//
// See https://www.twilio.com/docs/usage/security#validating-requests.
package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Signature returns the base64 HMAC-SHA1 that Twilio sends in X-Twilio-Signature for a
// form-encoded POST: the full URL followed by each parameter name and value, with names
// sorted by byte order and no delimiters, keyed with the account's auth token.
func Signature(authToken, fullURL string, params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		for _, v := range params[k] {
			b.WriteString(k)
			b.WriteString(v)
		}
	}

	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Valid reports whether signature is Twilio's signature for the request. Twilio signs
// some URLs with the default port and some without, so both forms are tried. The
// comparison is constant-time so response timing can't reveal a partial match.
func Valid(authToken, fullURL string, params url.Values, signature string) bool {
	for _, u := range []string{fullURL, toggleDefaultPort(fullURL)} {
		if u == "" {
			continue
		}
		if hmac.Equal([]byte(Signature(authToken, u, params)), []byte(signature)) {
			return true
		}
	}
	return false
}

// toggleDefaultPort removes an explicit port from rawURL, or adds the scheme's default
// port if there is none. It returns "" if rawURL can't be parsed or has an unknown scheme.
func toggleDefaultPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Port() != "" {
		u.Host = u.Hostname()
		return u.String()
	}
	switch u.Scheme {
	case "https":
		u.Host = net.JoinHostPort(u.Hostname(), "443")
	case "http":
		u.Host = net.JoinHostPort(u.Hostname(), "80")
	default:
		return ""
	}
	return u.String()
}

// Command sendhook sends a fake Twilio webhook, signed the way Twilio signs it, to a locally
// running ingest service. It's a development tool for trying the system without Twilio.
//
// It reads TWILIO_AUTH_TOKEN, INGEST_PUBLIC_URL and INGEST_PORT from the environment, like
// the ingest service, so the signature matches. Run with -h for options.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/internal/twilio"
)

type options struct {
	channel      string // voice or sms
	from, to     string
	id           string // CallSid/MessageSid; random if empty
	body         string // SMS text
	target       string // where to send, e.g. http://127.0.0.1:8081
	publicURL    string // what Twilio would have called; the signature covers this
	token        string
	badSignature bool
}

func main() {
	port := os.Getenv("INGEST_PORT")
	if port == "" {
		port = "8080"
	}
	o := options{token: os.Getenv("TWILIO_AUTH_TOKEN"), publicURL: os.Getenv("INGEST_PUBLIC_URL")}
	flag.StringVar(&o.channel, "channel", "voice", "voice or sms")
	flag.StringVar(&o.from, "from", "+14158675310", "caller's number (try anonymous)")
	flag.StringVar(&o.to, "to", "+18005550001", "tracking number called")
	flag.StringVar(&o.id, "id", "", "CallSid/MessageSid; reuse one to see deduplication (default random)")
	flag.StringVar(&o.body, "body", "Hi, I saw your ad", "SMS text")
	flag.StringVar(&o.target, "target", "http://127.0.0.1:"+port, "ingest service address")
	flag.BoolVar(&o.badSignature, "bad-signature", false, "send an invalid signature")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "sendhook:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.token == "" || o.publicURL == "" {
		return fmt.Errorf("TWILIO_AUTH_TOKEN and INGEST_PUBLIC_URL must be set; run: set -a; source .env; set +a")
	}
	req, err := buildRequest(o)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s %s\n%s\n", resp.Proto, resp.Status, strings.TrimSpace(string(body)))
	return nil
}

// buildRequest creates the webhook POST for o, sent to o.target but signed for o.publicURL.
func buildRequest(o options) (*http.Request, error) {
	ch := envelope.Channel(o.channel)
	idParam, ok := envelope.IDParam(ch)
	if !ok {
		return nil, fmt.Errorf("channel must be voice or sms, got %q", o.channel)
	}
	id := o.id
	if id == "" {
		id = twilioIDPrefix[ch] + randomHex(16)
	}

	form := url.Values{idParam: {id}, "From": {o.from}, "To": {o.to}}
	if o.channel == "sms" {
		form.Set("Body", o.body)
	}

	path := "/webhooks/" + o.channel
	token := o.token
	if o.badSignature {
		token = "wrong-token"
	}
	signature := twilio.Signature(token, strings.TrimSuffix(o.publicURL, "/")+path, form)

	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(o.target, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", signature)
	return req, nil
}

// twilioIDPrefix is how Twilio's provider IDs start for each channel.
var twilioIDPrefix = map[envelope.Channel]string{envelope.Voice: "CA", envelope.SMS: "SM"}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

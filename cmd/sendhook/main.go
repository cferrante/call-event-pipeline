// Command sendhook sends a fake Twilio webhook, or a fake SendGrid Inbound Parse email, signed
// the way each provider signs it, to a locally running ingest service. It's a development
// tool for trying the system without Twilio or SendGrid accounts.
//
// It reads TWILIO_AUTH_TOKEN, INGEST_PUBLIC_URL, INGEST_PORT and (for email)
// SENDGRID_SIMULATOR_PRIVATE_KEY from the environment, so signatures match what ingest
// expects. Run with -h for options.
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
	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
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

	// Email (SendGrid Inbound Parse) only.
	subject     string
	attachBytes int              // size of one attachment to include; 0 for none
	signer      *sendgrid.Signer // from SENDGRID_SIMULATOR_PRIVATE_KEY
}

func main() {
	port := os.Getenv("INGEST_PORT")
	if port == "" {
		port = "8080"
	}
	o := options{token: os.Getenv("TWILIO_AUTH_TOKEN"), publicURL: os.Getenv("INGEST_PUBLIC_URL")}
	flag.StringVar(&o.channel, "channel", "voice", "voice, sms or email")
	flag.StringVar(&o.from, "from", "", "caller's number, or sender's address for email (try anonymous)")
	flag.StringVar(&o.to, "to", "", "tracking number called, or tracking address for email")
	flag.StringVar(&o.id, "id", "", "CallSid/MessageSid/Message-ID; reuse one to see deduplication (default random)")
	flag.StringVar(&o.body, "body", "Hi, I saw your ad", "SMS or email text")
	flag.StringVar(&o.subject, "subject", "Question about your ad", "email subject")
	flag.IntVar(&o.attachBytes, "attach-bytes", 0, "email: include one attachment of this many bytes")
	flag.StringVar(&o.target, "target", "http://127.0.0.1:"+port, "ingest service address")
	flag.BoolVar(&o.badSignature, "bad-signature", false, "send an invalid signature")
	flag.Parse()

	if o.from == "" {
		o.from = map[bool]string{true: "jordan@example.net", false: "+14158675310"}[o.channel == "email"]
	}
	if o.to == "" {
		o.to = map[bool]string{true: "search-fall@leads.example.com", false: "+18005550001"}[o.channel == "email"]
	}
	if o.channel == "email" {
		if raw := os.Getenv("SENDGRID_SIMULATOR_PRIVATE_KEY"); raw != "" {
			s, err := sendgrid.ParsePrivateKey(raw)
			if err != nil {
				fmt.Fprintln(os.Stderr, "sendhook: SENDGRID_SIMULATOR_PRIVATE_KEY:", err)
				os.Exit(1)
			}
			o.signer = s
		}
	}

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "sendhook:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.channel != "email" && (o.token == "" || o.publicURL == "") {
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
	if o.channel == string(envelope.Email) {
		return buildEmailRequest(o)
	}
	ch := envelope.Channel(o.channel)
	idParam, ok := envelope.IDParam(ch)
	if !ok {
		return nil, fmt.Errorf("channel must be voice, sms or email, got %q", o.channel)
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

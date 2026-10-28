package main

import (
	"bytes"
	"encoding/xml"
)

// smsResponse tells Twilio not to reply to the texter.
const smsResponse = "<Response></Response>"

// voiceResponse returns the TwiML for an incoming call: forward it to forwardTo, or end it if
// forwardTo is empty. It is static and instant: call routing never waits on attribution or AI.
func voiceResponse(forwardTo string) string {
	if forwardTo == "" {
		return "<Response></Response>"
	}
	var b bytes.Buffer
	b.WriteString("<Response><Dial>")
	_ = xml.EscapeText(&b, []byte(forwardTo)) // writes to memory; cannot fail
	b.WriteString("</Dial></Response>")
	return b.String()
}

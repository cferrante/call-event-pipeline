package main

import "testing"

func TestVoiceResponseWithoutForwardingEndsTheCall(t *testing.T) {
	if got := voiceResponse(""); got != "<Response></Response>" {
		t.Errorf("voiceResponse(\"\") = %q, want an empty Response", got)
	}
}

func TestVoiceResponseForwardsTheCall(t *testing.T) {
	want := "<Response><Dial>+14158675310</Dial></Response>"
	if got := voiceResponse("+14158675310"); got != want {
		t.Errorf("voiceResponse = %q, want %q", got, want)
	}
}

// Config validation only allows E.164 numbers, but the builder must still never let a
// value inject markup into the instructions Twilio executes.
func TestVoiceResponseEscapesXML(t *testing.T) {
	want := "<Response><Dial>&lt;Hangup/&gt;&amp;</Dial></Response>"
	if got := voiceResponse("<Hangup/>&"); got != want {
		t.Errorf("voiceResponse = %q, want %q", got, want)
	}
}

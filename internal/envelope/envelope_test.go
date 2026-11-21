package envelope

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"
	"time"
)

var receivedAt = time.Date(2026, 10, 27, 12, 0, 0, 123_000_000, time.UTC)

func voiceParams() url.Values {
	return url.Values{
		"CallSid":    {"CA1234567890ABCDE"},
		"AccountSid": {"AC123"},
		"From":       {"+14158675310"},
		"To":         {"+18005551212"},
		"CallStatus": {"ringing"},
	}
}

func smsParams() url.Values {
	return url.Values{
		"MessageSid": {"SM1234567890ABCDE"},
		"AccountSid": {"AC123"},
		"From":       {"+14158675310"},
		"To":         {"+18005551212"},
		"Body":       {"Hi, I saw your ad"},
	}
}

func TestFromWebhookVoice(t *testing.T) {
	env, err := FromWebhook(Voice, voiceParams(), receivedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Envelope{
		SchemaVersion: 1,
		Channel:       Voice,
		Provider:      "twilio",
		ProviderID:    "CA1234567890ABCDE",
		From:          "+14158675310",
		To:            "+18005551212",
		ReceivedAt:    receivedAt,
		Params:        voiceParams(),
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("envelope =\n%+v\nwant\n%+v", env, want)
	}
}

func TestFromWebhookSMS(t *testing.T) {
	env, err := FromWebhook(SMS, smsParams(), receivedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Channel != SMS || env.ProviderID != "SM1234567890ABCDE" {
		t.Errorf("channel, provider_id = %q, %q; want %q, %q", env.Channel, env.ProviderID, SMS, "SM1234567890ABCDE")
	}
}

func TestFromWebhookStoresReceivedAtInUTC(t *testing.T) {
	newYork := time.FixedZone("EDT", -4*60*60)

	env, err := FromWebhook(Voice, voiceParams(), receivedAt.In(newYork))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.ReceivedAt.Location() != time.UTC || !env.ReceivedAt.Equal(receivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", env.ReceivedAt, receivedAt)
	}
}

// The envelope keeps its own copy, so a caller reusing the params can't change a stored event.
func TestFromWebhookCopiesParams(t *testing.T) {
	params := voiceParams()
	env, err := FromWebhook(Voice, params, receivedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	params.Set("To", "+19999999999")
	params["From"][0] = "+10000000000"

	if got := env.Params.Get("To"); got != "+18005551212" {
		t.Errorf("Params To = %q after caller mutation, want unchanged", got)
	}
	if got := env.Params.Get("From"); got != "+14158675310" {
		t.Errorf("Params From = %q after caller mutation, want unchanged", got)
	}
}

func TestFromWebhookRejectsInvalidInput(t *testing.T) {
	smsWithCallSid := smsParams()
	smsWithCallSid.Del("MessageSid")
	smsWithCallSid.Set("CallSid", "CA1")

	voiceWithMessageSid := voiceParams()
	voiceWithMessageSid.Del("CallSid")
	voiceWithMessageSid.Set("MessageSid", "SM1")

	missingTo := voiceParams()
	missingTo.Del("To")

	tests := []struct {
		name    string
		channel Channel
		params  url.Values
	}{
		{"sms without MessageSid", SMS, smsWithCallSid},
		{"voice without CallSid", Voice, voiceWithMessageSid},
		{"missing To", Voice, missingTo},
		{"unknown channel", Channel("fax"), voiceParams()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := FromWebhook(tt.channel, tt.params, receivedAt); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestAnonymousCaller(t *testing.T) {
	tests := []struct {
		from string
		want bool
	}{
		{"+14158675310", false},
		{"+442079460958", false},
		{"", true},
		{"anonymous", true},
		{"Anonymous", true},
		{"unknown", true},
		{"+266696687", true},   // ANONYMOUS on a keypad
		{"266696687", true},    // same, without +
		{"+7378742833", true},  // RESTRICTED
		{"+2562533", true},     // BLOCKED
		{"+86282452253", true}, // UNAVAILABLE
		{"+8656696", true},     // UNKNOWN
		// Leads are keyed by From as stored, so only E.164 can identify one. Accepting these
		// would create a second lead for +14158675310, or a lead for a short code.
		{"14158675310", true},
		{"12345", true},
	}
	for _, tt := range tests {
		t.Run(tt.from, func(t *testing.T) {
			env := Envelope{From: tt.from}
			if got := env.AnonymousCaller(); got != tt.want {
				t.Errorf("AnonymousCaller(%q) = %v, want %v", tt.from, got, tt.want)
			}
		})
	}
}

func TestParseRoundTrip(t *testing.T) {
	env, err := FromWebhook(SMS, smsParams(), receivedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, env) {
		t.Errorf("round trip =\n%+v\nwant\n%+v", got, env)
	}
}

func TestParseUsesSnakeCaseFieldNames(t *testing.T) {
	env, err := FromWebhook(Voice, voiceParams(), receivedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"schema_version", "channel", "provider", "provider_id", "from", "to", "received_at", "params"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("JSON is missing %q: %s", key, data)
		}
	}
}

func TestParseRejectsInvalidEvents(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"not JSON", `not json`},
		{"unknown schema version", `{"schema_version":2,"channel":"voice","provider":"twilio","provider_id":"CA1","to":"+18005551212","received_at":"2026-10-27T12:00:00Z"}`},
		{"missing provider_id", `{"schema_version":1,"channel":"voice","provider":"twilio","to":"+18005551212","received_at":"2026-10-27T12:00:00Z"}`},
		{"unknown channel", `{"schema_version":1,"channel":"fax","provider":"twilio","provider_id":"CA1","to":"+18005551212","received_at":"2026-10-27T12:00:00Z"}`},
		{"missing to", `{"schema_version":1,"channel":"voice","provider":"twilio","provider_id":"CA1","received_at":"2026-10-27T12:00:00Z"}`},
		{"missing received_at", `{"schema_version":1,"channel":"voice","provider":"twilio","provider_id":"CA1","to":"+18005551212"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.data)); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestIsE164(t *testing.T) {
	tests := map[string]bool{
		"+14158675310":      true,
		"+442079460958":     true,
		"14158675310":       false, // no +
		"+04158675310":      false, // country codes don't start with 0
		"+1 415 867 5310":   false,
		"+1234567890123456": false, // 16 digits
		"":                  false,
		"anonymous":         false,
	}
	for in, want := range tests {
		if got := IsE164(in); got != want {
			t.Errorf("IsE164(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIDParam(t *testing.T) {
	tests := []struct {
		ch     Channel
		want   string
		wantOK bool
	}{
		{Voice, "CallSid", true},
		{SMS, "MessageSid", true},
		{Channel("fax"), "", false},
	}
	for _, tt := range tests {
		if got, ok := IDParam(tt.ch); got != tt.want || ok != tt.wantOK {
			t.Errorf("IDParam(%q) = %q, %v; want %q, %v", tt.ch, got, ok, tt.want, tt.wantOK)
		}
	}
}

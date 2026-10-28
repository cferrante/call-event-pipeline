// Package envelope defines the lead event published to the stream for every inbound
// call or SMS webhook.
package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion is the envelope format this code writes and accepts.
const SchemaVersion = 1

// Channel is how the lead contacted us.
type Channel string

const (
	Voice Channel = "voice"
	SMS   Channel = "sms"
)

// Envelope is one inbound interaction, exactly as received. Params holds every original
// form parameter so events can be replayed and reprocessed later.
type Envelope struct {
	SchemaVersion int        `json:"schema_version"`
	Channel       Channel    `json:"channel"`
	Provider      string     `json:"provider"`
	ProviderID    string     `json:"provider_id"`
	From          string     `json:"from"`
	To            string     `json:"to"`
	ReceivedAt    time.Time  `json:"received_at"`
	Params        url.Values `json:"params"`
}

// idParam is the form parameter that uniquely identifies each channel's interaction.
var idParam = map[Channel]string{
	Voice: "CallSid",
	SMS:   "MessageSid",
}

// FromWebhook builds an envelope from a Twilio webhook's form parameters. receivedAt is
// passed in rather than read from the clock so callers (and tests) control it.
func FromWebhook(ch Channel, params url.Values, receivedAt time.Time) (Envelope, error) {
	idName, ok := idParam[ch]
	if !ok {
		return Envelope{}, fmt.Errorf("unknown channel %q", ch)
	}
	env := Envelope{
		SchemaVersion: SchemaVersion,
		Channel:       ch,
		Provider:      "twilio",
		ProviderID:    params.Get(idName),
		From:          params.Get("From"),
		To:            params.Get("To"),
		ReceivedAt:    receivedAt.UTC(),
		Params:        cloneValues(params),
	}
	if env.ProviderID == "" {
		return Envelope{}, fmt.Errorf("%s webhook is missing %s", ch, idName)
	}
	if env.To == "" {
		return Envelope{}, errors.New("webhook is missing To")
	}
	return env, nil
}

// Parse decodes and validates an envelope read from the stream. An error means the
// message can never be processed, so the caller should dead-letter it rather than retry.
func Parse(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	switch {
	case env.SchemaVersion != SchemaVersion:
		return Envelope{}, fmt.Errorf("unsupported schema_version %d", env.SchemaVersion)
	case idParam[env.Channel] == "":
		return Envelope{}, fmt.Errorf("unknown channel %q", env.Channel)
	case env.ProviderID == "":
		return Envelope{}, errors.New("missing provider_id")
	case env.To == "":
		return Envelope{}, errors.New("missing to")
	case env.ReceivedAt.IsZero():
		return Envelope{}, errors.New("missing received_at")
	}
	return env, nil
}

// e164 matches a phone number in E.164 format: "+", then 2 to 15 digits, not starting with 0.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)

// IsE164 reports whether s is a phone number in E.164 format, e.g. +14158675310.
func IsE164(s string) bool {
	return e164.MatchString(s)
}

// withheldCallerIDs are the numbers Twilio has used for withheld caller ID: the word typed
// on a phone keypad (ANONYMOUS → 266696687). Newer calls send the word itself instead.
var withheldCallerIDs = keypadNumbers("ANONYMOUS", "RESTRICTED", "BLOCKED", "UNAVAILABLE", "UNKNOWN")

// AnonymousCaller reports whether the caller withheld their number, so From can't
// identify a lead.
//
// Only an E.164 From can identify a lead, because leads are keyed by From exactly as stored:
// accepting "14158675310" would create a second lead for "+14158675310".
func (e Envelope) AnonymousCaller() bool {
	return !IsE164(e.From) || withheldCallerIDs[strings.TrimPrefix(e.From, "+")]
}

// keypadNumbers returns the digits each word produces on a phone keypad.
func keypadNumbers(words ...string) map[string]bool {
	const keys = "22233344455566677778889999" // digit for each letter A–Z
	nums := make(map[string]bool, len(words))
	for _, w := range words {
		var b strings.Builder
		for _, r := range w {
			b.WriteByte(keys[r-'A'])
		}
		nums[b.String()] = true
	}
	return nums
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

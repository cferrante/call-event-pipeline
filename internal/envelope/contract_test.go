package envelope_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/contract"
	"github.com/cferrante/call-event-pipeline/internal/envelope"
)

// Producer side of the contract: every envelope this code publishes validates against the
// published schema, including withheld caller IDs and webhooks with extra parameters.
func TestPublishedEnvelopesMatchTheSchema(t *testing.T) {
	sch := contract.Schema(t, contract.LeadEventV1)
	at := time.Date(2026, 10, 27, 12, 0, 0, 123_000_000, time.FixedZone("PDT", -7*3600))
	cases := map[string]struct {
		ch     envelope.Channel
		params url.Values
	}{
		"voice":           {envelope.Voice, url.Values{"CallSid": {"CA1"}, "From": {"+14155550100"}, "To": {"+18005550001"}, "CallStatus": {"ringing"}}},
		"sms":             {envelope.SMS, url.Values{"MessageSid": {"SM1"}, "From": {"+14155550101"}, "To": {"+18005550002"}, "Body": {"hi", "there"}}},
		"withheld caller": {envelope.Voice, url.Values{"CallSid": {"CA2"}, "From": {"Anonymous"}, "To": {"+18005550001"}}},
		"no From at all":  {envelope.Voice, url.Values{"CallSid": {"CA3"}, "To": {"+18005550001"}}},
	}
	for name, c := range cases {
		env, err := envelope.FromWebhook(c.ch, c.params, at)
		if err != nil {
			t.Fatalf("%s: build envelope: %v", name, err)
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if err := contract.Validate(sch, data); err != nil {
			t.Errorf("%s: published envelope violates the schema: %v\n%s", name, err, data)
		}
	}

	email, err := envelope.FromInboundEmail(url.Values{
		"from":     {"Jordan Lee <jordan@example.net>"},
		"subject":  {"Quote?"},
		"text":     {"price please"},
		"headers":  {"Message-ID: <m1@example.net>\n"},
		"envelope": {`{"to":["search-fall@leads.example.com"],"from":"jordan@example.net"}`},
	}, 3, at)
	if err != nil {
		t.Fatalf("email: build envelope: %v", err)
	}
	data, err := json.Marshal(email)
	if err != nil {
		t.Fatalf("email: marshal: %v", err)
	}
	if err := contract.Validate(sch, data); err != nil {
		t.Errorf("email: published envelope violates the schema: %v\n%s", err, data)
	}
}

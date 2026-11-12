package schemas_test

import (
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/contract"
)

// Every example from every earlier minor version must still validate against the current
// schema: a schema edit that would reject messages already in the stream fails here.
func TestEveryV1ExampleValidates(t *testing.T) {
	sch := contract.Schema(t, contract.LeadEventV1)
	for _, ex := range contract.Examples(t, "lead-event", 1) {
		if err := contract.Validate(sch, ex.Data); err != nil {
			t.Errorf("%s: %v", ex.Path, err)
		}
	}
}

// The schema must reject what no consumer could process; a schema that accepts anything
// would pass every other test here.
func TestV1RejectsInvalidEvents(t *testing.T) {
	sch := contract.Schema(t, contract.LeadEventV1)
	valid := `{"schema_version":1,"channel":"sms","provider":"twilio","provider_id":"SM1",` +
		`"from":"+14155550101","to":"+18005550002","received_at":"2026-10-27T12:00:01Z","params":{}}`
	if err := contract.Validate(sch, []byte(valid)); err != nil {
		t.Fatalf("baseline event rejected: %v", err)
	}
	cases := map[string]string{
		"missing provider_id": strings.Replace(valid, `"provider_id":"SM1",`, ``, 1),
		"empty provider_id":   strings.Replace(valid, `"provider_id":"SM1"`, `"provider_id":""`, 1),
		"missing to":          strings.Replace(valid, `"to":"+18005550002",`, ``, 1),
		"major version 2":     strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		"version as string":   strings.Replace(valid, `"schema_version":1`, `"schema_version":"1.1"`, 1),
		"unknown channel":     strings.Replace(valid, `"channel":"sms"`, `"channel":"fax"`, 1),
		"bad received_at":     strings.Replace(valid, `"2026-10-27T12:00:01Z"`, `"yesterday"`, 1),
		"params not lists":    strings.Replace(valid, `"params":{}`, `"params":{"Body":"hi"}`, 1),
	}
	for name, doc := range cases {
		if err := contract.Validate(sch, []byte(doc)); err == nil {
			t.Errorf("%s: accepted, want rejected", name)
		}
	}
}

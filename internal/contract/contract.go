// Package contract loads the published event schemas for tests: producers check what they
// publish against them, and the compatibility test checks every past example still passes.
// Only tests import it, so the JSON Schema library never reaches a service binary.
package contract

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/cferrante/call-event-pipeline/schemas"
)

// LeadEventV1 is the schema file for major version 1 of the lead event.
const LeadEventV1 = "lead-event/v1.schema.json"

// Schema compiles a schema file from package schemas, with formats such as date-time
// enforced (JSON Schema treats them as annotations only, unless asked).
func Schema(t testing.TB, file string) *jsonschema.Schema {
	t.Helper()
	b, err := fs.ReadFile(schemas.FS, file)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse schema %s: %v", file, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	url := "https://call-event-pipeline.invalid/schemas/" + file
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add schema %s: %v", file, err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile schema %s: %v", file, err)
	}
	return sch
}

// Validate checks one JSON document against a schema.
func Validate(sch *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse event: %w", err)
	}
	return sch.Validate(inst)
}

// Example is one published example event.
type Example struct {
	Path string // e.g. lead-event/examples/v1.0/voice.json
	Data []byte
}

// Examples returns every example of an event's major version, from every minor version,
// in path order.
func Examples(t testing.TB, event string, major int) []Example {
	t.Helper()
	prefix := fmt.Sprintf("v%d.", major)
	dir := path.Join(event, "examples")
	var out []Example
	err := fs.WalkDir(schemas.FS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if d.IsDir() || !strings.HasPrefix(rel, prefix) || path.Ext(p) != ".json" {
			return nil
		}
		b, err := fs.ReadFile(schemas.FS, p)
		if err != nil {
			return err
		}
		out = append(out, Example{Path: p, Data: b})
		return nil
	})
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("no examples for %s v%d", event, major)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

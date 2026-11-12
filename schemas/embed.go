// Package schemas holds the published event contracts: a JSON Schema per major version of
// each event, and example events for every minor version. Other teams validate against
// these files, whatever language their consumer is written in.
package schemas

import "embed"

// FS holds the schema and example files.
//
//go:embed lead-event
var FS embed.FS

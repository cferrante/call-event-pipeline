package envelope_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/envelope"
	"github.com/cferrante/call-event-pipeline/schemas"
)

// The release this code writes must be a published one: its major version matches
// schema_version, and examples for it exist in the contract.
func TestSchemaReleaseIsPublished(t *testing.T) {
	major, _, ok := strings.Cut(envelope.SchemaRelease, ".")
	if !ok || major != strconv.Itoa(envelope.SchemaVersion) {
		t.Fatalf("SchemaRelease %q doesn't match SchemaVersion %d", envelope.SchemaRelease, envelope.SchemaVersion)
	}
	dir := "lead-event/examples/v" + envelope.SchemaRelease
	if entries, err := fs.ReadDir(schemas.FS, dir); err != nil || len(entries) == 0 {
		t.Errorf("no published examples in schemas/%s", dir)
	}
}

package testenv

import "testing"

type fakeTB struct{ skipped, failed bool }

func (f *fakeTB) Helper()           {}
func (f *fakeTB) Skip(args ...any)  { f.skipped = true }
func (f *fakeTB) Fatal(args ...any) { f.failed = true }

func TestMissingSkipsByDefault(t *testing.T) {
	t.Setenv("REQUIRE_INTEGRATION", "")
	f := &fakeTB{}
	Missing(f, "no database")
	if !f.skipped || f.failed {
		t.Fatalf("skipped=%v failed=%v, want a skip", f.skipped, f.failed)
	}
}

func TestMissingFailsWhenIntegrationRequired(t *testing.T) {
	t.Setenv("REQUIRE_INTEGRATION", "1")
	f := &fakeTB{}
	Missing(f, "no database")
	if !f.failed || f.skipped {
		t.Fatalf("skipped=%v failed=%v, want a failure", f.skipped, f.failed)
	}
}

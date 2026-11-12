// Package testenv decides what an integration test does when a dependency it needs, such
// as Postgres or nats-server, isn't available.
package testenv

import "os"

// TB is the part of testing.TB that Missing uses, so it can be tested with a fake.
type TB interface {
	Helper()
	Skip(args ...any)
	Fatal(args ...any)
}

// Missing reports that a dependency is unavailable. Locally the test is skipped, so unit
// work doesn't need services running. With REQUIRE_INTEGRATION=1, as in CI, it fails
// instead: a skipped test counts as a pass, so a misconfigured CI would otherwise be green
// while testing nothing.
func Missing(t TB, reason string) {
	t.Helper()
	if os.Getenv("REQUIRE_INTEGRATION") == "1" {
		t.Fatal(reason + " (REQUIRE_INTEGRATION=1)")
		return
	}
	t.Skip(reason)
}

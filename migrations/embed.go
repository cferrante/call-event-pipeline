// Package migrations holds the database schema as numbered SQL files, applied in name
// order by internal/migrate. Never edit a file once it has been applied anywhere; add a
// new one instead.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

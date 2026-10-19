// Package migrations holds the SQL migrations of the service, embedded into the binary.
// Files: NNN_name.up.sql and NNN_name.down.sql (docs/migrations.md).
package migrations

import "embed"

// FS contains the migration files at its root. The pattern is "*" rather than "*.sql" so
// that a service without migrations yet (a fresh copy of the template) still builds;
// non-SQL files are ignored by the runner.
//
//go:embed *
var FS embed.FS

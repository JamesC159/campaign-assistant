// Package migrations embeds the SQL schema migrations.
//
// Files are named NNNN_description.sql and applied in order, each in its own
// transaction. Migrations are forward-only: never edit one that has been
// applied anywhere; add a new file instead (the migrator rejects checksum changes).
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS

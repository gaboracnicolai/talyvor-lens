// Package migrations embeds the plain-SQL migration files so the gateway
// binary can apply them via its `migrate` subcommand without the .sql files
// being present in the (binary-only) runtime image. The files themselves are
// unchanged, sequential NNNN_name.sql migrations; see internal/dbmigrate for
// the runner that applies them.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

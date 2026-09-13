// Package migrations contains the PostgreSQL schema owned by the gateway.
//
// Keeping migrations embedded makes the gateway binary self-contained and
// allows deployments to apply exactly the schema it was built against.
package migrations

import "embed"

// SQL contains ordered SQL migration files.
//
//go:embed *.sql
var SQL embed.FS

// Files is the ordered migration list. Versions are encoded in the filename.
var Files = []string{
	"001_registry.sql",
}

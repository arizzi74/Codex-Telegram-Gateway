// Package migrations contains the SQLite schema owned by the gateway.
//
// Embedding the migrations keeps gateway binaries self-contained. The postgres
// subdirectory retains the legacy schema solely for offline migration/audit;
// those files are never executed by the gateway.
package migrations

import "embed"

// SQL contains ordered SQLite migration files.
//
//go:embed *.sql
var SQL embed.FS

// Files is the ordered migration list. Versions are encoded in the filename.
var Files = []string{"001_registry.sql", "002_admin_session_activity.sql", "003_telegram_session_wizards.sql"}

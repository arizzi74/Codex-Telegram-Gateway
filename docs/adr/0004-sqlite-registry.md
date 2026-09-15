# SQLite registry

Context: routing, callbacks, command acceptance, and Telegram deliveries must
survive gateway restarts. The deployment uses one gateway service on one host.

Decision (2026-09-15, supersedes PostgreSQL): use a pure-Go SQLite driver with a
local database file, WAL, FULL synchronization, foreign keys, and a busy timeout.
Immediate transactions serialize writers before resolving routes or claiming
work. SQL constraints and triggers preserve immutable routing and events.
Embedded, checksummed migrations remain authoritative.

Consequences: no database server or database credentials are required. All
integration tests use temporary database files and run without environment setup.
The database directory must be private and writable, and backups must include
committed WAL data through SQLite's backup API. A gateway deployment owns one
local file; network filesystem storage is unsupported. Workers continue using
bbolt and keep running during a gateway outage.

Existing installations use the offline PostgreSQL import helper, which preserves
identities, credentials, routing, watermarks, and delivery state and verifies
row content, counts, foreign keys, and integrity before publishing the SQLite file.

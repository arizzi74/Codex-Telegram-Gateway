# Operations

For prebuilt binaries and scheduled updates, use the [installation and update
guide](installation.md). The manual service procedures below remain available
for custom deployments and recovery.

Run the gateway behind TLS termination at its configured public HTTPS origin,
such as `https://gateway.example.com`. The gateway
listens only on loopback; public access goes through an HTTPS proxy. Guided setup
can add the gateway routes to a selected nginx HTTPS virtual host or configure a
standalone Caddy proxy on Debian or Ubuntu, using
`codex-gateway-proxy.service` and `/etc/codex-gateway-proxy/Caddyfile`. The
[HTTPS setup options](installation.md#finish-gateway-setup) explain alternative
public ports and certificate requirements.
Run `sudo codex-telegramgw finish gateway` to resume HTTPS, Telegram, and first
administrator setup. For a manually managed nginx deployment, the sample
[nginx configuration](../deploy/nginx/telegramgw.conf) proxies health checks,
the admin interface, HTTP APIs, and WebSocket upgrades with bounded body and
timeout settings. Check it with `nginx -t` before a reload.

The proxy must overwrite `X-Real-IP` with its socket peer address: nginx uses
`proxy_set_header X-Real-IP $remote_addr;` and Caddy uses
`header_up X-Real-IP {remote_host}` inside `reverse_proxy`. Include this setting
on every worker and admin API route when updating existing proxy configurations.
The gateway accepts this single IP only from a loopback peer and ignores
`Forwarded` and `X-Forwarded-For`; direct non-loopback requests use the socket
address. Local callers share the proxy's trust boundary. Admin login initiation
and completion have separate per-client and global limits in the gateway, so
generated proxy snippets require no global nginx rate-limit zones. Valid worker
tokens bypass anonymous failure limits, preserving reconnects even behind a
shared NAT or a proxy missing the client header.

## Gateway host

Create a dedicated unprivileged service account and data directories, then
install the Linux unit as root:

```sh
useradd --system --home /var/lib/codex-gateway --shell /usr/sbin/nologin codexgateway
install -d -o codexgateway -g codexgateway -m 0700 /var/lib/codex-gateway
install -d -m 0750 /etc/codex-gateway
install -m 0600 /path/to/secrets.env /etc/codex-gateway/secrets.env
install -m 0640 /path/to/gateway.json /etc/codex-gateway/gateway.json
install -m 0644 deploy/systemd/codex-gateway.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now codex-gateway
```

`database_path` names the local SQLite file, usually
`/var/lib/codex-gateway/gateway.db`. Its parent directory must be writable by the
gateway account so SQLite can create its WAL and shared-memory files. The gateway
enables WAL, foreign keys, FULL synchronization, and immediate write transactions.
Use local storage; one gateway service owns the registry.

`secrets.env` supplies the webhook secret variable named by `webhook_secret_env`.
Keep Telegram, bootstrap, and worker tokens out of unit files, shell history, and Git. Inspect service output with:

```sh
journalctl -u codex-gateway -f
systemctl status codex-gateway
```

Apply a gateway update by stop/start or `systemctl restart codex-gateway` only
after the replacement binary and matching migrations are present. Workers
remain running and reconnect after the gateway returns.

After changing `allowed_chat_ids`, restart the gateway to load the new policy.
At startup it disables remembered destinations outside that list and permanently
suppresses their queued messages, including partially delivered replies and
approval requests. Every outbound message, edit, session menu and typing request
also checks the active policy before sending. Readding a chat permits new
notifications without replaying the suppressed backlog. Sessions, pending
approvals and saved selections remain intact; deletion of old progress messages
and scoped menus can still complete. An empty `allowed_chat_ids` list allows
every chat for the authorized user, so use a nonempty list to restrict chats.

## Worker service

Worker `redact_patterns` apply to outbound display text before it enters the
durable event outbox, including session names/previews, question prompts,
headers/options, errors, and runtime metadata. Pending outbox events are
sanitized on startup without changing their replay identities. Raw question
values remain in the worker's private local records so selecting a redacted
option still submits the original answer; colliding visible labels are numbered.
Previously delivered gateway history is not retroactively removed.

Routing data is deliberately preserved: workspace paths (`cwd`, `default_cwd`,
workspace browser paths), opaque IDs, and model/permission command arguments
must round-trip unchanged. Do not embed secrets in these routing values;
redaction rules protect display fields, not identifiers or filesystem locations.

`/diff` disables executable Git hooks (including `core.fsmonitor`), clean/process
filters, external diff and textconv, and ignores inherited Git overrides and
user/system Git configuration. Repositories using content filters are displayed
without those filters, so this view can differ from a local configured Git diff.

On Linux, as the developer who owns the repositories and Codex credentials:

```sh
scripts/install-worker.sh --worker-binary /absolute/path/codex-worker --config /absolute/path/worker.json
systemctl --user status codex-worker
journalctl --user -u codex-worker -f
```

The installer validates and normalizes the configuration with `LoadWorker`
before atomically writing it with mode `0600` to
`~/.config/codex-worker/config.json`; relative paths therefore continue to
refer to the source configuration’s files after installation. It installs both
`codex-worker` and `codex-local` in `~/.local/bin` (`--local-binary` overrides
the default sibling helper), then enables a user systemd service. Its `HOME`
and `PATH` intentionally belong to that user, so Codex can see the user’s
normal repositories and credentials. Use `--no-start` when staging a
configuration for review. The user-service installer rejects paths containing
spaces or shell/XML control characters; stage the bundle under a simple
absolute path.

Fresh guided installation permits the owning user's home directory and all its
subfolders by default, while prepared configurations keep their explicit
`allowed_workspace_roots`. Selecting a starting project does not narrow the
home-directory root. Files remain subject to the account's normal filesystem
permissions and Codex's configured execution policy.

Linux workers need systemd lingering to remain available after logout and start
at boot. The guided installer checks and enables it, asking for sudo if needed.
For a manual installation, an administrator can run
`sudo loginctl enable-linger USER` for the worker's account. Verify with
`loginctl show-user USER --property=Linger`.

On macOS the same installer creates and validates a native LaunchAgent plist
with `PlistBuddy`, then bootstraps it for the current GUI user. It does not use
`sed` or splice untrusted paths into XML. Logs go to
`~/Library/Logs/codex-worker.log`; inspect its state with
`launchctl print gui/$(id -u)/com.iaia.codex-worker`.

Run `codex-worker --config ~/.config/codex-worker/config.json doctor` before
starting a new worker. Use `codex-worker ... status` for the local durable
state. For an interactive local attachment to a running session, use
`codex-worker --config ~/.config/codex-worker/config.json attach SESSION_OR_THREAD`.
Use `codex-worker attach --latest` to skip the picker and resume the most recently
updated saved session in the exact current directory, including Telegram-created
sessions. The helper explicitly supplies the current directory to the remote
Codex CLI. If no session exists there, Codex opens a new one in that directory.
This option requires a single attachable worker runtime; with multiple runtimes,
select a session by name or thread ID. `--latest` cannot be combined with a
session argument.
For an isolated one-off local Codex process, run `codex-local start` from the
working directory.

## SQLite backup and restore

Use the SQLite command-line tool's online backup API for a consistent backup of
an active database, including committed data still in the WAL:

```sh
umask 077
sqlite3 /var/lib/codex-gateway/gateway.db '.backup /secure/backups/gateway.db'
sqlite3 /secure/backups/gateway.db 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

Run as the gateway account or an administrator; protect backups because they
contain credentials and conversation data. `integrity_check` must return `ok`,
and `foreign_key_check` must return no rows. Do not copy just a live `.db` file:
recent committed data can still be in its `-wal` sidecar.

For restore, stop the gateway, preserve the current database and both sidecars,
and restore the verified backup with mode `0600` and gateway ownership. Remove
only the old destination's `-wal` and `-shm` files while every connection is
closed, then start the gateway and verify `/tgreadyz` and worker reconnection.
Workers keep their local Codex processes and durable outboxes running.

## Switching from PostgreSQL

Version 0.3.0 uses SQLite exclusively. Before switching, build and test the new
release, retain the old binary/configuration, and make a private PostgreSQL dump.
Stop every gateway process that can write the source database; workers may stay
running and buffer events. Keep the old database available for recovery.

The migration helper requires Python 3 with SQLite 3.37+ and `psql`. It uses
standard PostgreSQL connection settings (`PGDATABASE`, `PGUSER`, `PGHOST`,
`PGSERVICE`, or a private `.pgpass` file); no PostgreSQL driver is needed by the
new gateway. Run it as the gateway's service account with read access to the
source and write access to the destination directory:

```sh
PGDATABASE=telegramgw python3 scripts/migrate-postgres-to-sqlite.py \
  --sqlite /var/lib/codex-gateway/gateway.db
```

The destination must not exist. The helper reads one consistent, read-only
PostgreSQL snapshot, checks the old migration checksums, converts all registry
tables, and verifies every table's row count and content digest, foreign keys,
and SQLite integrity before publishing the new file. Source records and secrets
are never printed. Legacy PostgreSQL migrations remain in `migrations/postgres/`
only for verification and migration; they are not linked into the gateway.

Replace `database_url_env` in the gateway configuration with:

```json
"database_path": "/var/lib/codex-gateway/gateway.db"
```

Remove the old database URL from the service environment, install the new binary
and systemd template, reload systemd, and start the gateway. Check `/tgreadyz`, worker
reconnection, event acknowledgements, and the existing admin login and session
bindings. There is no need to enroll workers or register passkeys again.

Before the new gateway accepts work, an unsuccessful cutover can restore the old
binary/configuration and reconnect to the retained PostgreSQL database. After
SQLite has accepted new work, PostgreSQL is stale: preserve the SQLite database
and reconcile new records before any rollback. Do not automatically revert and
silently lose accepted commands or event acknowledgements.

The existing Linux systemd host can use `sudo python3
scripts/deploy-sqlite-host-update.py --worker-user WORKER_USER --check` for
preflight, then omit `--check` to perform the verified cutover. It backs up
configuration, the old binary, and PostgreSQL; it keeps the worker running and
checks that its runtime PIDs are unchanged after reconnecting. Its paths match
the systemd template. The older `deploy-host-update.sh` is specific to the
0.2.1 worker restart and is not used for this database migration.

These Python tools are retained in the source repository for historical
PostgreSQL cutovers. They are not distributed by the current installer or used
for routine installation and updates. The current release manager is the
native Go `codex-telegramgw` executable described in
[installation and updates](installation.md).

## Worker storage and ambiguous outcomes

The worker uses embedded SQLite at its configured `state_file`, with WAL mode
and a separate `.lock` file enforcing a single worker per store. The database,
WAL, shared-memory files, and migration backup remain private to the worker
account. No SQLite CLI, external database server, or C library is needed by the
worker binary.

Routine discovery requests database-only thread listings and uses subscribed
turn/status events for already-loaded user conversations. Hidden helper threads
are classified without repeatedly loading their turn history. The independent
maintenance check still reads current activity before allowing an update.

Transcript statistics keep their scan offsets and partial-line state in SQLite.
Once indexed, unchanged files are not reread, even after a worker restart or
in-memory cache eviction; appended data is scanned from the saved offset.
Replacing or truncating a file, or changing redaction rules, invalidates its
checkpoint. The first scan of existing history still performs the initial
indexing work.

Pending-question recovery reads newest turns first and stops after reaching the
questions the worker already knows. Each attempt reads at most eight fresh
pages; completed older pages are cached in SQLite so an interrupted attempt can
make progress on the next inventory cycle. A changed newest page invalidates
the previous cache, and old unknown questions are never imported.

On the first startup with an older bbolt store, the worker locks and verifies
the original, copies every record into SQLite, verifies the copied data, and
keeps a private `state_file.bbolt-backup` before atomically replacing the state
file. Existing backups are preserved using `.bbolt-backup.1`, `.2`, and so on.
Commands, event acknowledgements, runtime generations, enrollment identity, and update
request markers retain their values. A failure before replacement leaves the
original store unchanged.

Older worker binaries cannot open the converted SQLite store. Restoring the
legacy backup after the new worker has accepted commands discards newer ledger
and outbox records and can replay old submissions. Preserve the current state
and review outcomes before considering that rollback.

For backup, stop the worker cleanly first or use SQLite's online backup API.
Copying only the main file while WAL is active does not produce a complete
backup. Read-only SQL connections can inspect the current format, but never
open the live database with bbolt or delete the lock file. Preserve the original
first, using the actual configured `state_file`:

```sh
systemctl --user stop codex-worker
cp -p /path/to/configured/worker.db ~/worker.db.recovery-copy
```

If the state file cannot open, restore the most recent verified copy, start the
worker, and let it reconnect and reconcile sessions. Never delete the ledger to
make a command retry. A command that reached Codex before a crash can be
reported as `outcome_unknown`; inspect the Codex thread/history and repository
state before deciding what to do. Do not replay `turn/start` just to make its
status look complete.

## Credentials and recovery

Create a first-admin token only from the gateway host:

```sh
codex-gateway --config /etc/codex-gateway/gateway.json admin bootstrap
```

It expires in 15 minutes and is shown once. Open `/tgadmin/` over HTTPS, enroll a
resident user-verified personal passkey, and add a second passkey before
revoking the first. The browser console can create workers, rotate their token,
and revoke them. Copy a returned worker token directly to the worker’s private
token file; it is deliberately unavailable after the create or rotate response.

If every admin passkey is lost, there is deliberately no self-service or
password recovery route: a new bootstrap token cannot add a second owner once
the singleton administrator record exists. Recover from a tested database
backup or follow a separately approved incident procedure. Rotating a worker
token fences its active connection, so update
the worker token file and restart that worker promptly.

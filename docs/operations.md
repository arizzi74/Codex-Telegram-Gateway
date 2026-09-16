# Operations

For prebuilt binaries and scheduled updates, use the [installation and update
guide](installation.md). The manual service procedures below remain available
for custom deployments and recovery.

Run the gateway behind TLS termination at its configured public HTTPS origin,
such as `https://gateway.example.com`. The gateway
listens only on loopback; public access goes through nginx. The sample
[nginx configuration](../deploy/nginx/telegramgw.conf) proxies health checks,
the admin interface, HTTP APIs, and WebSocket upgrades with bounded body and
timeout settings. Check it with `nginx -t` before a reload.

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

## Worker service

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

On macOS the same installer creates and validates a native LaunchAgent plist
with `PlistBuddy`, then bootstraps it for the current GUI user. It does not use
`sed` or splice untrusted paths into XML. Logs go to
`~/Library/Logs/codex-worker.log`; inspect its state with
`launchctl print gui/$(id -u)/com.iaia.codex-worker`.

Run `codex-worker --config ~/.config/codex-worker/config.json doctor` before
starting a new worker. Use `codex-worker ... status` for the local durable
state. For an interactive local attachment to a running session, use
`codex-worker --config ~/.config/codex-worker/config.json attach SESSION_OR_THREAD`.
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
closed, then start the gateway and verify `/readyz` and worker reconnection.
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
and systemd template, reload systemd, and start the gateway. Check `/readyz`, worker
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

## Worker bbolt recovery and ambiguous outcomes

Stop the worker before copying or inspecting its `state_file`; bbolt is a
single-writer database. Preserve the original first:

```sh
systemctl --user stop codex-worker
cp --preserve=mode,timestamps ~/.config/codex-worker/state/worker.db ~/worker.db.recovery-copy
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

It expires in 15 minutes and is shown once. Open `/admin/` over HTTPS, enroll a
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

# Operations

Run the gateway behind TLS termination on `gateway.example.com`. The gateway
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

`secrets.env` supplies the environment variable named by `database_url_env`
and the webhook secret variable named by `webhook_secret_env`. Do not put the
Telegram token, bootstrap token, worker token, or PostgreSQL URL in the unit
file, shell history, or a repository. Inspect service output with:

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

## PostgreSQL backup and restore

Back up a consistent compressed database dump using a role with read access:

```sh
pg_dump --format=custom --file /secure/backups/telegramgw-$(date +%F).dump "$CODEX_GATEWAY_DATABASE_URL"
pg_restore --list /secure/backups/telegramgw-YYYY-MM-DD.dump
```

Test restores on a separate database. For an actual restore, stop the gateway,
restore into the intended database with `pg_restore --clean --if-exists`, check
ownership and connection settings, then start the gateway and verify `/readyz`.
Do not restore over a live database while gateway instances are writing it.

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

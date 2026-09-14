# Codex Telegram control plane

A Go gateway, PostgreSQL registry, and Linux/macOS worker for controlling local
Codex sessions through an allowlisted Telegram bot. The gateway includes a
passkey-authenticated admin console. Workers supervise private Codex app-server
processes and support local terminal attachment.

## Installed on this host

- Admin console: **https://gateway.example.com/admin/**
- Gateway service: `sudo systemctl status codex-gateway`
- Worker service: `systemctl --user status codex-worker`
- Worker diagnostics: `codex-worker status` and `codex-worker doctor`
- Gateway configuration: `/etc/codex-gateway/gateway.json`
- Worker configuration: `~/.config/codex-worker/config.json`
- Allowed workspace: `/home/USERNAME/projects/telegramgw`

The bot authorizes only the numeric `WLID` in the supplied private `.botsecrets`.
Changing a Telegram username does not grant access. To use the bot, send
`/tgstart`, then `/tginstances` or `/tgsessions` and select a session.
All gateway commands begin with `/tg`: `/tgconnect`, `/tgstatus`, `/tgnew`,
`/tgdisconnect`, `/tgsteer`, `/tginterrupt`, and `/tginput`. Telegram's initial
`/start` button remains an alias for `/tgstart`.

Unprefixed commands control Codex: `/status` shows the selected session's model,
reasoning, recorded context/token usage and limits; `/model`, `/compact`,
`/review`, `/fork`, `/rename`, and the other commands appear in the bot menu.
Use `/help` for Codex commands and `/tghelp` for gateway controls. Terminal-only
commands explain how to use the attached CLI. The bot shows “typing…” while
Codex is preparing a response and stops when a reply or an input request arrives.
See [Telegram commands](docs/telegram-commands.md) for syntax and supported actions.

Ordinary messages submit turns to the selection frozen when each message is
accepted, without a separate “Queued for…” acknowledgement. Codex's progress
messages appear temporarily while the turn runs and are removed after the final
response is delivered. Cleanup also handles interruption and failure, and
resumes after gateway restarts. Replies to earlier bot messages retain that
session's routing.
Approval and input buttons refer to the exact pending request and expire.
If a saved session is open in another independent Codex process, close it there
before sending a turn, use `/fork` to branch its saved conversation, or `/tgnew`
to start fresh. Merely selecting a session does not take its writer lock.

To enroll the first admin passkey, run this locally:

```sh
sudo /usr/local/sbin/codex-gateway-admin admin bootstrap
```

Open the admin console and enter the one-time token under **First
administrator?**. Register your passkey, sign in, and add a spare passkey.
The token expires in 15 minutes. The console manages worker enrollment,
credential rotation, revocation, and operational inventory.

## Local Codex terminal

```sh
# Attach the terminal to a worker-managed app-server and session:
codex-worker attach SESSION_NAME_OR_THREAD_ID

# Choose a session on the single worker runtime interactively:
codex-worker attach

# Start a private app-server, resume this directory's latest session,
# and open the terminal on that same server:
codex-local start

# Attach directly when a private socket and thread ID are already known:
codex-local attach --socket /absolute/private/app.sock THREAD_ID
```

`codex-local start` creates a new session when the directory has no history.
The helper owns its app-server and proxy until the terminal exits. A session
already open in another independent Codex app must be closed there before it
can be resumed on a new server; Codex enforces the active-writer lock.
The terminal can share a worker-owned thread through the worker's private
socket. No Codex listener is exposed over the network.

## Build and test

Requires Go 1.26+, PostgreSQL 14+, and Codex CLI **0.154.0** for live workers.
Unit and integration tests use a fake Codex server and need no model credentials.
PostgreSQL integration tests create and remove isolated schemas.

```sh
go test ./...
TEST_DATABASE_URL='postgres:///telegramgw_test?host=/var/run/postgresql&user=TEST_USER' go test -race ./... -timeout=90s
make lint
make build VERSION=0.2.0
make release VERSION=0.2.0
```

`bin/` contains local executables. `dist/` contains stripped `CGO_ENABLED=0`
executables, release archives, and SHA-256 checksums for Linux amd64/arm64
(gateway, worker, helper) and macOS amd64/arm64 (worker and helper).
The CI workflow runs tests with PostgreSQL, checks formatting/vet, and builds
and verifies the release archives. A hosted CI run requires a remote repository.

## Design and operations

- [Specification](CODEX_TELEGRAM_CONTROL_PLANE_SPEC.md)
- [Acceptance evidence](docs/acceptance.md)
- [Implementation tracker](docs/implementation.md)
- [Operations and recovery](docs/operations.md)
- [Passkey administration](docs/admin.md)
- [Codex compatibility and local transport](docs/codex-compatibility.md)
- [Architecture decisions](docs/adr/)
- [Example configuration](examples/) and [service/proxy templates](deploy/)

Gateway commands are persisted with immutable worker/runtime/generation/session
identities before dispatch. Workers commit command receipts and important
events to bbolt before acknowledgement. Reconnection replays unacknowledged
events; a gateway outage leaves local Codex processes running. Commands with
ambiguous execution outcomes are reported for inspection and never blindly
replayed. Telegram itself has no send idempotency key: a process crash between
an accepted send and its database checkpoint can produce a duplicate message.
Completed chunks are checkpointed and retries use the original rendered text.

Secrets, local configuration, state databases, and build products are excluded
from Git. Never commit `.botsecrets` or enrollment tokens.

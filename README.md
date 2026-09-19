# Codex Telegram control plane

A Go gateway, embedded SQLite registry, and Linux/macOS worker for controlling local
Codex sessions through an allowlisted Telegram bot. The gateway includes a
passkey-authenticated admin console. Workers supervise private Codex app-server
processes and support local terminal attachment.

## Quick install

Requires curl and `sha256sum` or `shasum`. Downloads are verified native Go
binaries; Python and a Go compiler are not required.

**Worker (Linux or macOS):** install and authenticate Codex first, then run as
the account that owns your workspaces:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sh
```

Follow the prompts for your gateway address, enrolled worker ID and token, and
workspace. An existing `worker.json` is reused; rerunning on an installed worker
preserves its configuration and running sessions.

**Gateway (Linux with systemd):** run the same installer with sudo:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sudo sh
```

Follow the prompts for your public HTTPS address, Telegram bot username and
token, and allowed Telegram user ID. The installer creates the configuration,
private secrets, SQLite database, and service. Existing installations are adopted
without restarting. Configure HTTPS separately, then follow the
[gateway setup steps](docs/installation.md#finish-gateway-setup).

Gateway URLs use `/tgadmin/` for the console, `/tgapi/v1/` for APIs,
and `/tghealthz` and `/tgreadyz` for health checks.

Both commands enable daily automatic updates from
[GitHub Releases](https://github.com/arizzi74/Codex-Telegram-Gateway/releases).
The worker also checks for stable Codex runtime updates once per day (UTC).
Supported standalone installations update when the worker has no active or
queued work, then its app servers restart and their versions are verified.
See the [installation guide](docs/installation.md) for HTTPS setup, worker
enrollment, and manual updates.

## Components

- **Gateway:** receives Telegram updates, manages worker connections, and serves
  the passkey-authenticated admin console.
- **Registry:** stores routing, commands, events, approvals, and Telegram
  delivery state in a local SQLite file.
- **Worker:** runs on a development machine, supervises Codex app servers, and
  executes commands within configured workspace roots.
- **Local helper:** opens a terminal interface against a private app-server
  socket or starts an independent local runtime.

## Configuration and startup

Use the [example configuration](examples/) as a starting point. Set the gateway's
public HTTPS URL, SQLite `database_path`, and Telegram webhook secret
environment variable for your deployment. Configure each worker with its gateway
WebSocket URL, enrollment credential, state-file location, allowed workspaces,
and runtime profiles.

Create a private `.botsecrets` file containing `BOTNAME`, `BOTTOKEN`, `WLNAME`,
and `WLID`, then reference its location in the gateway configuration. Keep real
credentials and deployment configuration outside Git.

Run the gateway and worker with their respective configuration files:

```sh
codex-gateway --config /path/to/gateway.json serve
codex-worker --config /path/to/worker.json run
```

The gateway creates its SQLite database at `database_path`; relative paths are
resolved against the configuration directory. Use a local disk and a private,
writable directory. It needs the webhook-secret environment variable named in
its configuration; no database server or database credentials are required.
The worker needs a private enrollment-token file and access to the Codex executable. Use `codex-worker status` and
`codex-worker doctor` to inspect the worker and its runtime compatibility.
When using a custom configuration file, pass the same `--config` option to
diagnostics and terminal attachment commands.

[Service and proxy templates](deploy/) are provided for Linux systemd, macOS
launchd, and nginx. Customize their paths, service accounts, permissions, and
TLS settings before installing them. Choose worker permissions to match the
operations you authorize Codex to perform.

## Telegram commands

The bot authorizes only the numeric `WLID` in its private `.botsecrets` file.
Changing a Telegram username does not grant access. To use the bot, send
`/tgstart`, then `/tginstances` or `/tgsessions` and select a session.
All gateway commands begin with `/tg`: `/tgconnect`, `/tgstatus`, `/tgnew`,
`/tghistory`, `/tgdisconnect`, `/tgsteer`, `/tginterrupt`, and `/tginput`. Telegram's initial
`/start` button remains an alias for `/tgstart`.

Unprefixed commands control Codex: `/status` shows the selected session's model,
reasoning, recorded context/token usage and limits; `/model`, `/compact`,
`/review`, `/fork`, `/rename`, and the other commands appear in the bot menu.
Use `/help` for Codex commands and `/tghelp` for gateway controls. Terminal-only
commands explain how to use the attached CLI. The bot shows “typing…” while
Codex is preparing a response and stops when a reply or an input request arrives.
See [Telegram commands](docs/telegram-commands.md) for syntax and supported actions.

Use `/tghistory` to display saved Codex prompts as separate **You · Codex** bot
messages, with an **Older prompts** button for earlier pages. This reads the
selected session's history without running those prompts again.

Send a photo or a JPEG, PNG, WebP or GIF image file to the selected session,
with an optional caption. Images can be up to 10 MiB; both the gateway and
worker must support image input (v0.5.13 or later).

Ordinary messages submit turns to the selection frozen when each message is
accepted, without a separate “Queued for…” acknowledgement. Codex's commentary
and tool calls appear temporarily while the turn runs. The latest tool call
appears in a monospace message that each subsequent tool call replaces.
Temporary messages are removed after the final response is delivered.
Cleanup also handles interruption and failure, and
resumes after gateway restarts. Replies to earlier bot messages retain that
session's routing.
Approval and input buttons refer to the exact pending request and expire.
If a saved session is open in another independent Codex process, close it there
before sending a turn, use `/fork` to branch its saved conversation, or `/tgnew`
to start fresh. Merely selecting a session does not take its writer lock.

## Admin console

To enroll the first administrator, run this with the gateway's configuration
and access to its SQLite database:

```sh
codex-gateway --config /path/to/gateway.json admin bootstrap
```

Open `/tgadmin/` on your gateway's public HTTPS URL, such as
`https://gateway.example.com/tgadmin/`, and enter the one-time token under **First
administrator?**. Register your passkey, sign in, and add a spare passkey.
The token expires in 15 minutes. The console manages worker enrollment,
credential rotation, and revocation. It lists user sessions with prompt counts,
token usage, activity and last messages, and shows the connected Telegram bot's
identity and webhook status. Subagent sessions are excluded, as in `/tgsessions`.
Passkeys use the hostname from the configured public HTTPS URL as their
relying-party ID; browser requests must match the full configured origin.

## Local Codex terminal

```sh
# Attach the terminal to a worker-managed app-server and session:
codex-worker attach SESSION_NAME_OR_THREAD_ID

# Choose a session on the single worker runtime interactively:
codex-worker attach

# Resume the most recently updated session in the current directory:
codex-worker attach --latest

# Start a private app-server, resume this directory's latest session,
# and open the terminal on that same server:
codex-local start

# Attach directly when a private socket and thread ID are already known:
codex-local attach --socket /absolute/private/app.sock THREAD_ID
```

Use `codex-worker attach` to share a worker-managed session with Telegram.
`--latest` skips the picker and searches the current directory's saved sessions,
including sessions created through Telegram. If none exists, Codex opens a new
session in that directory. With multiple worker runtimes, specify a session
name or thread ID instead.
Closing the attached terminal leaves the worker and its app server running.
An idle attached CLI can reconnect across automatic worker updates. Active turns,
requests and approvals still defer an update; see the
[update guide](docs/installation.md#check-and-apply-updates) for the first upgrade from
older workers.

`codex-local start` owns an independent app server and proxy until the terminal
exits, and creates a new session when the directory has no history. A session
already open in another independent Codex app must be closed there before it
can be resumed on a new server; Codex enforces the active-writer lock.
The terminal can share a worker-owned thread through the worker's private
socket. No Codex listener is exposed over the network.

## Build and test

Requires Go 1.26+ and Codex CLI **0.154.0** for live workers.
Unit and integration tests use a fake Codex server and need no model credentials.
All database integration tests run automatically using isolated temporary SQLite
files. The pure-Go SQLite driver also works with `CGO_ENABLED=0`.

```sh
go test ./...
go test -race ./... -timeout=90s
make lint
make build VERSION=0.4.0
make release VERSION=0.4.0
```

`bin/` contains local executables. `dist/` contains stripped `CGO_ENABLED=0`
executables, release archives, and SHA-256 checksums for Linux amd64/arm64
(gateway, worker, helper and release manager) and macOS amd64/arm64 (worker, helper
and release manager). Linux executables have no dynamic-loader or shared-library
dependency. The four standalone managers support installation without Python;
packaging and archive verification are also implemented in Go.
The CI workflow runs the SQLite integration and race tests, checks formatting/vet,
and builds and verifies the release archives. A hosted CI run requires a remote repository.
Only the source-only historical PostgreSQL migration/deployment tests require
Python on the build host; target machines do not need it.

Existing PostgreSQL installations must use the verified [migration procedure](docs/operations.md#switching-from-postgresql)
before starting this version. Changing the configuration alone does not transfer data.

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

The repository ignores `.botsecrets`, environment files, local configuration
overrides, state databases, and build products. Keep enrollment-token files and
other private configuration outside the repository or add explicit ignore rules
for their locations before creating them.

# Installation and automatic updates

The installer downloads prebuilt binaries from
[GitHub Releases](https://github.com/arizzi74/Codex-Telegram-Gateway/releases).
A Go toolchain, Python and a repository checkout are not required on the target machine.
Gateway packages support Linux amd64/arm64. Worker and terminal-helper packages
support Linux and macOS, on amd64/arm64.

## Before installing

Install curl and provide `sha256sum` (Linux) or `shasum` (macOS). The bootstrap
uses the system POSIX shell and standard utilities. Installation and updates run
in the downloaded native Go manager. Linux binaries are statically linked;
macOS binaries use the normal macOS system libraries. Linux service installation
uses systemd; macOS workers use launchd. Workers also need the supported Codex executable, its normal
account credentials, and access to their configured workspaces.

For a gateway, have your public HTTPS address, Telegram bot username and token,
and allowed numeric Telegram user ID ready. Guided setup creates the JSON
configuration and private secret files for you, including a random webhook
secret and a SQLite database at `/var/lib/codex-gateway/gateway.db`. Configure
HTTPS termination separately using the [proxy template](../deploy/nginx/telegramgw.conf).
For custom settings, prepare the [JSON configuration](../examples/gateway.json)
and a private environment file instead. The managed gateway database must be
under `/var/lib/codex-gateway`.

For a worker, enroll it in the gateway admin console and copy its worker ID and
one-time token. The guided installer creates the configuration for you, or you
can prepare [worker.json](../examples/worker.json) with its private token file,
gateway WSS URL, allowed workspace roots, and runtime profiles. Keep all real configuration and
credentials outside the repository. See [operations](operations.md) for setup
and recovery details.

## Download the installer

```sh
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh \
  --output /tmp/codex-telegramgw-install.sh
```

The bootstrap resolves a stable GitHub Release and verifies the downloaded
manager against that release's SHA-256 manifest before running it. The manager
verifies both the platform archive and its contained binary. Downloaded archives
cannot install symlinks or paths outside the staging directory.

## Install a gateway

Run on the Linux gateway host:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sudo sh
```

Running with sudo selects gateway setup; running without sudo selects worker
setup. No installer arguments are needed. Automatic update checks run every five
minutes, at minutes `00`, `05`, `10`, … in the gateway's local time.

- An existing gateway in the standard location is adopted without replacing
  its configuration or restarting its service.
- Otherwise, a prepared `gateway.json` and private `secrets.env` in the current
  directory are reused. The bot secrets file referenced by the JSON must also
  exist with private permissions.
- If no configuration exists, the wizard asks for your HTTPS address, bot
  username, allowed Telegram user ID and display label, local port, and bot
  token. Token entry is hidden; the webhook secret is generated automatically.

The prompts read the terminal directly, so they work through the pipe. Without
a terminal, prepare the configuration files first. The installer sets up the
service account, private configuration, SQLite data directory, gateway binary,
systemd service, and updater. It does not install PostgreSQL or configure DNS,
TLS certificates, or a reverse proxy.

You can also use `sudo sh /tmp/codex-telegramgw-install.sh` or
`sudo codex-telegramgw setup gateway`. For an unattended installation using
custom paths, the explicit command remains available:

```sh
sudo sh /tmp/codex-telegramgw-install.sh install gateway \
  --config /path/to/gateway.json \
  --secrets-env /path/to/secrets.env \
  --auto-update
```

### Finish gateway setup

Configure your public HTTPS reverse proxy to reach the selected local port
(`127.0.0.1:8080` by default), including WebSocket upgrades. Then run these
commands on the gateway host to register the Telegram webhook and command menu
and create the first administrator's one-time token:

```sh
gateway() {
  sudo systemd-run --quiet --wait --pipe --collect \
    --uid=codexgateway --gid=codexgateway \
    -p EnvironmentFile=/etc/codex-gateway/secrets.env \
    -p WorkingDirectory=/var/lib/codex-gateway -p UMask=0077 \
    /usr/local/lib/codex-telegramgw/codex-gateway \
    --config /etc/codex-gateway/gateway.json "$@"
}
gateway webhook set
gateway menu set
gateway admin bootstrap
```

This runs gateway administration as its service account and loads the private
environment through systemd. The one-time token is printed to your terminal.
Open the printed `/admin/` address, register a passkey within 15 minutes, and
enroll a worker to obtain its ID and token.

## Install a worker

Run as the developer account that owns Codex and the workspaces:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sh
```

No installer arguments are needed. The default is worker setup with automatic
update checks every five minutes, at minutes `02`, `07`, `12`, … in the worker's
local time, two minutes after gateway checks:

- If a worker is already installed in the standard location, setup adopts it
  without changing its configuration or restarting its sessions.
- Otherwise, it uses a private `worker.json` in the current directory if present.
- If neither exists, it asks for the gateway address, enrolled worker ID and
  token, worker name, and workspace. Token entry is hidden. It detects Codex
  on your `PATH` and creates one primary runtime for the selected workspace.

The prompts use the terminal directly, so they work when the script is piped
into `sh`. Without an interactive terminal, provide `worker.json` beforehand.
You can also run the downloaded script with `sh /tmp/codex-telegramgw-install.sh`
or run the native manager's `codex-telegramgw setup worker` command.

This installs both `codex-worker` and `codex-local` for the current user and sets
up the worker service. Relative configuration paths are resolved before the
configuration is installed. Add `~/.local/bin` to the terminal's `PATH` if needed.

For unattended installation with a configuration at another location, the
explicit command is still available:

```sh
sh /tmp/codex-telegramgw-install.sh install worker \
  --config /path/to/worker.json \
  --auto-update
```

The installer checks the configured Codex executable but does not install or
update Codex itself. That runtime continues to use its own update mechanism.

## Adopt an existing installation

For services already installed in the standard paths, use `adopt` instead of
`install`. This preserves configuration and running services, and installs the
update manager:

```sh
sudo sh /tmp/codex-telegramgw-install.sh adopt gateway --auto-update
sh /tmp/codex-telegramgw-install.sh adopt worker --auto-update
```

Gateway adoption also makes the configuration directory root-owned so the
service account cannot change settings used by the privileged updater. Existing
SQLite data and credentials remain in place. Worker adoption must run as its
owning user.

Existing Python-based updaters can install the first native-manager release
through their normal update command. Its compatibility payload replaces the old
`release-manager.py` file with the native executable; the filename may remain
until the native manager installs its canonical `codex-telegramgw` path. It is
an executable binary and no longer requires Python. Fresh installations use
the canonical path directly. Older releases whose only standalone manager was
Python cannot be selected with the new bootstrap.

## Check and apply updates

```sh
# Gateway administration:
sudo codex-telegramgw update gateway --check
sudo codex-telegramgw update gateway

# Worker administration, as its owning user:
codex-telegramgw update worker --check
codex-telegramgw update worker
```

Checks fetch release metadata and checksums to show whether a newer stable
release is available; they do not download or verify the component archives.
When applying an update, the native manager downloads and verifies all required
archives before stopping the service. A download or verification failure leaves
a running service online and its installed binaries in place.

The native manager retries temporary HTTP and network failures, with at most
four attempts per download within five minutes. It waits between attempts and
respects GitHub's rate limits and retry delays. Retry logs and final errors name
the failed request and include the HTTP status and GitHub request ID when
available, without exposing credentials or signed download URLs.

Applying an update preserves configuration, credentials, and database locations.
Gateway updates back up SQLite before migrations and verify readiness after
startup. Workers continue running while the gateway restarts and replay their
durable outboxes when it returns.

Worker updates require cooperation from the running worker. It must have no
active or waiting turns, queued work, attached terminal clients, or outstanding
events awaiting gateway acknowledgement. During a prepared update, the worker
fences new commands and terminal attachments until it restarts or the preparation
expires. Busy workers defer the update and keep working.

For this release, a worker that has accepted a native terminal attachment also
defers live updates after the terminal disconnects. This avoids interrupting
native requests that may still be queued inside the app server. After finishing
that work, stop the worker and apply the update using the commands below.
An app-server request timeout also blocks live updates for that runtime, because
the timed-out request may still execute later.

Workers predating this update protocol cannot safely participate in unattended
restarts. Their automatic updates defer. For the first upgrade, finish active
turns, close attached terminals, stop the old worker service, and run the worker
update command. The new worker supports guarded automatic updates thereafter.

On Linux, run these commands in a separate terminal after finishing worker tasks:

```sh
systemctl --user stop codex-worker.service
codex-telegramgw update worker
```

The updater starts the worker after installing the new version. On macOS, stop
it with `launchctl bootout "gui/$(id -u)/com.iaia.codex-worker"` before running
the same update command. Stopping the worker ends its current app-server
processes, so do not run this from a Codex turn hosted by that worker.
If a download fails after you manually stop the worker, start it again with
`systemctl --user start codex-worker.service` on Linux, or load its LaunchAgent
again on macOS, before retrying the update.

## Automatic updates

Guided gateway and worker setup enable scheduled checks for stable releases every
five minutes automatically. Gateway checks run at minutes `00`, `05`, `10`, …;
worker checks run two minutes later at `02`, `07`, `12`, … in each machine's local
time. Linux systemd timers use one-second accuracy with no random delay. macOS
workers use the same minute schedule in launchd. After sleep or downtime, missed
checks may run when the machine resumes instead of preserving the offset.
For explicit `install` and `adopt` commands, pass `--auto-update` to enable it.
To manage it later:

```sh
sudo codex-telegramgw auto-update enable gateway
sudo codex-telegramgw auto-update disable gateway
codex-telegramgw auto-update enable worker
codex-telegramgw auto-update disable worker
```

The gateway scheduler runs as a system service; the worker scheduler runs as its
owning user. Releases marked draft or prerelease are not selected automatically.
An older version is not installed over a newer one. Network or verification
failures leave the installed binaries in place.

Older native gateway managers can fail under systemd with `$HOME is not defined`
before checking GitHub. The current manager uses system paths for the gateway
and only requires a home directory for worker installations. To let an older
installed manager download the fixed release at its next scheduled check, run
`sudo systemctl edit codex-gateway-update.service` and add:

```ini
[Service]
Environment=HOME=/root
```

Then run `sudo systemctl daemon-reload`. This supplies the older manager's
required environment without starting an update or restarting either service.

On Linux, inspect the schedules and update logs with:

```sh
systemctl list-timers codex-gateway-update.timer
systemctl --user list-timers codex-worker-update.timer
sudo journalctl -u codex-gateway-update.service
journalctl --user -u codex-worker-update.service
```

Gateway backups are kept in `/var/backups/codex-gateway/updates`; worker binary
backups are in `~/.local/state/codex-worker/updates`. If migration fails before
startup, the updater restores the previous binary and SQLite backup. Once new
service startup has been attempted, failures retain the new files for inspection
so later writes cannot be lost through an automatic rollback.

## Publishing an update

Maintainers commit and push the change, then create a new stable version tag:

```sh
git tag vMAJOR.MINOR.PATCH
git push origin vMAJOR.MINOR.PATCH
```

The release workflow runs formatting, vet, migration and race tests, all platform
builds, and archive verification. Only after those checks pass does it publish
the version's binaries, installer, update manager, and checksum manifest. It
uploads a draft first so automatic updaters never select a partially uploaded
release. Published releases are not overwritten; corrections use a new version.

Each release publishes ten component archives, four native manager executables
(`codex-telegramgw-{linux,darwin}-{amd64,arm64}`), `install.sh`, and `SHA256SUMS`.
Every component archive contains the matching native manager and inner checksums.
Packaging and verification use the Go release tool. Python remains only in the
repository's historical PostgreSQL migration tools and their CI tests; those
tools are not shipped in the release archives.

The source of trust is this GitHub repository and its release publishing access.
Checksums detect mismatched or damaged downloads; protect the repository's write
permissions as carefully as deployment access.

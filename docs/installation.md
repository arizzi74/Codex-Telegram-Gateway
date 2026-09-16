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

For a gateway, prepare the [JSON configuration](../examples/) first. Set your
own HTTPS origin, a local SQLite `database_path`, the path to
your private `.botsecrets`, and the environment-variable name for the Telegram
webhook secret. Provide that variable in a private environment file. Configure
HTTPS termination separately using the [proxy template](../deploy/nginx/telegramgw.conf).
The managed gateway service requires its database under `/var/lib/codex-gateway`,
for example `/var/lib/codex-gateway/gateway.db`.

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
sudo sh /tmp/codex-telegramgw-install.sh install gateway \
  --config /path/to/gateway.json \
  --secrets-env /path/to/secrets.env \
  --auto-update
```

The installer sets up the service account, private configuration and SQLite data
directory, gateway binary, systemd service, and update manager. Configure the
Telegram webhook and command menu after HTTPS is ready, then enroll your worker.
The gateway uses SQLite; the installer does not install PostgreSQL.

## Install a worker

Run as the developer account that owns Codex and the workspaces:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sh
```

No installer arguments are needed. The default is worker setup with daily
automatic updates enabled:

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

Checks show whether a newer stable release is available. Applying an update
preserves configuration, credentials, and database locations. Gateway updates
back up SQLite before migrations and verify readiness after startup. Workers
continue running while the gateway restarts and replay their durable outboxes
when it returns.

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

## Automatic updates

Worker setup enables a daily scheduled check for stable releases automatically.
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

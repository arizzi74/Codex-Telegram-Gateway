# Installation and automatic updates

The installer downloads prebuilt binaries from
[GitHub Releases](https://github.com/arizzi74/Codex-Telegram-Gateway/releases).
A Go toolchain and a repository checkout are not required on the target machine.
Gateway packages support Linux amd64/arm64. Worker and terminal-helper packages
support Linux and macOS, on amd64/arm64.

## Before installing

Install Python 3.10+ and curl. Linux service installation uses systemd; macOS
workers use launchd. Workers also need the supported Codex executable, its normal
account credentials, and access to their configured workspaces.

Prepare the [gateway or worker JSON configuration](../examples/) first. For a
gateway, set your own HTTPS origin, a local SQLite `database_path`, the path to
your private `.botsecrets`, and the environment-variable name for the Telegram
webhook secret. Provide that variable in a private environment file. Configure
HTTPS termination separately using the [proxy template](../deploy/nginx/telegramgw.conf).
The managed gateway service requires its database under `/var/lib/codex-gateway`,
for example `/var/lib/codex-gateway/gateway.db`.

Worker configuration needs an enrolled worker ID and its private token file,
the gateway WSS URL, allowed workspace roots, and runtime profiles. Enrollment
is available through the gateway admin console. Keep all real configuration and
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
sudo bash /tmp/codex-telegramgw-install.sh install gateway \
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
bash /tmp/codex-telegramgw-install.sh install worker \
  --config /path/to/worker.json \
  --auto-update
```

This installs both `codex-worker` and `codex-local` for the current user and sets
up the worker service. Relative configuration paths are resolved before the
configuration is installed. Add `~/.local/bin` to the terminal's `PATH` if needed.

## Adopt an existing installation

For services already installed in the standard paths, use `adopt` instead of
`install`. This preserves configuration and running services, and installs the
update manager:

```sh
sudo bash /tmp/codex-telegramgw-install.sh adopt gateway --auto-update
bash /tmp/codex-telegramgw-install.sh adopt worker --auto-update
```

Gateway adoption also makes the configuration directory root-owned so the
service account cannot change settings used by the privileged updater. Existing
SQLite data and credentials remain in place. Worker adoption must run as its
owning user.

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

`--auto-update` installs a daily scheduled check for stable releases. To manage
it later:

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
git tag v0.4.0
git push origin v0.4.0
```

The release workflow runs formatting, vet, migration and race tests, all platform
builds, and archive verification. Only after those checks pass does it publish
the version's binaries, installer, update manager, and checksum manifest. It
uploads a draft first so automatic updaters never select a partially uploaded
release. Published releases are not overwritten; corrections use a new version.

The source of trust is this GitHub repository and its release publishing access.
Checksums detect mismatched or damaged downloads; protect the repository's write
permissions as carefully as deployment access.

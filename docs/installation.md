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
uses systemd; macOS workers use launchd. Run Linux worker setup from a direct
login as the developer account, including SSH, so its systemd user session is
available. On macOS, sign in to the desktop and use Terminal; the worker's
LaunchAgent runs while that desktop account remains logged in.
Installing Codex when it is missing also requires the standard `tar` and `gzip`
utilities. See the official [Codex CLI](https://developers.openai.com/codex/cli)
and [authentication](https://developers.openai.com/codex/auth) instructions for
its account and sign-in options.

For a gateway, have your public HTTPS address, Telegram bot username and token,
and allowed numeric Telegram user ID ready. In Telegram, use `@BotFather`'s
`/newbot` command to create the bot or `/token` to obtain an existing bot's token.
The allowed user ID belongs to your personal account; it is a number, not your
username or the bot's ID. Guided setup creates the JSON
configuration and private secret files for you, including a random webhook
secret and a SQLite database at `/var/lib/codex-gateway/gateway.db`. Point your
domain's DNS records to the gateway and allow its chosen public HTTPS port in
both its host and cloud firewalls. The wizard can add the gateway to an existing
nginx HTTPS virtual host, install a standalone Caddy proxy on Debian or Ubuntu,
check an existing proxy, or guide manual setup using the
[proxy template](../deploy/nginx/telegramgw.conf). Automatic certificates also
require an accessible certificate challenge port; see the HTTPS options below.
For custom settings, prepare the [JSON configuration](../examples/gateway.json)
and a private environment file instead. The managed gateway database must be
under `/var/lib/codex-gateway`.

For a worker, enroll it in the gateway admin console and copy its worker ID and
one-time token. Setup offers to install the standalone Codex CLI if it is missing
and guides its account sign-in. You can use device login over SSH, opening the
displayed link on another computer. The guided installer creates the worker
configuration for you, or you
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
setup. No installer arguments are needed. Daily automatic updates are enabled.

- An existing gateway in the standard location is adopted without replacing
  its configuration or restarting its service.
- Otherwise, a prepared `gateway.json` and private `secrets.env` in the current
  directory are reused. The bot secrets file referenced by the JSON must also
  exist with private permissions.
- If no configuration exists, the wizard detects nginx and lists its configured
  HTTPS virtual hosts. Choose an existing host for the gateway routes, or choose
  a standalone proxy and its public HTTPS address and port. It also asks for the
  bot username, allowed Telegram user ID and display label, local gateway port,
  and bot token. Token entry is hidden; the webhook secret is generated
  automatically.
- After installation, the wizard guides HTTPS setup, checks the public routes,
  registers the Telegram menu and webhook, and prints the admin console address
  and a one-time token for the first passkey. Enroll your worker in that console
  before running the worker installer.

The prompts read the terminal directly, so they work through the pipe. Without
a terminal, prepare the configuration files first. The installer sets up the
service account, private configuration, SQLite data directory, gateway binary,
systemd service, and updater. It uses SQLite, so no separate database server is
needed. DNS and firewall changes remain your responsibility; the prompts explain
what is required and let you retry or finish later.

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

The fresh guided installation includes these steps. To resume them later, or
finish an installation made from prepared configuration files, run:

```sh
sudo codex-telegramgw finish gateway
```

The wizard offers these HTTPS options:

- **Existing nginx virtual host:** setup checks whether nginx is installed and
  reads its active configuration to list HTTPS hostnames and ports. Select the
  host that should serve the gateway. Setup inserts a dedicated include in that
  TLS server block, checks the configuration with `nginx -t`, and reloads nginx.
  Existing certificate settings, other virtual hosts, and website paths are
  preserved. Automatic integration uses the standard nginx systemd service;
  custom nginx service commands or incompatible configurations can be completed
  manually.
- **Standalone HTTPS proxy:** setup uses Caddy in a separate
  `codex-gateway-proxy.service`, with configuration in
  `/etc/codex-gateway-proxy/Caddyfile`. Choose a public hostname and HTTPS port;
  443 is the normal default, while setup suggests 8443 when nginx is installed.
  An alternative port is useful when another server already uses 443.
  Setup can install Caddy from Debian or Ubuntu packages, or reuse an installed
  binary without replacing its existing service or configuration. The selected
  ports must be free. The system package manager maintains Caddy separately from
  gateway binary updates.
- **Check, manual, or later:** check a proxy you have already configured, display
  configuration instructions, or resume setup another time with the same
  command.

For the standalone proxy, automatic certificate issuance and renewal require
public DNS pointing to this machine and a certificate validation port reachable
by the certificate authority. On HTTPS port 443, Caddy can validate certificates
through that port even if another server uses port 80. On HTTPS ports 88 or
8443, Caddy also needs public TCP port 80 free and accessible. Choosing an
alternative HTTPS port does not move the certificate authority's validation
ports. If another server owns ports 80 and 443, select an existing nginx HTTPS
host or provide a valid certificate chain and matching private key for the
standalone proxy. HTTPS on port 80 requires supplied certificates. Setup checks
the supplied certificate against the public hostname.
See [Caddy's automatic HTTPS documentation](https://caddyserver.com/docs/automatic-https).

Keep renewing supplied certificates with your existing certificate provider.
The wizard copies the chain and key into its private proxy directory. After
renewal, refresh those copies and restart only the dedicated proxy with:

```sh
sudo codex-telegramgw https refresh
```

This command rechecks the original certificate and key files, copies them only
when changed, and restarts `codex-gateway-proxy.service`. Add it to your
certificate provider's renewal hook when using supplied certificates. Gateway
update checks also refresh these copies when a renewed certificate is found.

Telegram accepts webhook HTTPS ports **443, 80, 88, and 8443**; it does not accept
arbitrary alternative ports. All four require TLS, including port 80. A public
address using a nondefault port includes it, for example
`https://gateway.example.com:8443`; workers and the Telegram webhook use that
same address. See the [Telegram webhook requirements](https://core.telegram.org/bots/api#setwebhook).

If you manage the proxy yourself, forward it to the selected local port
(`127.0.0.1:8080` by default), including WebSocket upgrades. The
[nginx template](../deploy/nginx/telegramgw.conf) routes `/tgadmin/`,
`/tgapi/`, `/tghealthz`, and `/tgreadyz`. Preserve these paths when forwarding:
workers connect at `/tgapi/v1/workers/connect` and Telegram delivers updates
to `/tgapi/v1/telegram/webhook`.

Once public HTTPS passes its checks, the wizard registers the Telegram webhook
and command menu, then creates a first administrator's one-time token if needed.
Open the printed `/tgadmin/` address, register a passkey within 15 minutes, and
enroll a worker to obtain its ID and token. Existing administrator passkeys are
preserved when completion is rerun.
If an existing gateway predates guided administrator setup, the wizard prints
`sudo codex-telegramgw update gateway`; run that command when convenient, then
run `sudo codex-telegramgw finish gateway` again. Existing administrators can
continue signing in while that step is pending.

For manual maintenance, the equivalent gateway administration helper loads the
private environment through systemd and runs as the service account:

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

The last command prints a one-time token to your terminal. Use the guided finish
command for normal setup; this helper is also useful when manually changing
the webhook or command menu.

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
- If neither exists, it checks your user service session, offers to install Codex
  if needed, and guides Codex sign-in. It asks for the gateway address, enrolled
  worker ID and token, worker name, and initial working directory. Invalid
  entries can be corrected without restarting the wizard; token entry is hidden.
- A newly generated configuration allows the user's entire home directory and
  its subfolders. The initial working directory selects where the primary runtime
  starts; it does not restrict access to that one project. Explicit workspace
  restrictions in prepared or existing configurations are preserved. Choosing an
  initial directory outside your home adds that directory as an allowed root,
  alongside your home. Symlinks pointing outside an allowed root do not expand
  access; configure another root explicitly when needed.

The prompts use the terminal directly, so they work when the script is piped
into `sh`. Without an interactive terminal, provide `worker.json` beforehand.
You can also run the downloaded script with `sh /tmp/codex-telegramgw-install.sh`
or run the native manager's `codex-telegramgw setup worker` command.

This installs both `codex-worker` and `codex-local` for the current user and sets
up the worker service. Relative configuration paths are resolved before the
configuration is installed.

The worker stores its command ledger, event outbox, and conversation statistics
cache in embedded SQLite at the configured `state_file`. The Go binary includes
the database engine; installing SQLite, Python, a database server, or a separate
C library is unnecessary. Existing bbolt state is converted automatically at
startup without changing the configured path. A private `.bbolt-backup` copy is
retained before conversion; see [worker storage and recovery](operations.md#worker-storage-and-ambiguous-outcomes)
before restoring a backup or rolling back to an older binary.

For a new Linux service, the wizard asks whether to restrict the worker. The
default is **yes**. Restricted mode enables `PrivateTmp`, `PrivateUsers`, and
`NoNewPrivileges`; for example, a command such as `sudo apt update` cannot
elevate privileges from the worker. Answer **no** for full system access, which
installs these settings:

```ini
[Service]
PrivateTmp=no
PrivateUsers=no
NoNewPrivileges=no
```

Full access uses the Linux account's existing permissions and sudo rules. It
does not grant sudo privileges or change Codex session permissions or configured
workspace roots. Updates, adoption, and repeated setup preserve the existing
service's access settings. macOS services use the account's normal permissions;
the Linux restriction choice does not apply to launchd.

On Linux, setup enables systemd lingering so the
worker can stay online after logout and start at boot. If administrator access
is needed, it offers to run `sudo loginctl enable-linger` and verifies the result.
Declining leaves the worker installed but it may stop after logout. On macOS,
the worker starts when the user signs in to the desktop.

Setup prints immediately usable `codex-worker status`, `doctor`, and
`attach --latest` commands with full executable paths. If `~/.local/bin` is not
on your terminal's `PATH`, it also prints:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Run that line in your current terminal and add it to your shell's startup file
for future terminals. The installer cannot change the environment of the shell
that launched the curl command.

For unattended installation with a configuration at another location, the
explicit command is still available:

```sh
sh /tmp/codex-telegramgw-install.sh install worker \
  --config /path/to/worker.json \
  --auto-update
```

Prepared or unattended worker installations require an existing authenticated
Codex executable. Fresh interactive setup can install and authenticate it for
you using OpenAI's standalone installer; Node.js, npm, Python, and a compiler are
not required. Existing Codex installations and credentials are reused.

For a new unattended Linux installation, add `--service-access full` to the
explicit `install worker` command to select full access, or
`--service-access restricted` for restricted access. Omitting it uses restricted
access. This option applies to installing a new Linux service; it cannot change
an existing service's settings through an update or adoption.

Once worker automatic updates are enabled, the updater also checks the official stable Codex
release channel once per UTC day. User-owned standalone installations are
updated through the native `codex update` command. Installations managed by npm,
Homebrew, or a manually pinned release retain their existing update mechanism.

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

# Check or apply only the worker's Codex runtime maintenance:
codex-telegramgw update codex --check
codex-telegramgw update codex
```

Checks fetch release metadata, the checksum manifest, and its signed build
provenance to show whether a newer stable release is available. They verify the
manifest signature but do not download the component archives.
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

### Request worker updates from Telegram

Send `/tgupdateworkers` without arguments. It queues a check for every enabled
worker, including offline workers, without requiring or changing the selected
session. Repeated requests share an existing pending update. Offline workers
receive it when they reconnect, and requests survive gateway and worker restarts.

Each worker checks the configured GitHub release repository and waits until all
its turns and pending work finish before installing a newer release and
restarting. A worker already on the latest version reports that result without
restarting. Results return to the chat and topic that requested the update.
Temporary download failures are retried; a failed request reports its outcome
and can be submitted again.

This command requires gateway and worker v0.5.29 or later. An older worker
reports that it first needs a local `codex-telegramgw update worker`. Requested
updates use an independent updater service and do not change automatic update
schedules. They update the worker and its local helper; the separate daily
Codex runtime check continues on its normal schedule.

### Upgrade to the `tg` URL routes

The gateway serves the console at `/tgadmin/`, API endpoints under `/tgapi/v1/`,
and health checks at `/tghealthz` and `/tgreadyz`. Previous unprefixed routes
return 404. Update proxy rules and external health checks together with the
gateway; preserve WebSocket support for `/tgapi/v1/workers/connect`.

Plan this first route change across the gateway and its workers. Finish active
turns, then stop each worker from a separate terminal before changing the
gateway. On Linux, use `systemctl --user stop codex-worker.service` as the
worker's owning user. This also stops its app servers, so do not run it from
a turn hosted by that worker.

Use the bootstrap to run the new manager for this gateway update. Older managers
continue checking the previous readiness URL even after installing a new binary:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sudo sh -s -- update gateway
```

Reload the matching proxy configuration, then run `gateway webhook set` using
the helper from [Finish gateway setup](#finish-gateway-setup) so Telegram sends
updates to the new URL. Update each stopped worker as its owning user with
`codex-telegramgw update worker`; the updater starts it again. New workers
automatically normalize the previous standard WebSocket URL when loading their
configuration. Custom endpoints are preserved. Keep the public HTTPS origin
unchanged; existing worker credentials, saved sessions, and passkeys remain valid.

Verify `/tgreadyz`, worker reconnection, and bot webhook status in `/tgadmin/`
before resuming turns.

### Updating running workers

Worker updates require cooperation from the running worker. It must have no
active or waiting turns, queued work, unanswered native CLI requests or approvals,
or outstanding events awaiting gateway acknowledgement. An attached, idle CLI
does not prevent an update, and a completed CLI connection leaves no permanent
blocker. The worker tracks native JSON-RPC requests and asynchronous work, including
requests whose terminal disconnected before the server replied.

During a prepared update, new commands and terminal attachments are fenced until
the worker restarts or the preparation expires. A request sent through an existing
CLI during this interval receives an explicit retry error and is not executed.
Busy workers defer the update and log the specific reason. Unknown protocol
activity and requests whose completion cannot be verified still defer updates.

Routine Codex notifications do not permanently block updates. After a temporary
thread `systemError` or loss of an otherwise settled native connection, the next
attempt fences new requests and rechecks live thread state, worker sessions, and
queued work. It clears the observation gap only if those checks succeed without
new native activity arriving. A lost connection with only known read-only requests
can also recover this way. Native prompt queues are checked explicitly, including
queues observed on threads outside the gateway's workspace list. These checks
run when preparing an update, without extra history polling.
Active turns, pending questions, unconfirmed modifying RPCs,
and unsupported asynchronous operations still block a restart; an idle snapshot
alone cannot prove those operations finished. Deferred-update logs include the
kind of protocol problem without recording prompts or RPC payloads.

The worker publishes a stable private attachment socket across restarts. Compatible
Codex TUIs can use their native reconnect/resume handling at that address; the
gateway never replays prompts or RPCs. If the terminal cannot reconnect, attach
again with `codex-worker attach SESSION` after the worker is ready.

Older workers that block every native CLI attachment need a one-time controlled
upgrade. Finish active turns, exit the attached CLI, stop the old worker service,
and run the worker update command below. Then attach again so the CLI uses the new
stable socket. The same first-upgrade procedure applies to workers predating the
update coordination protocol. Subsequent idle CLI updates can run automatically.
Workers already stuck on `native CLI activity could not be verified` from an
older version also need this one-time upgrade: the running process must load the
new guard before it can recover automatically.

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

Guided gateway and worker setup enable a daily scheduled check for stable releases automatically.
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

Worker update checks also maintain the Codex runtime. Stable release discovery
is limited to one automatic check per UTC day, even if a local timer runs more
frequently. The private `codex-update.json` beside the worker's `update.json`
records the last attempt and available version. A failed check is retried on the
next daily check; a discovered update is reconsidered on every worker update
tick until the live worker can confirm it is idle. `--check` is read-only and
uses a cached release when the daily check is not yet due.

Runtime installation uses the same idle reservation as worker maintenance,
including native CLI turns, approvals, pending requests, and queued Telegram
work. The worker stops before the native standalone updater downloads and
installs Codex, then starts its app servers again. The updater verifies fresh
worker readiness and the actual runtime versions before completing. Failures or
cancellation trigger an attempt to restore the worker service, and an
interrupted restart remains pending for the next updater invocation. A worker
that was already stopped is left stopped.

If Codex's own updater has already changed the installed launcher, the gateway
updater still detects an older running app server and schedules its restart
once idle. Runtime profiles sharing the same standalone installation are
updated together. Manually pinned executables and package-manager installations
are skipped. The native Codex installer uses its normal shell/download/archive
tools; this procedure adds no Python dependency. Gateway-only hosts do not
check or update Codex. Disabling worker automatic updates also disables these
scheduled runtime checks; the gateway and worker timer defaults are unchanged.

A runtime degraded by a missing protocol method retries discovery once per
minute. It returns to healthy status only after those methods and a complete
discovery pass succeed; work is not interrupted by these probes.

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

The release workflow runs formatting, vet, govulncheck, migration and race tests,
all platform builds, and archive verification. CI uses the latest Go 1.26 patch
(at least 1.26.8); a daily scheduled CI run checks for newly published advisories. Only after those checks pass does it publish
the version's binaries, installer, update manager, and checksum manifest. It
uploads a draft first so automatic updaters never select a partially uploaded
release. Published releases are not overwritten; corrections use a new version.

Each release publishes ten component archives, four native manager executables
(`codex-telegramgw-{linux,darwin}-{amd64,arm64}`), `install.sh`, `SHA256SUMS`, and
`SHA256SUMS.sigstore.json` (17 files total).
Every component archive contains the matching native manager and inner checksums.
Packaging and verification use the Go release tool. Python remains only in the
repository's historical PostgreSQL migration tools and their CI tests; those
tools are not shipped in the release archives.

The native manager verifies the manifest's Sigstore build attestation before
accepting its hashes or executing downloaded programs. It requires GitHub's OIDC
issuer and this repository's exact `.github/workflows/release.yml` identity at
the selected version tag, plus certificate, transparency-log, timestamp, and
artifact-digest verification. Trust roots are compiled into the manager; release
assets cannot substitute them. Verification uses Go and needs no `gh`, Python,
OpenSSL, or external verifier on the installed machine. The release workflow is
pinned to immutable Action commits and verifies the same policy before publishing.

New managers reject unsigned legacy releases, including explicit version pins.
Previously installed managers can adopt the first signed release using their
existing checksum verification; subsequent updates require provenance. The initial
curl bootstrap still trusts GitHub HTTPS and the installer/manager it retrieves.
For a separately trusted first install, review and build the manager from a trusted
source checkout. Signatures do not protect against an authorized change to the
trusted release workflow: repository write access remains deployment access.
Trust-root rotations require a reviewed manager update before old roots expire.

Privileged gateway updates reject non-root-owned or group/world-writable existing
executables. After stopping the gateway, the manager copies SQLite and any WAL or
rollback journal through a retained, confined directory descriptor into a private
staging directory before creating a verified backup. It rejects symlinks, hard
links, and files changed during copying; working database paths are never reopened
by privileged SQLite backup code.

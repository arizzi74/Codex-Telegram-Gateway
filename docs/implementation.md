# Implementation and verification tracker

The gateway and worker implement the specification in Go, with changes committed
in incremental steps. Deployment addresses and local installation details belong
in private operator configuration.

## Completed milestones

- [x] Configuration, versioned protocol, migrations, enrollment, WSS, heartbeat.
- [x] Codex adapter, discovery, supervised runtimes and degraded compatibility.
- [x] Telegram webhook authorization, deduplication, selection and immutable routing.
- [x] Durable command ledger, event outbox, replay, generations and uncertain outcomes.
- [x] Approval/input controls, steering, interruption and independent session queues.
- [x] Passkey admin console, one-time bootstrap and credential management.
- [x] Local CLI attachment and current-directory launch/resume helper.
- [x] Service/proxy configuration, portable installer, documentation and release artifacts.
- [x] AT-01 through AT-20 verification: automated integration, local CI and live drills.
- [x] Live deployment and requirement audit.

## Verification

The gateway was migrated to SQLite and deployed as version 0.3.0 on 2026-09-15.
The import verified all 453 source rows across 20 tables before startup. Live
health/readiness, worker reconnection and acknowledgements, unchanged runtime
PIDs, Telegram menu/webhook, and database integrity checks passed. The worker
continues running its existing binary and Codex runtime; no worker restart was
required. Private backups retain the former PostgreSQL state and configuration.


Go 1.26.5 linux/arm64 and Codex CLI 0.154.0. The gateway now uses a local
SQLite file; workers retain their bbolt stores. `VERSION=0.3.0 ./scripts/ci.sh`
runs formatting, vet, migration tests, race tests, cross-builds, and archive
checksum verification without a database server. See
[the acceptance ledger](acceptance.md) for named tests and their limits.

The gateway enforces the numeric Telegram identity configured for the bot;
other actors are rejected. The current deployed worker is enrolled and
connected using its private worker credentials. A real database outage made
readiness fail and recover while the worker and Codex remained alive. A real
owned Codex process exit advanced its runtime generation and preserved session
identities.
The private shared transport and terminal helper were exercised with the
installed Codex version. No model turns were submitted by live smoke tests.

The GitHub Actions workflow runs the same local CI job on pushes and pull requests. Darwin executables were cross-built with
CGO disabled; the LaunchAgent was not run on a Darwin host.

## Owner setup

The first personal passkey requires the owner's authenticator. Run
`codex-gateway --config /path/to/gateway.json admin bootstrap` as the gateway's
service account, then open `/admin/` at the configured public HTTPS origin and
register. The implementation was tested
with a synthetic authenticator; production contains no fabricated passkey.

CLI sharing is the user's explicit extension to direct app-server stdio:
[ADR 0008](adr/0008-shared-local-cli.md) records the private Unix socket and
stdio byte-proxy/WebSocket framing bridge. Codex itself has no public listener.


## 2026-09-14 command update

Added `/tg` gateway names, typed Codex slash commands, a published bot menu,
transient typing indicators and precise writer-lock diagnostics. `/status`
includes selected-session recorded context usage. New/fork completion compares
selection revisions before automatic selection, preserving later user choices.
See [Telegram commands](telegram-commands.md) for syntax and terminal-only limits.

## 2026-09-14 temporary progress update

Plain prompts no longer enqueue a Telegram acknowledgement. Completed Codex
commentary items produce durable progress events; item phase distinguishes
commentary from the final answer. The sender checkpoints temporary message IDs
and deletes them after the full terminal response reaches the same destination.
Cleanup has its own retry loop and survives gateway restarts. Late progress is
suppressed, and failure/interruption also clean up the completed turn.

The owner authorized removing the worker restriction that prevented `sudo` and
restarting the service. The installed worker unit and repository template now
set `NoNewPrivileges=no` and `ProtectSystem=no`, retaining `PrivateTmp=yes` and
`UMask=0077`. The gateway service keeps its existing restrictions. A new
transient service launched by the user service manager with the worker settings
successfully ran `sudo -n id -u` and returned `0`.

Maintenance completed through the user service manager, outside the worker's
process group, after all turns finished. It activated the verified 0.2.1 release
with:

```sh
scripts/deploy-host-update.sh --wait
```

The helper verifies release checksums and waits up to ten minutes for running
or waiting turns to finish. It installs the release, restarts the gateway and
worker, and verifies reconnection. `--check` performs a read-only preflight.
The replacement worker and Codex runtime both report `NoNewPrivs: 0`; sudo
works from new worker-hosted commands. Gateway and worker are running version
0.2.1 with matching release checksums, a connected worker, acknowledged events,
healthy HTTPS endpoints, and the 69-command Telegram menu intact.

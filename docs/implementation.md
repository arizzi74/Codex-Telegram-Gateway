# Implementation and verification tracker

The specification and the user's additions are implemented in Go and committed
in incremental steps. The gateway and local worker are deployed on
`gateway.example.com`.

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

Go 1.26.5 linux/arm64 and Codex CLI 0.154.0. PostgreSQL 14 runs locally.
`TEST_DATABASE_URL=... VERSION=0.1.0 ./scripts/ci.sh` passes formatting, vet,
race tests, cross-builds and archive checksum verification. See
[the acceptance ledger](acceptance.md) for named tests and their limits.

The gateway enforces the numeric Telegram identity configured for the bot;
other actors are rejected. The current deployed worker is enrolled and
connected using its private worker credentials. A real database outage made
readiness fail and recover while the worker and Codex remained alive. A real
owned Codex process exit advanced its runtime generation and preserved session
identities.
The private shared transport and terminal helper were exercised with the
installed Codex version. No model turns were submitted by live smoke tests.

The local CI job was run; the hosted workflow has not been triggered because
this local repository has no remote. Darwin executables were cross-built with
CGO disabled; the LaunchAgent was not run on a Darwin host.

## Owner setup

The first personal passkey requires the owner's authenticator. Run
`sudo /usr/local/sbin/codex-gateway-admin admin bootstrap`, then open
`https://gateway.example.com/admin/` and register. The implementation was tested
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

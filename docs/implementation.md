# Implementation and verification tracker

The source of truth is `CODEX_TELEGRAM_CONTROL_PLANE_SPEC.md`, plus the user's
explicit additions: passkey administration, CLI attachment and start/resume
helpers, static stripped binaries, incremental commits, and deployment on
`gateway.example.com` with the single numeric Telegram identity from `.botsecrets`.

## Milestones

- [ ] Foundation: configuration, protocol, migrations, enrollment, WSS, heartbeat.
- [ ] Codex adapter: stdio RPC, initialization, discovery, supervision.
- [ ] Telegram: webhook authorization, dedupe, selection and immutable routing.
- [ ] Durability: ledger, outbox, replay, generations, uncertain outcomes.
- [ ] Controls: approvals, input, steering, interruption, per-session queues.
- [ ] Passkey admin UI and enrollment/recovery flow.
- [ ] Local CLI attach and launch/resume helpers.
- [ ] Operations: services, nginx, configuration, documentation, release artifacts.
- [ ] Mandatory acceptance tests AT-01 through AT-20.
- [ ] Live deployment and requirement-by-requirement completion audit.

## Environment evidence

Initial inspection: Go 1.26.5 linux/arm64; Codex CLI 0.154.0. No existing
implementation or Git history. The supplied bot secret file is mode 0600.
The host has nginx; PostgreSQL availability has not yet been established.

## Policy additions

The user's passkey UI request overrides the specification's browser UI non-goal.
Public Codex app-server exposure remains prohibited. CLI attachment must stay
local and preserve the stdio adapter boundary; implementation will be documented
in an ADR once validated against the installed runtime.

## Validation

No milestone or acceptance test has passed yet. Tests must use fake Codex and
Telegram servers without external model credentials. Live Telegram messaging
requires the user's existing authorization for bot operation and must stay
within the configured allowlist.

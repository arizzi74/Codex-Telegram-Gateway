# Acceptance evidence

This is an evidence ledger, not an implementation checklist. A requirement is
only marked **verified** where a current test or an observed live check proves
the stated behavior. **Partial** means useful coverage exists but does not
prove every part of the acceptance scenario. **Pending** requires a live or CI
run that has not been recorded here.

The PostgreSQL tests below require `TEST_DATABASE_URL`. They create a fresh
schema and use migrations, so they exercise the real registry database without
using a shared test schema.

## Temporary Telegram progress (2026-09-14)

Version 0.2.1 passes the full local `scripts/ci.sh` job: formatting, vet,
race tests with PostgreSQL, ten release archives, and inner/outer checksums.

- `TestControlPlaneWorkerOutboxSurvivesGatewayRestartIntegration` verifies
  quiet prompt acceptance, two visible commentary messages during an active
  turn, gateway restart, one permanent final answer, and deletion of only the
  temporary message IDs after the final send.
- Registry integration tests verify all final chunks must be checkpointed
  before cleanup, retry leases survive a fresh Store, late progress is
  suppressed, and cleanup stays within the same bot/chat/topic/session/turn
  and runtime generation. Failure and interruption use the same cleanup flow.
- Gateway tests verify silent progress messages, precise deletion targets,
  Telegram retry-after handling, already-deleted messages, and independent
  retries that never resend the final answer.
- Worker tests distinguish commentary from final answers using Codex item
  phase, preserve older servers with no phase, reject stale turn messages,
  and redact progress before it enters the durable outbox.

The owner authorized removing the worker restriction and restarting it. The
installed unit and repository template now set `NoNewPrivileges=no` and
`ProtectSystem=no`; `PrivateTmp=yes` and `UMask=0077` remain. After the user
manager reloaded the unit, a new transient service with the same settings ran
`sudo -n id -u` successfully and returned `0`.

The scheduled maintenance job completed successfully on 2026-09-14 at 08:49
CEST after the active turns finished. Gateway and worker report version 0.2.1;
all three installed binaries match the verified release. The replacement worker
is connected with no unacknowledged events, and its generation-7 Codex runtime
is running. Both processes report `NoNewPrivs: 0`, and `sudo -n true` succeeds
from a new worker-hosted command.

Post-deployment checks confirm HTTPS health/readiness and admin HTML return
200, the unauthenticated admin session endpoint returns 401, the Telegram menu
still contains all 69 commands, and the webhook has no pending updates or error.
The live registry is receiving temporary progress message checkpoints; cleanup
of the verification turn remains gated on delivery of that turn's final answer.

## AT-01 through AT-20

| ID | Status | Current evidence | Remaining proof |
| --- | --- | --- | --- |
| AT-01 Worker registration | Verified | `internal/gateway:TestAT01RegistrationAT02CredentialRejection` connects over TLS WSS with an enrolled token and checks online state plus OS, architecture, version, and hostname in PostgreSQL. The current deployed worker is also enrolled and connected. | None; repeat the live enrollment check after a topology change. |
| AT-02 Invalid Worker credential | Verified | The same test rejects an invalid bearer token before metadata is recorded; `TestWorkerHelloCannotClaimAnotherIdentity` also rejects a valid token paired with another worker ID. | None for automated acceptance. |
| AT-03 Discover sessions | Verified | `internal/worker:TestControlPlaneWorkerOutboxSurvivesGatewayRestartIntegration` starts the fake App Server with three threads, waits for all three in `registry.SessionSnapshot`, posts actual `/tgsessions <runtime-id>` through the webhook, and verifies the Telegram UI contains Alpha, Beta, and Gamma. `TestRuntimeDiscoverySubscribesOnlyLoadedWorkspaceThreadsOnce` covers loaded-thread reconciliation. | None. |
| AT-04 Select session without affecting execution | Verified | The control-plane integration test starts A, selects B through Telegram, then verifies A retains its active turn and no additional `turn/start`, `turn/interrupt`, or `thread/resume` RPC occurs. | None. |
| AT-05 Deterministic queued routing | Verified | `internal/registry:TestAcceptTelegramRoutingPriorityAndFrozenCommandIntegration` accepts a command, changes selection, then verifies the persisted command retains its original session and thread. The `commands_preserve_routing` trigger enforces this in the database. | None for the persistence invariant. |
| AT-06 Reply routing | Verified | The same routing integration test records a bot-message route for A, changes selection, and verifies a reply goes to A. Multi-question reply addressing is also covered by `TestAcceptTelegramMultiQuestionReplyRoutingIntegration`. | None for the routing invariant. |
| AT-07 Duplicate Telegram update | Verified | `TestAcceptTelegramRoutingPriorityAndFrozenCommandIntegration` resubmits the same update ID and verifies the duplicate result; `telegram_updates` has the `(bot_id, update_id)` primary key and the accept transaction inserts it first. | None. |
| AT-08 Duplicate Gateway command delivery | Verified | `internal/worker:TestAgentAcceptsDuplicateAndRejectsStaleGenerationWithoutRPC` delivers the same command twice and proves exactly one `turn/start` RPC occurs. `Store.Receive` persists the ledger before acknowledgement. | None for worker-side deduplication. |
| AT-09 Gateway restart | Verified | `internal/worker:TestControlPlaneWorkerOutboxSurvivesGatewayRestartIntegration` uses real PostgreSQL, Hub, webhook, Dispatcher, Sender, worker WSS transport, bbolt, and fake App Server. It stops the gateway during a turn, emits a final event, verifies it in bbolt, restarts the gateway, reconnects, and verifies one Telegram final delivery. | None for the automated restart path. |
| AT-10 Worker temporary network loss | Verified | The control-plane integration test closes only the captured worker WSS transport while keeping the HTTPS server, Agent, and fake App Server alive. It verifies Registry `unreachable`, the same local active turn/runtime, reconnect to `online`, and the three-session reconciliation snapshot. | None. |
| AT-11 Stale runtime command | Verified | `internal/worker:TestAgentAcceptsDuplicateAndRejectsStaleGenerationWithoutRPC` verifies `rejected_stale_runtime` and that stale control does not reach Codex. `internal/registry:TestConnectionFencingAndRuntimeGenerationIntegration` covers persisted generation fencing. | None. |
| AT-12 Correct approval | Verified | `internal/worker:TestAgentApprovalRequiresExactPendingRequestAndClears` verifies an approval response addresses the exact pending App Server request. `internal/registry:TestAcceptTelegramCommandsCallbacksAndDispatchIntegration` verifies callback claim/race handling creates only one immutable approval command. | None for the exact-target invariant. |
| AT-13 Stale approval | Verified | The control-plane test captures the persisted Telegram approval button, has the fake App Server resolve its exact request, waits for Registry pending approval count to become zero, clicks the stale token, verifies the stale-button UI response, and verifies no App Server response is emitted. | None. |
| AT-14 Steering correct turn | Verified | `internal/worker:TestAgentControlExpectedTurnFence` checks the exact active turn is passed to `turn/steer` and rejects a changed turn. | None. |
| AT-15 Interrupt correct turn | Verified | `TestAgentControlExpectedTurnFence` applies the same expected-turn fence to interrupt. | None. |
| AT-16 Queue serialization | Verified | `internal/worker:TestAgentSessionFIFOAndIndependentSessions` verifies three same-session turns run FIFO and only release after completion. | None. |
| AT-17 Cross-session concurrency | Verified | The same agent test starts independent sessions concurrently while retaining the same-session one-active-turn limit. | None. |
| AT-18 Runtime crash | Verified | `internal/worker:TestRuntimeClientCrashStartsNextGeneration` closes the fixture transport and checks a new generation. The control-plane integration test repeats the crash through a connected worker and verifies its three persisted session identities recover. | Live 2026-09-13: worker survived owned Codex SIGTERM, generation 2→3, new PID, stable session identities and fully acknowledged outbox. |
| AT-19 Gateway database restart | Verified (live) | 2026-09-13: the PostgreSQL cluster was stopped. HTTPS `/healthz` remained 200 and `/readyz` became 503; the running worker and Codex PIDs remained alive and unchanged. After the cluster restart, `/readyz` returned 200. The control-plane test separately covers readiness/runtime isolation with an injected failed `Ping`. | Repeat after material deployment topology changes. |
| AT-20 Worker cross-platform build | Verified (local CI job) | 2026-09-13: `scripts/ci.sh` completed with PostgreSQL: format check, `go vet ./...`, `go test -race ./...`, all ten CGO-free cross-build archives, and inner/outer SHA-256 verification. It builds `codex-worker` for Linux/Darwin × amd64/arm64. `.github/workflows/ci.yml` invokes this exact job. | Hosted workflow has not been triggered because this repository has no configured remote. |

## Hard invariants and security review

| Requirement family | Evidence | Status |
| --- | --- | --- |
| Immutable Telegram target | `AcceptTelegram` takes an advisory transaction lock, deduplicates, resolves, and writes a command in one transaction. Migration `001_registry.sql` rejects routing-field updates. Routing integration tests cover selection, reply mapping, and duplicate updates. | Verified |
| Durable Class A events | `Store.AppendEvent` accepts only durable event kinds, assigns a monotonic sequence in the bbolt transaction, and `Connection` deletes only after `event_ack`. `RecordResult` persists command outcome and event together. `TestConnectionReplaysOutboxAfterReconnectAndAcknowledges`, `TestStoreReopenPreservesLedgerOutboxAndGeneration`, and AT-09 cover replay. | Verified |
| Gateway event ingestion | `Store.IngestEvent` fences the connection, verifies ordered/replayed sequence, writes event and projection transactionally, advances the watermark, then Hub sends `event_ack`. Registry event integration tests cover replay, rollback, stale generation, and cross-worker targets. | Verified |
| Command dedupe and uncertain outcomes | `Store.Receive` writes the immutable command before dispatch. Duplicate command IDs return their prior record. Startup converts `executing` records to `outcome_unknown` and emits `command_result_unknown`; store tests cover this recovery. | Verified |
| Runtime generation fence | Local generation is persisted before spawn; every command is checked by `Agent.dispatch` and the session actor. Registry also verifies connection and generation before command/callback operations. Worker and registry generation tests cover both fences. | Verified |
| Telegram authorization, secret and body bound | `Webhook.ServeHTTP` checks the webhook secret before JSON parsing, caps the body at 1 MiB, enforces numeric user/chat allowlists, and writes through the transactional registry acceptance path. Gateway/auth tests cover rejection and parsing. | Verified |
| Worker credentials and WSS identity | Enrollment tokens are 32 random bytes with only SHA-256 hashes stored; verification is constant-time. Worker configuration accepts only an absolute `wss://` gateway URL; Hub validates the bearer token before hello, binds it to one worker ID, disables compression, caps frames, and rate-limits failed authentication. | Verified |
| Local worker confinement | Worker requires a private token file and state file, only dials an absolute `wss://` endpoint, has no listener, and canonicalizes allowed workspace roots before `/new` or resumed work. Auth/config/runtime tests cover traversal, symlink escape, public listener rejection, and rejected workspace. | Verified |
| Passkey-only administrator | `internal/admin` requires the exact configured HTTPS origin, RP ID `gateway.example.com`, discoverable resident credentials and required user verification. Bootstrap and ceremony/session secrets are random, hashed at rest, short-lived, one-use, and bound to Strict secure cookies. Mutations use exact Origin plus double-submit CSRF; CSP permits self assets only. `TestAdminBootstrapCeremonyReplayAndExpiryIntegration` and `TestP256PasskeyAssertionRequiresUserVerification` cover durable replay/expiry and cryptographic UV enforcement. | Verified by the full HTTP registration/login test with a synthetic P-256 authenticator; personal enrollment remains an operator setup step. |
| Process and proxy controls | Config rejects non-loopback gateway listeners. The systemd gateway unit runs a dedicated unprivileged account; worker units run as the local user. Nginx is the TLS reverse-proxy boundary. | Verified on the Linux deployment; macOS service template/installer is supplied and cross-built, without a live macOS host. |
| Release integrity | Release script uses `CGO_ENABLED=0`, `-trimpath`, archives README/LICENSE/examples/deploy, and writes archive checksums. The 2026-09-13 local CI job verified all ten archives and their inner/outer checksums. | Verified locally; hosted workflow untriggered because no remote is configured. |

## Future topology rechecks

1. **Target-host controls.** The systemd/launchd units and nginx configuration
   are reviewed artifacts; repeat the live service-account, loopback, TLS, and
   log checks when topology changes.

## Deployment verification (2026-09-13)

- [x] 2026-09-13 local `scripts/ci.sh`: race suite, vet/format, ten
  cross-build archives and inner/outer checksum verification. The same script
  is the hosted workflow job; no hosted trigger was run because no remote is
  configured.
- [x] Gateway host: dedicated `codexgateway` service account, loopback listener,
  HTTPS `/healthz` and `/readyz` 200, admin HTML 200/API 401 without authentication,
  nginx configuration check and reload. Full passkey registration/login and
  rejection flows pass HTTP tests with a synthetic authenticator. First personal
  passkey enrollment is an operator setup step; no personal credential was fabricated.
- [x] 2026-09-13 PostgreSQL outage/recovery drill for AT-19: `/healthz` 200,
  `/readyz` 503 then 200, worker and Codex PIDs unchanged.
- [x] Automated WSS-loss/reconnect drill for AT-10, including Registry
  `unreachable` state and post-reconnect session reconciliation.
- [x] Live worker runtime SIGTERM drill for AT-18: worker stayed alive,
  generation 2→3, new owned process, stable session IDs, outbox fully ACKed.
- [x] Live Telegram configuration: correct HTTPS webhook, zero pending updates,
  no reported error; missing secret and invalid worker token rejected with 401,
  correct webhook secret with wrong numeric actor rejected with 403.
- [x] Private Codex socket 0600 in 0700 directory.
- [x] Local terminal smoke: fresh directory reaches Ready on the owned shared
  server and exits cleanly; resuming a thread held by another app reports its
  active-writer conflict. No model turn was submitted during these smoke checks.


## Telegram command update (2026-09-14)

Gateway commands now use the `/tg` namespace. Unprefixed commands are typed
Codex client actions, with terminal-only commands documented in
[Telegram commands](telegram-commands.md). The bot menu contains both groups.

Verification includes:

- The full control-plane integration sends `/status` during an active turn and
  `/tgstatus` through the actual webhook, worker transport and Telegram sender.
- Registry tests prove slash targets stay frozen, results reach only their
  requesting chat, fork results preserve source ownership, and delayed new/fork
  results cannot overwrite a later selection or disconnect.
- Typing tests cover the Bot API payload, four-second refresh, reconstruction
  from durable state, active-turn completion and waiting for user input.
- Worker tests preserve active-turn correlation through review/init and through
  read-only commands during a turn; invalid syntax is a definite failure.
- Live Codex 0.154.0 checks verified read-only model/configuration/rate limits,
  skills/hooks/plugins, and isolated thread model/effort, plan, sandbox,
  background-terminal, MCP and committed installed-app operations.
- A catalog exceeding 8 MiB no longer tears down the local adapter; direct
  JSONL and private WebSocket framing share a bounded 32 MiB ceiling. Gateway
  WSS limits are unchanged. Plugin output is filtered and bounded.
- An isolated two-server smoke proved a saved cold thread can be forked while
  the source is writer-locked in another server. No live model turn was sent.

The reported production rejection was a separate Codex process holding the
selected thread's writer lock. It now yields actionable `session_busy`
instructions, including `/fork` and `/tgnew`, instead of a generic protocol error.

Deployment verification: version 0.2.0 gateway and worker services are active,
HTTPS health/readiness return 200, the unauthenticated admin session API returns
401, the webhook reports zero pending updates and no error, and Telegram
`getMyCommands` confirms all 69 registered commands. Installed binaries match
the verified static stripped release outputs. The local CI job passes race
checks with PostgreSQL, vet/format, all ten cross-builds and checksums.

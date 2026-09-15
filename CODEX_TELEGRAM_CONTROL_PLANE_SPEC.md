# Codex Telegram Control Plane
## Gateway, Registry, and Cross-Platform Worker — Implementation Specification

**Status:** Draft v1.0
**Intended implementer:** Codex / software engineering agent
**Primary language:** Go
**Target gateway OS:** Linux
**Target worker OS/architectures:** Linux amd64, Linux arm64, macOS amd64, macOS arm64
**Transport:** HTTPS/WSS between workers and Gateway; local stdio JSON-RPC between Worker and Codex App Server
**Database:** SQLite on Gateway; bbolt or equivalent pure-Go embedded durable store on Worker
**Last updated:** 2026-09-13

---

# 1. Purpose

Build a control plane that lets a user operate multiple Codex runtimes and Codex sessions from a single Telegram bot.

The system consists of:

1. **Gateway**
   - Public HTTPS service.
   - Receives Telegram webhook events.
   - Maintains authenticated WSS connections from Workers.
   - Routes Telegram commands to the correct Worker and Codex session.
   - Sends Codex events, results, questions, and approvals back to Telegram.

2. **Registry**
   - Logical service implemented inside the Gateway for v1.
   - Persists Workers, Codex runtimes, sessions, Telegram bindings, commands, events, pending approvals, and delivery state.
   - Uses SQLite.
   - Must support deterministic routing and recovery after Gateway restart.

3. **Worker**
   - Cross-platform daemon running on Linux or macOS, amd64 or arm64.
   - Connects outbound to the Gateway over authenticated WSS.
   - Starts and supervises local Codex App Server processes.
   - Talks to Codex App Server locally using stdio JSON-RPC/JSONL.
   - Reconciles local Codex threads with the Gateway Registry.
   - Executes Gateway commands and forwards Codex events.
   - Persists a local command ledger and event outbox so a temporary Gateway outage does not lose important state.

This specification intentionally separates:

- **Worker**: a daemon on one computer.
- **Runtime**: one supervised `codex app-server` process.
- **Session**: one Codex thread.
- **Telegram Binding**: the currently selected Session for a Telegram context.

Do not use the word "session" for a process.

---

# 2. Implementation Decision: Go

Go SHOULD be used for all custom components.

## 2.1 Why Go

Go is a particularly good fit because:

- Linux and macOS are first-class Go targets.
- `amd64` and `arm64` are first-class architectures.
- Gateway and Worker can share protocol/domain packages.
- Worker binaries can be distributed as single executables.
- Concurrency maps naturally to:
  - one WSS connection per Worker,
  - per-runtime JSON-RPC pumps,
  - per-session command coordinators,
  - heartbeat and retry loops.
- The standard library provides strong HTTP, TLS, subprocess, context, JSON, logging, and cryptographic support.
- The design can avoid CGO, making cross-compilation and release packaging substantially simpler.

Required build targets:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
```

Build with:

```text
CGO_ENABLED=0
```

unless a future dependency explicitly requires otherwise.

## 2.2 Recommended dependency policy

Prefer the Go standard library where reasonable.

Suggested external dependencies:

```text
github.com/coder/websocket       # Worker <-> Gateway WebSocket
modernc.org/sqlite              # pure-Go SQLite
go.etcd.io/bbolt                 # Worker durable ledger/outbox
github.com/google/uuid           # UUID generation, optional
```

Database migrations MAY use:

```text
github.com/pressly/goose/v3
```

Do not add a large web framework for v1.

Use:

```text
net/http
log/slog
context
encoding/json
os/exec
crypto/*
```

as the core stack.

---

# 3. Important Codex Integration Constraint

The Worker MUST communicate with local Codex App Server using its local **stdio** transport.

Do **not** expose Codex App Server directly to the Internet.

Do **not** use Codex App Server's remote WebSocket transport as the Worker-to-Gateway protocol.

Rationale:

- Codex App Server exposes a JSON-RPC interface suitable for custom clients.
- Codex documents stdio as a supported local transport.
- Its WebSocket transport is currently marked experimental/unsupported for production.
- Hiding the Codex protocol behind a Worker adapter protects the rest of the system from Codex protocol changes.

The Worker therefore acts as an anti-corruption layer:

```text
Telegram
   |
   v
Gateway + Registry
   |
   | custom WSS protocol
   v
Worker
   |
   | local stdio JSON-RPC / JSONL
   v
Codex App Server
```

---

# 4. High-Level Architecture

```text
                               Internet
                                  |
                                  v
                    +---------------------------+
                    | HTTPS reverse proxy       |
                    | Caddy / nginx / LB        |
                    +-------------+-------------+
                                  |
                                  v
                    +---------------------------+
                    | codex-gateway             |
                    |                           |
Telegram ---------->| Telegram Webhook          |
                    | Command Router            |
                    | Worker WSS Hub            |
                    | Registry Service          |
                    | Telegram Renderer         |
                    | Approval Coordinator      |
                    | Dispatcher                |
                    +-------------+-------------+
                                  |
                                  v
                         +----------------+
                         | SQLite     |
                         +----------------+

                    authenticated outbound WSS
                    /                       \
                   /                         \
                  v                           v
        +------------------+        +------------------+
        | codex-worker     |        | codex-worker     |
        | MacBook          |        | Linux server     |
        +--------+---------+        +--------+---------+
                 |                           |
         stdio JSON-RPC              stdio JSON-RPC
                 |                           |
         +-------v--------+          +-------v--------+
         | Codex Runtime |          | Codex Runtime |
         +-------+--------+          +-------+--------+
                 |                           |
          Codex threads               Codex threads
```

The Gateway and Registry are logically separate but MUST be deployed as one binary/service in v1.

Suggested executable names:

```text
codex-gateway
codex-worker
```

---

# 5. Non-Goals for v1

The following are explicitly out of scope:

- Browser-based administration UI.
- Kubernetes orchestration.
- Redis/Kafka/NATS.
- Direct remote access to Codex App Server.
- Automatic remote shell access to Worker hosts.
- Peer-to-peer Workers.
- Multi-region Gateway replication.
- Windows Worker support.
- Arbitrary attachment synchronization between hosts.
- Exactly-once execution guarantees.
- Automatic conflict-free editing of the same checkout by multiple sessions.
- Transparent takeover of arbitrary already-running terminal Codex processes that were not started or registered by the Worker.

The architecture MUST leave room for these later without requiring a rewrite of the core routing model.

---

# 6. Terminology and Identity

## 6.1 Worker

One installed daemon on one host.

Stable identifier:

```text
worker_id UUID
```

A Worker MUST preserve its `worker_id` across restarts.

Example:

```text
worker_id: 85303ed2-4cc8-4bea-a682-d289512bcb35
name: example-macbook
hostname: worker.example.com
os: darwin
arch: arm64
```

## 6.2 Runtime

One locally supervised Codex App Server process.

Stable Registry identifier:

```text
runtime_id UUID
```

Every time the Worker restarts a Codex App Server process, increment:

```text
runtime_generation uint64
```

The pair:

```text
(runtime_id, runtime_generation)
```

uniquely identifies one live process generation.

Never use PID as identity.

PID is diagnostics only.

## 6.3 Session

One Codex thread.

Registry identity:

```text
session_id UUID
```

Codex identity:

```text
codex_thread_id string
```

Uniqueness:

```text
(runtime_id, codex_thread_id)
```

or, if threads are persisted across runtime generations:

```text
(worker_id, runtime_profile_id, codex_thread_id)
```

The implementation MUST allow a Session to survive a Runtime process restart when Codex can resume the persisted thread.

## 6.4 Telegram Context

A routing scope:

```text
telegram_bot_id
telegram_user_id
telegram_chat_id
telegram_message_thread_id nullable
```

The selected session is stored for this complete context.

Never maintain one global "current session".

## 6.5 Command

One immutable command accepted by the Gateway.

Stable identifier:

```text
command_id UUID
```

Its target Worker, Runtime generation, and Session MUST be resolved before dispatch.

## 6.6 Event

One Worker-to-Gateway message describing state or Codex output.

Every Worker connection uses monotonically increasing:

```text
event_seq uint64
```

Persist the sequence before sending if the event must survive reconnect.

---

# 7. Runtime Model

A Worker MAY supervise multiple Runtime profiles.

Example Worker configuration:

```yaml
worker:
  name: example-macbook

gateway:
  url: wss://codex.example.com/api/v1/workers/connect
  token_file: ~/.config/codex-worker/token

runtimes:
  - id: primary
    name: Primary Codex
    codex_binary: /opt/homebrew/bin/codex
    working_directory: /Users/USERNAME/dev
    autostart: true

  - id: client-project
    name: Client Project
    codex_binary: /opt/homebrew/bin/codex
    working_directory: /Users/USERNAME/dev/client-project
    autostart: false
```

For v1, a runtime profile launches:

```text
codex app-server --listen stdio://
```

or the current equivalent command defined by the installed Codex version.

The exact Codex invocation MUST be isolated in:

```text
internal/codexadapter
```

No Gateway code may depend on Codex JSON-RPC structs.

---

# 8. Repository Layout

Use one Go module and one repository.

Recommended layout:

```text
/
  cmd/
    codex-gateway/
      main.go
    codex-worker/
      main.go

  internal/
    gateway/
      server.go
      router.go
      telegram.go
      telegram_render.go
      worker_hub.go
      dispatcher.go
      approvals.go

    registry/
      workers.go
      runtimes.go
      sessions.go
      commands.go
      events.go
      bindings.go
      approvals.go
      store.go

    worker/
      agent.go
      connection.go
      runtimes.go
      sessions.go
      ledger.go
      outbox.go
      reconcile.go

    codexadapter/
      client.go
      process.go
      rpc.go
      events.go
      approvals.go
      types.go

    protocol/
      envelope.go
      messages.go
      errors.go
      version.go

    config/
      gateway.go
      worker.go

    auth/
      worker_token.go
      telegram.go

    observability/
      metrics.go
      logging.go

  migrations/
    0001_initial.sql
    ...

  packaging/
    systemd/
      codex-gateway.service
      codex-worker.service
    launchd/
      com.example.codex-worker.plist

  scripts/
    release.sh

  docs/
    protocol.md
    operations.md

  go.mod
  go.sum
  Makefile
  README.md
```

Rules:

- `internal/protocol` contains only the custom Worker/Gateway protocol.
- `internal/codexadapter` is the only package allowed to know Codex App Server wire details.
- `internal/registry` is the only package that performs Registry SQL.
- Telegram-specific types MUST NOT leak into Worker code.

---

# 9. Gateway Deployment

## 9.1 Network topology

The Linux host has public HTTPS access.

Recommended:

```text
Internet
  |
  v
Caddy/nginx
  |
  +--> /api/v1/telegram/webhook
  +--> /api/v1/workers/connect
  +--> /healthz
  +--> /readyz
        |
        v
codex-gateway 127.0.0.1:8080
```

TLS SHOULD terminate at Caddy/nginx in v1.

The Gateway itself SHOULD listen only on:

```text
127.0.0.1:8080
```

unless deployed behind a trusted private load balancer.

## 9.2 Public endpoints

Required:

```text
POST /api/v1/telegram/webhook
GET  /api/v1/workers/connect      # HTTP upgrade to WebSocket
GET  /healthz
GET  /readyz
```

Optional administrative endpoint:

```text
GET /metrics
```

`/metrics` MUST NOT be publicly exposed without access control.

---

# 10. Telegram Integration

Use Telegram webhook mode because the Gateway already has public HTTPS.

At startup, Gateway SHOULD verify webhook configuration and log discrepancies.

The Gateway MUST validate:

```text
X-Telegram-Bot-Api-Secret-Token
```

against a configured secret.

Use an allowlist of authorized Telegram numeric user IDs.

Optional additional allowlist:

```text
telegram_chat_id
```

Every incoming update MUST be rejected before command processing if the actor is unauthorized.

## 10.1 Telegram deduplication

Telegram `update_id` MUST be persisted.

Database uniqueness constraint:

```text
UNIQUE(bot_id, update_id)
```

Processing workflow:

```text
receive webhook
  -> authenticate webhook secret
  -> parse update
  -> authorize user/chat
  -> begin DB transaction
  -> insert telegram_update
  -> if conflict: return HTTP 200
  -> resolve command target
  -> persist immutable command
  -> commit
  -> return HTTP 200
  -> async dispatch
```

Do not block Telegram webhook responses on Codex execution.

## 10.2 Telegram commands

Required v1 commands:

```text
/start
/help
/instances
/sessions
/connect
/status
/disconnect
/new
/steer
/interrupt
```

Recommended later:

```text
/history
/rename
/restart
/logs
```

### `/instances`

Shows Workers and Runtimes.

Example:

```text
🟢 example-macbook
  Primary Codex
  3 sessions · 1 running

🟡 linux-dev
  Backend Runtime
  unreachable · last seen 27s ago
```

Buttons:

```text
[Sessions]
```

### `/sessions`

If no runtime argument is supplied and multiple runtimes exist, show runtime selection first.

Each session:

```text
auth-fix
Running · /Users/USERNAME/dev/api
[Connect] [Status]
```

Show loaded and persisted sessions distinctly.

### `/connect`

Selects a Session.

It MUST NOT start, stop, resume, or interrupt Codex merely because selection changed.

Example response:

```text
Connected to auth-fix

Host: example-macbook
Runtime: Primary Codex
Workspace: /Users/USERNAME/dev/api
Status: Running

Messages in this chat now target this session.
```

### `/status`

Shows:

```text
Worker
Runtime
Runtime generation
Session name
Codex thread ID abbreviated
cwd
Git branch if known
Connectivity
Session state
Active turn ID abbreviated
Pending approvals
Queued commands
Last event time
```

### `/new`

Syntax:

```text
/new
/new <runtime>
```

If a runtime has a configured default cwd, use it.

Future extension:

```text
/new <runtime> <workspace-alias>
```

Never accept arbitrary filesystem paths from Telegram unless an explicit workspace allowlist feature is enabled.

### `/steer`

Only valid while a turn is active.

Gateway MUST include the currently known active `turn_id`.

Worker MUST translate it to Codex:

```text
turn/steer
```

with:

```text
expectedTurnId
```

If the turn changed, reject rather than steering the wrong turn.

### `/interrupt`

Targets a specific active turn ID.

The immutable command MUST contain:

```text
expected_turn_id
```

Worker MUST refuse a stale interrupt that no longer matches the session's active turn.

## 10.3 Ordinary messages

A non-command text message:

1. Resolve selected Telegram Binding.
2. Reject if no selected Session.
3. Persist a `start_turn` command targeting that Session.
4. Dispatch asynchronously.

Default behavior while the Session is busy:

```text
queue for next turn
```

Do not silently reinterpret ordinary messages as `turn/steer`.

Telegram reply:

```text
Queued for auth-fix.
```

If idle:

```text
Sent to auth-fix.
```

## 10.4 Callback buttons

Telegram callback payloads are size-limited.

Do not serialize full routing state into callback data.

Persist a random opaque callback token:

```text
callback_token
```

Telegram receives:

```text
callback_data = "cb:<opaque-token>"
```

Database maps token to:

```text
action
actor constraints
session_id
command_id or approval_id
runtime_generation
expiry
```

Tokens MUST expire.

---

# 11. Telegram Routing Rules

Routing MUST be deterministic.

Priority:

1. Explicit command target encoded in a validated callback token.
2. Telegram topic/session binding, if topic mode is enabled.
3. Reply-to mapping from a previous bot message.
4. Current Telegram Binding.
5. Otherwise fail and ask the user to select a Session.

Never re-evaluate the active binding after a command is persisted.

## 11.1 Immutable command envelope

Example Registry representation:

```json
{
  "command_id": "0db20b54-b5d5-4ea0-a666-f9e40a519986",
  "source": "telegram",
  "telegram_bot_id": "primary",
  "telegram_update_id": 938221,
  "telegram_user_id": 123456,
  "telegram_chat_id": 123456,
  "telegram_message_thread_id": null,
  "worker_id": "85303ed2-4cc8-4bea-a682-d289512bcb35",
  "runtime_id": "2ae24ccb-51de-4e46-8054-a049188dd58a",
  "runtime_generation": 7,
  "session_id": "096d32a6-d17b-4c28-ac83-849dba2fd420",
  "operation": "start_turn",
  "expected_turn_id": null,
  "payload": {
    "text": "Fix the failing tests"
  }
}
```

After creation, these routing fields MUST NOT change:

```text
worker_id
runtime_id
runtime_generation
session_id
operation
expected_turn_id
```

---

# 12. Worker-to-Gateway Protocol

Use one long-lived authenticated WSS connection per Worker.

Endpoint:

```text
wss://codex.example.com/api/v1/workers/connect
```

The custom protocol is application-defined JSON messages.

Do not reuse Codex JSON-RPC directly.

## 12.1 Framing

One WebSocket text frame = one JSON object.

Common envelope:

```json
{
  "version": 1,
  "type": "event",
  "message_id": "uuid",
  "sent_at": "2026-09-13T14:00:00Z",
  "payload": {}
}
```

Required top-level fields:

```text
version
type
message_id
sent_at
payload
```

The protocol MUST reject unsupported major versions.

## 12.2 Connection authentication

Preferred v1 design: per-Worker high-entropy bearer token.

Header:

```text
Authorization: Bearer <worker-token>
```

Token requirements:

- minimum 256 bits random entropy,
- stored hashed in Gateway DB,
- stored in Worker config file with user-only permissions,
- individually revocable,
- rotatable.

Do not use one global token for all Workers.

The Gateway MUST bind the authenticated token to exactly one `worker_id`.

Future migration path:

```text
mTLS or signed short-lived Worker credentials
```

## 12.3 Initial handshake

After WebSocket upgrade:

Worker -> Gateway:

```json
{
  "version": 1,
  "type": "hello",
  "message_id": "...",
  "sent_at": "...",
  "payload": {
    "worker_id": "...",
    "worker_name": "example-macbook",
    "hostname": "worker.example.com",
    "os": "darwin",
    "arch": "arm64",
    "worker_version": "1.0.0",
    "protocol_min": 1,
    "protocol_max": 1,
    "last_acked_event_seq": 4711,
    "runtimes": [
      {
        "runtime_id": "...",
        "profile_id": "primary",
        "name": "Primary Codex",
        "generation": 7,
        "pid": 24192,
        "state": "running"
      }
    ]
  }
}
```

Gateway -> Worker:

```json
{
  "version": 1,
  "type": "hello_ack",
  "message_id": "...",
  "sent_at": "...",
  "payload": {
    "connection_id": "...",
    "heartbeat_interval_seconds": 10,
    "resume_from_event_seq": 4698
  }
}
```

Worker MUST then replay required unacknowledged durable events beginning after the requested sequence.

## 12.4 Heartbeat

Worker -> Gateway:

```json
{
  "type": "heartbeat",
  "payload": {
    "worker_id": "...",
    "uptime_seconds": 9123,
    "runtimes": [
      {
        "runtime_id": "...",
        "generation": 7,
        "state": "running",
        "pid": 24192
      }
    ]
  }
}
```

Initial values:

```text
heartbeat interval: 10 seconds
mark unreachable: 30 seconds without heartbeat
```

These MUST be configuration values.

## 12.5 Gateway command message

Gateway -> Worker:

```json
{
  "version": 1,
  "type": "command",
  "message_id": "...",
  "sent_at": "...",
  "payload": {
    "command_id": "...",
    "runtime_id": "...",
    "runtime_generation": 7,
    "session_id": "...",
    "codex_thread_id": "thr_123",
    "operation": "start_turn",
    "expected_turn_id": null,
    "arguments": {
      "text": "Fix the failing tests"
    },
    "created_at": "...",
    "expires_at": "..."
  }
}
```

## 12.6 Command acknowledgement

Worker MUST durably record receipt before acknowledging.

Worker -> Gateway:

```json
{
  "type": "command_ack",
  "payload": {
    "command_id": "...",
    "status": "accepted"
  }
}
```

Possible statuses:

```text
accepted
duplicate
rejected_stale_runtime
rejected_unknown_session
rejected_expired
rejected_invalid_state
```

A later completion is a separate event.

## 12.7 Worker event

Worker -> Gateway:

```json
{
  "type": "worker_event",
  "payload": {
    "event_seq": 4712,
    "event_id": "...",
    "worker_id": "...",
    "runtime_id": "...",
    "runtime_generation": 7,
    "session_id": "...",
    "kind": "turn_completed",
    "occurred_at": "...",
    "data": {}
  }
}
```

Gateway -> Worker:

```json
{
  "type": "event_ack",
  "payload": {
    "event_seq": 4712
  }
}
```

The Worker MAY compact acknowledged durable outbox entries.

---

# 13. Event Durability Classes

Not all streaming events require local persistence.

Define:

## Class A — MUST persist before sending

```text
runtime_started
runtime_stopped
runtime_failed
session_discovered
session_state_changed
turn_started
turn_completed
turn_failed
turn_interrupted
approval_requested
approval_resolved
user_input_requested
command_result_unknown
final_agent_message
```

## Class B — MAY be transient

```text
agent_message_delta
command_output_delta
tool_progress
token_usage_partial
```

If disconnected, Class B events MAY be dropped or coalesced.

The Worker MUST preserve Class A events until acknowledged.

---

# 14. Gateway Registry Database

Use SQLite.

Use UUID primary keys.

Store timestamps as fixed-width UTC RFC3339 text with nine fractional digits.

Store validated JSON text only for protocol payloads/extensions, not as a replacement for core relational columns. The executable schema is `migrations/001_registry.sql`; the examples below describe the relational model.

## 14.1 workers

```sql
CREATE TABLE workers (
    worker_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    hostname TEXT,
    os TEXT NOT NULL,
    arch TEXT NOT NULL,
    worker_version TEXT,
    auth_token_hash BLOB NOT NULL,
    enabled INTEGER NOT NULL DEFAULT TRUE,
    connectivity TEXT NOT NULL DEFAULT 'offline',
    last_seen_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
);
```

Connectivity enum values:

```text
online
unreachable
offline
disabled
```

## 14.2 runtimes

```sql
CREATE TABLE runtimes (
    runtime_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    profile_id TEXT NOT NULL,
    name TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    pid INTEGER,
    state TEXT NOT NULL,
    codex_version TEXT,
    default_cwd TEXT,
    started_at TEXT,
    stopped_at TEXT,
    last_seen_at TEXT,
    metadata TEXT NOT NULL DEFAULT '{}',
    UNIQUE(worker_id, profile_id)
);
```

Runtime states:

```text
stopped
starting
running
degraded
failed
```

## 14.3 sessions

```sql
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    codex_thread_id TEXT NOT NULL,
    codex_session_id TEXT,
    name TEXT,
    preview TEXT,
    cwd TEXT,
    git_branch TEXT,
    git_root TEXT,
    state TEXT NOT NULL,
    active_turn_id TEXT,
    loaded INTEGER NOT NULL DEFAULT FALSE,
    archived INTEGER NOT NULL DEFAULT FALSE,
    discovered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    last_activity_at TEXT,
    last_reconciled_at TEXT,
    metadata TEXT NOT NULL DEFAULT '{}',
    UNIQUE(runtime_id, codex_thread_id)
);
```

Session states:

```text
unknown
idle
running
waiting_approval
waiting_input
failed
not_loaded
```

Connectivity is NOT a Session state.

## 14.4 telegram_bindings

```sql
CREATE TABLE telegram_bindings (
    bot_id TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    selected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    PRIMARY KEY (bot_id, user_id, chat_id, message_thread_id)
);
```

Because SQLite treats NULL specially in uniqueness, implement the nullable topic key using either:

- `message_thread_id BIGINT NOT NULL DEFAULT 0`, or
- an expression-based unique index with `COALESCE`.

Preferred v1:

```text
message_thread_id = 0 means no topic
```

## 14.5 telegram_updates

```sql
CREATE TABLE telegram_updates (
    bot_id TEXT NOT NULL,
    update_id INTEGER NOT NULL,
    user_id INTEGER,
    chat_id INTEGER,
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    raw TEXT NOT NULL,
    PRIMARY KEY(bot_id, update_id)
);
```

## 14.6 commands

```sql
CREATE TABLE commands (
    command_id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL,
    session_id TEXT REFERENCES sessions(session_id),
    operation TEXT NOT NULL,
    expected_turn_id TEXT,
    payload TEXT NOT NULL,
    status TEXT NOT NULL,
    telegram_bot_id TEXT,
    telegram_update_id INTEGER,
    telegram_user_id INTEGER,
    telegram_chat_id INTEGER,
    telegram_message_thread_id INTEGER,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    dispatched_at TEXT,
    acknowledged_at TEXT,
    completed_at TEXT,
    expires_at TEXT,
    error_code TEXT,
    error_message TEXT
);
```

Command states:

```text
pending
dispatched
acknowledged
completed
failed
expired
outcome_unknown
```

## 14.7 events

Store Class A events long enough for auditing and recovery.

```sql
CREATE TABLE events (
    event_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT,
    runtime_generation INTEGER,
    session_id TEXT,
    event_seq INTEGER NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    UNIQUE(worker_id, event_seq)
);
```

## 14.8 approvals

```sql
CREATE TABLE approvals (
    approval_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    codex_request_id TEXT NOT NULL,
    codex_thread_id TEXT NOT NULL,
    codex_turn_id TEXT,
    codex_item_id TEXT,
    approval_type TEXT NOT NULL,
    request_payload TEXT NOT NULL,
    state TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution TEXT,
    UNIQUE(runtime_id, runtime_generation, codex_request_id)
);
```

Approval states:

```text
pending
approved
declined
cancelled
expired
cleared
```

## 14.9 telegram_callbacks

```sql
CREATE TABLE telegram_callbacks (
    token TEXT PRIMARY KEY,
    action TEXT NOT NULL,
    telegram_user_id INTEGER NOT NULL,
    session_id TEXT,
    command_id TEXT,
    approval_id TEXT,
    payload TEXT NOT NULL DEFAULT '{}',
    expires_at TEXT NOT NULL,
    used_at TEXT
);
```

Single-use callbacks SHOULD set `used_at` atomically.

## 14.10 bot_message_routes

```sql
CREATE TABLE bot_message_routes (
    bot_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT,
    approval_id TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    PRIMARY KEY(bot_id, chat_id, message_id)
);
```

This lets replies route back to the original Session even if the user has selected another one.

---

# 15. Registry Transactions

The following operations MUST be transactional:

## 15.1 Accept Telegram message

One transaction:

```text
dedupe update
resolve binding
validate target
create command
persist message-route metadata if needed
commit
```

## 15.2 Approval button

One transaction:

```text
lock callback row
verify actor
verify callback not used/expired
lock approval row
verify pending
set callback used
create immutable approval-response command
commit
```

## 15.3 Worker event ingestion

One transaction:

```text
dedupe by worker_id + event_seq
insert event if durable
apply Registry state transition
commit
send event_ack
```

Never ACK a durable event before its DB transaction commits.

---

# 16. Worker Local Durable Store

Use bbolt or another pure-Go transactional embedded KV store.

Suggested buckets:

```text
meta
commands
outbox
runtimes
sessions
```

## 16.1 meta

```text
worker_id
next_event_seq
last_gateway_acked_event_seq
schema_version
```

## 16.2 commands

Key:

```text
command_id
```

Value includes:

```text
received_at
operation
runtime_id
runtime_generation
session_id
state
result summary
```

Purpose:

- deduplicate commands after reconnect,
- prevent replay from executing the same action twice where possible.

## 16.3 outbox

Key:

```text
event_seq big-endian
```

Value:

```text
complete worker_event envelope
```

Deletion allowed only after Gateway ACK.

---

# 17. Worker Startup Sequence

Required sequence:

```text
1. Load configuration.
2. Open local durable store.
3. Load/create stable worker_id.
4. Validate configured Codex binaries.
5. Start configured autostart runtimes.
6. For each runtime:
   a. spawn Codex App Server
   b. establish stdio JSON-RPC
   c. perform Codex initialize handshake
   d. reconcile loaded/persisted threads
7. Start outbound Gateway reconnect loop.
8. Authenticate WSS.
9. Send hello with local snapshot.
10. Replay durable unacknowledged events.
11. Receive commands.
12. Continue periodic reconciliation and heartbeats.
```

A Gateway outage MUST NOT terminate Codex App Server processes.

---

# 18. Codex App Server Adapter

The Worker needs a dedicated Codex adapter.

The adapter MUST support at least:

```text
initialize / initialized
thread/start
thread/resume
thread/read
thread/list
thread/loaded/list
turn/start
turn/steer
turn/interrupt
thread/status/changed
turn/*
item/*
serverRequest/resolved
```

It MUST handle server-initiated approval/input requests.

## 18.1 JSON-RPC transport

For stdio:

- one JSON object per line,
- dedicated reader goroutine,
- dedicated writer goroutine,
- never allow multiple goroutines to write directly to stdin,
- map request IDs to response channels,
- route notifications to event handlers,
- route server requests to approval coordinator.

Suggested shape:

```go
type Client struct {
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    pending map[int64]chan RPCResponse
    writeCh chan RPCMessage
    events  chan Event
}
```

Access to `pending` MUST be concurrency-safe.

Every outgoing request MUST support context cancellation and timeout.

## 18.2 Initialization

Immediately after local transport establishment:

```text
initialize
initialized
```

No other Codex calls before successful initialization.

Store negotiated/version information if Codex provides it.

## 18.3 Thread discovery

On Runtime startup:

```text
thread/loaded/list
thread/list (paginate until complete or configured cap)
```

Reconcile:

```text
new Codex thread -> create local/Gateway Session
known thread -> update metadata/state
Registry session absent locally -> mark not_loaded/offline as appropriate
```

Do not treat persisted `notLoaded` threads as running.

## 18.4 Start new Session

`/new` ultimately invokes:

```text
thread/start
```

Persist returned:

```text
thread.id
thread.sessionId
```

Never invent Codex thread IDs.

## 18.5 Resume Session

Before sending a turn to a known persisted thread that is not currently loaded:

```text
thread/resume
```

then:

```text
turn/start
```

If resume fails because the thread no longer exists, mark Session failed/missing and return a deterministic error to Gateway.

## 18.6 Turn start

Translate:

```text
operation = start_turn
```

to:

```text
turn/start
```

Input:

```json
[
  {
    "type": "text",
    "text": "..."
  }
]
```

Record returned turn ID immediately and emit durable:

```text
turn_started
```

## 18.7 Steering

Translate:

```text
operation = steer
```

to:

```text
turn/steer
```

Include:

```text
expectedTurnId
```

Never steer based only on "whatever turn is active now".

## 18.8 Interrupt

Translate:

```text
operation = interrupt
```

to:

```text
turn/interrupt
```

Validate the expected active turn first.

## 18.9 Runtime protocol compatibility

Codex App Server evolves.

Therefore:

- isolate all Codex wire structs in `codexadapter`,
- log Codex version on Runtime start,
- maintain adapter capability flags,
- unknown notifications MUST NOT crash Worker,
- unknown item types SHOULD be logged at debug level,
- required method absence MUST produce a clear Runtime degraded state.

---

# 19. Per-Session Execution Coordinator

Worker MUST have one coordinator per Session.

Responsibilities:

```text
current active turn
pending ordinary command queue
pending approvals
session state
Codex thread loading/resume
serialized state mutation
```

Suggested design:

```go
type SessionCoordinator struct {
    commands chan Command
    events   chan CodexEvent
}
```

Each coordinator is single-writer for its own in-memory state.

Different Sessions MAY run concurrently.

Within one Session:

- ordinary `start_turn` commands queue,
- `steer` addresses the active turn,
- `interrupt` addresses the active turn,
- approval response addresses a specific pending Codex request.

## 19.1 Ordinary message queue

Default max:

```text
20 queued turns/session
```

Configurable.

Reject after limit with:

```text
session_queue_full
```

Default command expiry:

```text
1 hour
```

A command older than expiry MUST NOT suddenly execute after reconnection.

## 19.2 Queue behavior

When active turn completes:

```text
if queued start_turn exists
  -> dequeue oldest valid command
  -> start next turn
```

FIFO.

Steer commands do not join the ordinary queue.

---

# 20. Session State Machine

Canonical states:

```text
unknown
not_loaded
idle
running
waiting_approval
waiting_input
failed
```

Approximate transitions:

```text
not_loaded --resume--> idle
idle --turn/start--> running
running --approval request--> waiting_approval
waiting_approval --resolved--> running
running --user input request--> waiting_input
waiting_input --resolved--> running
running --turn complete--> idle
running --turn failure--> failed
failed --successful resume/reconcile--> idle/not_loaded
```

Connectivity is independent:

```text
Worker online/unreachable/offline
```

UI must combine them rather than corrupting Session state.

Example:

```text
Session: running
Worker: unreachable
Display: "state uncertain — worker last seen 35s ago"
```

Do not change `running` to `failed` solely because the network connection disappeared.

---

# 21. Approvals

Codex approval requests are server-initiated JSON-RPC requests.

The Worker MUST turn each into a durable `approval_requested` event.

Required metadata:

```text
runtime_id
runtime_generation
session_id
Codex JSON-RPC request id
threadId
turnId
itemId if present
approval type
reason
command/cwd/change summary if present
available decisions if present
```

Gateway persists Approval before rendering Telegram controls.

## 21.1 Telegram approval example

```text
⚠️ Approval required · auth-fix

Action: command execution
Directory: /Users/USERNAME/dev/api
Command: npm test

[Approve once] [Approve session] [Decline]
```

Only show decisions that are valid for that request.

## 21.2 Approval safety

When button is clicked:

Gateway MUST validate:

```text
Telegram user is authorized
callback token belongs to this actor
Approval is still pending
runtime_generation matches
Session matches
callback not expired
```

Worker MUST validate again:

```text
runtime generation
session
Codex request id still pending
turn id if supplied
```

If any mismatch:

```text
reject stale approval
```

Never apply an approval to the "current" request by position/order.

## 21.3 Approval lifecycle

Codex may clear a request before Telegram responds.

When Worker sees:

```text
serverRequest/resolved
```

it MUST emit:

```text
approval_resolved/cleared
```

Gateway disables or invalidates the Telegram callback.

A later click must respond:

```text
This approval is no longer pending.
```

---

# 22. User Input Requests

Treat Codex user-input requests similarly to approvals.

The Worker emits a durable event.

Gateway renders the prompt to Telegram.

For simple free-text prompts, Gateway MAY bind the user's reply to a pending input request.

Do not confuse this with changing the selected Session.

Recommended rule:

If a Telegram message is a reply to a pending input-request bot message, route to that exact request before using current Session binding.

---

# 23. Output Rendering

Do not forward every Codex stream delta as a new Telegram message.

## 23.1 Progress

For active work, Gateway SHOULD maintain one editable progress message.

Update no more frequently than a configurable interval, initially:

```text
2 seconds
```

Possible content:

```text
⏳ auth-fix
Running tests…
```

## 23.2 Final output

Send a persistent final message on turn completion.

Prefix session identity when useful:

```text
✅ auth-fix

Tests are passing. I changed...
```

Store:

```text
(bot_id, chat_id, bot_message_id) -> session_id, turn_id
```

so replies route correctly.

## 23.3 Large content

If output exceeds Telegram message limits:

- split into bounded chunks, or
- create a text/document attachment later.

v1 SHOULD prefer concise final messages and truncate verbose command logs with:

```text
Use /status or a future /logs command for details.
```

## 23.4 Secret redaction

Worker SHOULD redact known secrets before sending command/tool output upstream.

At minimum support configurable regex rules.

Gateway SHOULD also avoid rendering:

```text
worker auth tokens
Telegram bot token
Codex/OpenAI credentials
environment variables known to be secrets
```

---

# 24. Reconnect and Failure Semantics

## 24.1 Worker reconnect

Use exponential backoff with jitter.

Example:

```text
1s
2s
4s
8s
15s
30s max
```

Reset after stable connection.

## 24.2 Gateway restart

Expected behavior:

```text
Gateway process stops
Workers remain running
Codex runtimes remain running
Workers buffer Class A events
Gateway restarts
Workers reconnect
Registry loads persisted state
Workers hello/reconcile
Workers replay unacked Class A events
Gateway resumes Telegram operation
```

## 24.3 Worker process restart

Worker reloads:

```text
worker_id
local command ledger
outbox
runtime profiles
```

Existing Codex child processes from the previous Worker process may be orphaned.

v1 SHOULD choose one explicit policy:

### Recommended v1 policy

Worker owns only children it launched during its current lifetime.

On Worker restart:

- do not attempt unsafe PID adoption,
- launch new configured Codex Runtime processes,
- increment runtime generation,
- discover and resume persisted Codex threads.

This avoids incorrectly attaching to unrelated PIDs.

Future versions may add a safe local control socket for Runtime adoption.

## 24.4 Codex Runtime crash

Worker:

```text
detect child exit
emit runtime_failed
mark affected session state uncertain/not_loaded as appropriate
restart according to runtime restart policy
increment generation
reconcile persisted threads
```

Default restart policy:

```text
on-failure
max 5 attempts in 10 minutes
```

After that:

```text
runtime state = failed
manual restart required
```

## 24.5 Ambiguous command outcome

Exactly-once execution is not guaranteed.

Example:

```text
Worker sends turn/start to Codex
Codex accepts
Worker crashes before recording success
```

On recovery, Worker MUST NOT blindly replay `turn/start`.

Instead:

1. inspect/reconcile Codex thread state/history,
2. determine whether the command can be correlated,
3. if uncertain, mark:

```text
outcome_unknown
```

and notify Gateway.

Do not risk duplicate filesystem changes merely to make a command look successful.

---

# 25. Command Idempotency

Worker command handling rule:

```text
if command_id exists in local ledger:
    return previous acknowledgement/result
    do not execute again
```

Operations that are naturally idempotent MAY be retried.

Operations that start new work are not assumed idempotent:

```text
thread/start
turn/start
```

For these, ambiguous outcome handling applies.

Gateway dispatcher MAY resend a command whose ACK was lost, because Worker deduplicates by `command_id`.

---

# 26. Runtime Generation Safety

Every Gateway command MUST specify:

```text
runtime_id
runtime_generation
```

Worker compares to current generation.

If mismatch:

```text
rejected_stale_runtime
```

This prevents a delayed Telegram approval or command from being applied after Codex restarted.

A Session may subsequently be rebound/reconciled to the new Runtime generation, but the old Command remains immutable and failed/stale.

---

# 27. Security Requirements

## 27.1 Gateway

MUST:

- run as non-root,
- keep Telegram token outside source control,
- keep the SQLite database, its sidecars, and backups outside source control,
- validate Telegram webhook secret,
- authorize Telegram numeric user IDs,
- authenticate each Worker independently,
- rate-limit authentication failures,
- avoid logging bearer tokens,
- set HTTP server timeouts,
- cap request/body sizes,
- reject unsupported Worker protocol versions.

## 27.2 Worker

MUST:

- run as a normal OS user,
- restrict token/config permissions,
- never expose a listening public port in v1,
- connect outbound only,
- not provide general remote shell execution,
- only operate configured Codex Runtime profiles,
- only permit `/new` in configured allowed workspace roots,
- not send local secrets to Telegram intentionally.

## 27.3 Workspace allowlist

Worker config:

```yaml
security:
  allowed_workspace_roots:
    - /Users/USERNAME/dev
    - /Users/USERNAME/src
```

Linux example:

```yaml
security:
  allowed_workspace_roots:
    - /home/USERNAME/dev
```

Canonicalize paths and prevent `..` traversal.

Do not trust a Gateway-supplied path without local validation.

## 27.4 Reverse proxy headers

Gateway MUST only trust forwarding headers from the configured reverse proxy.

If proxy and Gateway are on the same host, bind Gateway to loopback.

## 27.5 Database secrets

Worker auth tokens MUST be stored as hashes, not plaintext.

Suggested:

```text
SHA-256 of a 32-byte random token
```

Because the token has high entropy, a fast cryptographic hash is acceptable.

Compare in constant time.

---

# 28. Configuration

## 28.1 Gateway

Example:

```yaml
server:
  listen: 127.0.0.1:8080
  public_base_url: https://codex.example.com

database:
  path: /var/lib/codex-gateway/gateway.db

telegram:
  bot_token_env: CODEX_GATEWAY_TELEGRAM_TOKEN
  webhook_secret_env: CODEX_GATEWAY_TELEGRAM_WEBHOOK_SECRET
  allowed_user_ids:
    - 123456789
  allowed_chat_ids: []

workers:
  heartbeat_interval: 10s
  unreachable_after: 30s

commands:
  default_expiry: 1h

logging:
  level: info
```

Secrets SHOULD come from environment variables or system secret files, not YAML.

## 28.2 Worker

Example:

```yaml
worker:
  name: example-macbook
  state_file: ~/.local/share/codex-worker/state.db

gateway:
  url: wss://codex.example.com/api/v1/workers/connect
  token_file: ~/.config/codex-worker/token

security:
  allowed_workspace_roots:
    - /Users/USERNAME/dev

runtimes:
  - id: primary
    name: Primary Codex
    codex_binary: /opt/homebrew/bin/codex
    working_directory: /Users/USERNAME/dev
    autostart: true
    restart_policy: on-failure

logging:
  level: info
```

---

# 29. Worker Enrollment

Provide a Gateway CLI command:

```text
codex-gateway worker create --name example-macbook
```

Output token once:

```text
Worker ID: 85303ed2-...
Token: cwk_<random-secret>
```

The plaintext token MUST NOT be retrievable later.

Also provide:

```text
codex-gateway worker list
codex-gateway worker revoke <worker-id>
codex-gateway worker rotate-token <worker-id>
```

These may be CLI commands against the database in v1.

No public administration API is required.

---

# 30. Health and Readiness

## Gateway `/healthz`

Returns 200 if process is alive.

No DB requirement.

## Gateway `/readyz`

Returns 200 only if:

```text
SQLite reachable
migrations valid
Telegram configuration valid enough to operate
```

Worker availability is NOT required for Gateway readiness.

## Worker local diagnostics

Worker SHOULD support CLI:

```text
codex-worker status
codex-worker doctor
```

`doctor` checks:

```text
config parse
Gateway URL
token file permission
Codex binary exists
Codex version command works
allowed workspace roots exist
local state DB opens
```

No HTTP listener is required.

---

# 31. Logging

Use structured JSON logging with `log/slog`.

Every relevant log record SHOULD include where available:

```text
worker_id
runtime_id
runtime_generation
session_id
command_id
turn_id
approval_id
telegram_update_id
```

Never include:

```text
bearer token
Telegram bot token
full secret environment variables
```

Logging levels:

```text
DEBUG protocol details without secrets
INFO  lifecycle and successful state changes
WARN  reconnects, stale commands, degraded behavior
ERROR failed persistent operations, Runtime crashes
```

---

# 32. Metrics

Optional but recommended v1 metrics:

```text
gateway_workers_online
gateway_commands_total{operation,status}
gateway_command_latency_seconds
gateway_worker_reconnects_total
gateway_telegram_updates_total
gateway_telegram_send_errors_total
gateway_pending_approvals

worker_runtime_restarts_total
worker_commands_total{operation,status}
worker_outbox_depth
worker_sessions{state}
worker_gateway_connected
```

Prometheus exposition may be added at `/metrics`.

---

# 33. Reverse Proxy Example

Caddy example:

```caddy
codex.example.com {
    encode zstd gzip

    reverse_proxy 127.0.0.1:8080
}
```

Gateway itself handles:

```text
/api/v1/telegram/webhook
/api/v1/workers/connect
/healthz
/readyz
```

Caddy automatically supports WebSocket upgrade forwarding.

For nginx, ensure Upgrade and Connection headers are correctly proxied.

---

# 34. Linux Service Example

Gateway systemd conceptual unit:

```ini
[Unit]
Description=Codex Telegram Gateway
After=network-online.target
Wants=network-online.target

[Service]
User=codex
Group=codex
ExecStart=/usr/local/bin/codex-gateway --config /etc/codex-gateway/config.yaml
Restart=on-failure
EnvironmentFile=/etc/codex-gateway/secrets.env
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Worker Linux:

```ini
[Unit]
Description=Codex Worker
After=network-online.target
Wants=network-online.target

[Service]
User=%i
ExecStart=/usr/local/bin/codex-worker run --config %h/.config/codex-worker/config.yaml
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
```

Packaging may need adjustment for per-user service deployment.

---

# 35. macOS Service

Provide a LaunchAgent plist, not a LaunchDaemon by default.

Reason:

Codex should normally execute as the logged-in developer user and inherit access to that user's repositories/configuration.

Conceptual:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.codex-worker</string>

  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/codex-worker</string>
    <string>run</string>
    <string>--config</string>
    <string>/Users/USER/.config/codex-worker/config.yaml</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

Installer MUST replace paths safely.

---

# 36. Release Artifacts

CI MUST build:

```text
codex-gateway-linux-amd64
codex-gateway-linux-arm64

codex-worker-linux-amd64
codex-worker-linux-arm64
codex-worker-darwin-amd64
codex-worker-darwin-arm64
```

Gateway Darwin builds are optional.

Release archives SHOULD include:

```text
binary
LICENSE
README
SHA256SUMS
sample config
```

Recommended Makefile targets:

```text
make test
make lint
make build
make release
```

Cross-build example:

```bash
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build ./cmd/codex-worker
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build ./cmd/codex-worker
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/codex-worker
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/codex-worker
```

---

# 37. Telegram UX Requirements

Keep the bot useful from a phone.

Avoid exposing UUIDs in normal interactions.

Generate human-readable short labels.

Example:

```text
MacBook / Primary / auth-fix
Linux / Backend / payment-tests
```

Use shortened internal IDs only when troubleshooting.

Buttons SHOULD be preferred for:

```text
runtime selection
session connection
approval decisions
```

Text commands remain available for power users.

---

# 38. Optional Telegram Topic Mode

Not required for MVP, but data model MUST not prevent it.

In topic mode:

```text
one Telegram message_thread_id -> one Session
```

Then ordinary messages in that topic route directly to the mapped Session.

`/connect` remains useful in normal private-chat mode.

Do not automatically create Telegram topics in v1.

---

# 39. Concurrency Requirements

## Gateway

Must safely support:

```text
multiple Workers
multiple Runtimes per Worker
multiple Sessions per Runtime
multiple simultaneous Codex turns across different Sessions
concurrent Telegram updates
concurrent approvals
```

Use SQLite transactions for shared persistent state.

Do not rely on process-local mutexes for correctness that must survive restart.

## Worker

Use:

- one serialized WebSocket writer,
- one WebSocket reader,
- one local durable-store access abstraction,
- one Codex adapter reader per Runtime,
- one Codex adapter writer per Runtime,
- one Session coordinator per active/known Session.

No concurrent raw writes to:

```text
Gateway WebSocket
Codex stdin
```

---

# 40. Timeouts

Initial defaults:

```text
Gateway HTTP read-header timeout: 5s
Telegram API request timeout: 15s
Worker hello timeout: 10s
Worker heartbeat: 10s
Worker unreachable threshold: 30s
Codex JSON-RPC simple request timeout: 30s
Codex thread/list timeout: 30s
Command expiry: 1h
Callback token expiry: 15m
```

Long Codex turns are event-driven and MUST NOT be governed by the simple RPC request timeout after `turn/start` has been accepted.

---

# 41. Error Model

Custom protocol errors:

```json
{
  "code": "stale_runtime",
  "message": "Runtime generation no longer matches.",
  "retryable": false
}
```

Canonical error codes:

```text
unauthorized
unsupported_protocol
unknown_worker
worker_disabled
unknown_runtime
stale_runtime
unknown_session
session_not_available
session_busy
session_queue_full
stale_turn
approval_not_pending
command_expired
invalid_workspace
codex_unavailable
codex_protocol_error
codex_method_unsupported
outcome_unknown
internal_error
```

Do not expose stack traces to Telegram.

---

# 42. Telegram Notification Policy

Send Telegram notifications for:

```text
final turn result
approval required
user input required
turn failed
runtime failed when user has active binding there
command outcome unknown
```

Do not spam Telegram for:

```text
every heartbeat
every stream delta
routine reconnect attempts
thread discovery details
```

Optional user preference later:

```text
notify when background turn completes
```

---

# 43. Testing Strategy

## 43.1 Unit tests

Gateway:

```text
Telegram auth/allowlist
command parsing
routing priority
binding resolution
callback validation
runtime-generation validation
Telegram rendering
```

Registry:

```text
transactions
deduplication
state transitions
approval races
```

Worker:

```text
command ledger dedupe
outbox replay
runtime generation handling
queue behavior
path allowlist
```

Codex adapter:

```text
JSON-RPC request/response correlation
notification parsing
server request parsing
unknown event handling
process exit handling
```

## 43.2 Fake Codex App Server

Implement a test binary or in-memory subprocess fixture that speaks the expected JSONL protocol.

It MUST simulate:

```text
initialize
thread/list
thread/loaded/list
thread/start
thread/resume
turn/start
turn/steer
turn/interrupt
streaming item events
approval requests
turn completion
process crash
malformed JSON
delayed responses
```

Integration tests MUST NOT require real Codex credentials.

## 43.3 Fake Telegram server

Abstract Telegram API behind an interface.

Tests inject updates and capture outgoing messages/callbacks.

---

# 44. Mandatory Acceptance Tests

Implementation is not MVP-complete until all tests below pass.

## AT-01 — Worker registration

Given an enrolled Worker,
when it connects using valid credentials,
then Registry shows it online and records OS/arch/version.

## AT-02 — Invalid Worker credential

Invalid bearer token cannot open Worker session.

No metadata is registered.

## AT-03 — Discover sessions

Worker starts fake Codex with three threads.

Gateway Registry receives all three and `/sessions` displays them.

## AT-04 — Select session without affecting execution

Session A is running.

User `/connect`s Session B.

Session A continues running uninterrupted.

## AT-05 — Deterministic queued routing

1. User selects A.
2. User sends "fix tests".
3. Command is persisted.
4. User immediately selects B.
5. Dispatcher sends delayed command.

Expected:

```text
"fix tests" is sent to A, never B.
```

## AT-06 — Reply routing

User is currently connected to B.

User replies to an earlier bot message belonging to A.

Expected:

```text
reply routes to A according to reply mapping.
```

## AT-07 — Duplicate Telegram update

Same `update_id` delivered twice.

Exactly one Gateway command is created.

## AT-08 — Duplicate Gateway command delivery

Worker receives same `command_id` twice.

Operation is executed no more than once.

Second delivery returns duplicate/prior result.

## AT-09 — Gateway restart

During a running turn:

1. Gateway stops.
2. Worker and Codex remain alive.
3. Turn completes.
4. Worker persists final event locally.
5. Gateway restarts.
6. Worker reconnects.
7. Final event is replayed.
8. Telegram receives final result.

## AT-10 — Worker temporary network loss

Worker disconnects from Gateway but Codex remains active.

Registry reports Worker unreachable, not Session failed.

After reconnect state reconciles.

## AT-11 — Stale runtime command

Runtime generation changes from 7 to 8.

A delayed command addressed to generation 7 arrives.

Worker rejects with:

```text
rejected_stale_runtime
```

## AT-12 — Correct approval

Codex generates approval request X.

Telegram button approves X.

Exactly X is resolved.

## AT-13 — Stale approval

Approval X is cleared by Codex before Telegram button is clicked.

Button click does not approve any later request.

User receives "no longer pending".

## AT-14 — Steering correct turn

Session has active turn T1.

Gateway sends steer expected T1.

Worker sends Codex `turn/steer` expected T1.

If active turn has become T2, steer is rejected.

## AT-15 — Interrupt correct turn

Same stale-turn semantics as steering.

## AT-16 — Queue serialization

Two ordinary messages arrive while one turn is running.

They execute FIFO after the current turn, never concurrently in the same Session.

## AT-17 — Cross-session concurrency

A and B are separate Sessions.

Both can have active turns simultaneously.

## AT-18 — Runtime crash

Codex process exits unexpectedly.

Worker emits runtime_failed, increments generation on restart, and reconciles persisted threads.

## AT-19 — Gateway database restart

SQLite temporarily unavailable.

Gateway readiness fails.

Already-running Workers/Codex remain unaffected.

Gateway recovers when DB returns.

## AT-20 — Worker cross-platform build

CI successfully produces:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
```

with `CGO_ENABLED=0`.

---

# 45. Implementation Phases

## Phase 1 — Foundation

Implement:

```text
Go monorepo
configs
logging
SQLite migrations
Worker enrollment
custom protocol types
Gateway WSS hub
Worker reconnect loop
heartbeat
```

Definition of done:

```text
Worker appears online/offline in Registry.
```

## Phase 2 — Codex adapter

Implement:

```text
Codex subprocess supervisor
stdio JSON-RPC
initialize handshake
thread/list
thread/loaded/list
thread/read/resume/start
event parser
```

Definition of done:

```text
Gateway Registry lists discovered Codex Sessions.
```

## Phase 3 — Telegram control

Implement:

```text
Telegram webhook
allowlist
dedupe
/instances
/sessions
/connect
/status
ordinary message routing
```

Definition of done:

```text
Telegram message starts a turn in selected Session.
```

## Phase 4 — Durable command/event pipeline

Implement:

```text
immutable commands
Worker ledger
Worker outbox
ACK protocol
reconnect replay
runtime generations
```

Definition of done:

```text
Gateway restart does not lose final result.
```

## Phase 5 — Approvals and steering

Implement:

```text
approval requests
callback token registry
approve/decline
user input requests
/steer
/interrupt
stale-turn safety
```

Definition of done:

```text
Approvals cannot be cross-wired between Sessions or Runtime generations.
```

## Phase 6 — Packaging and operations

Implement:

```text
systemd
launchd
release cross-builds
doctor command
metrics
operations documentation
```

---

# 46. MVP Definition

The MVP is complete when a user can:

1. Install `codex-worker` on a Mac or Linux machine.
2. Enroll it with the Linux Gateway.
3. Have the Worker launch Codex App Server locally.
4. Open Telegram.
5. Run `/instances`.
6. See connected Workers/Runtimes.
7. Run `/sessions`.
8. Select a Codex thread.
9. Send a normal Telegram message.
10. Have it execute in exactly that thread.
11. Switch to a different thread without stopping the first.
12. Receive final results from both threads.
13. Approve/decline an exact Codex approval request.
14. Survive a Gateway restart without terminating Codex work.

---

# 47. Explicit Design Decisions

The implementer MUST follow these unless this specification is revised.

## Decision 1

**Go for Gateway, Registry code, and Worker.**

## Decision 2

**SQLite for central Registry.**

Use a local SQLite file with WAL, FULL synchronization, foreign keys, and immediate write transactions. Do not use a network filesystem.

## Decision 3

**bbolt/pure-Go embedded store on Worker.**

Do not require a database server or SQLite on Worker.

## Decision 4

**Telegram webhook, not long polling**, because a public HTTPS Gateway already exists.

## Decision 5

**Worker makes outbound WSS connection to Gateway.**

Gateway never needs inbound access to Worker hosts.

## Decision 6

**Worker uses local Codex App Server stdio.**

Do not expose Codex's experimental WebSocket interface.

## Decision 7

**Gateway and Registry are one deployable service in v1.**

Keep internal packages separate.

## Decision 8

**Session selection is routing state only.**

It never starts/stops work by itself.

## Decision 9

**Target is frozen at command acceptance time.**

Never dispatch according to whichever Session is selected later.

## Decision 10

**One execution coordinator per Session.**

Concurrency across Sessions; serialization within one Session.

## Decision 11

**Runtime generation is part of every control target.**

Old commands cannot act on restarted Runtime generations.

## Decision 12

**No exactly-once claim.**

Use deduplication plus explicit `outcome_unknown` handling.

---

# 48. Open Questions Deferred from MVP

These should be recorded as future ADR candidates, not solved ad hoc during implementation:

1. Should one Worker Runtime host all Codex threads or should profiles map one runtime per project?
2. Should Workers support attachment upload/download?
3. Should Telegram topics map 1:1 to Sessions?
4. Should there be a web admin UI?
5. Should Worker identity migrate from bearer token to mTLS?
6. Should Workers support safe adoption of existing Codex Runtime processes?
7. Should Git worktrees be created automatically for concurrent Sessions in one repository?
8. Should the Gateway support multiple Telegram users with per-user ACLs?
9. Should session history be mirrored centrally or left exclusively on Worker hosts?
10. Should large logs/artifacts use S3-compatible object storage?

Do not let these block the MVP.

---

# 49. Suggested Initial ADRs

Create:

```text
docs/adr/0001-use-go.md
docs/adr/0002-worker-outbound-wss.md
docs/adr/0003-codex-stdio-adapter.md
docs/adr/0004-sqlite-registry.md
docs/adr/0005-immutable-command-routing.md
docs/adr/0006-runtime-generation.md
```

Each ADR should state Context / Decision / Consequences.

---

# 50. Source/API Notes for Implementer

Before writing the Codex adapter, verify the installed/current Codex App Server API against the current official documentation.

Relevant current concepts include:

```text
thread/start
thread/resume
thread/list
thread/loaded/list
thread/read
thread/status/changed
turn/start
turn/steer
turn/interrupt
serverRequest/resolved
item/commandExecution/requestApproval
item/fileChange/requestApproval
item/permissions/requestApproval
tool/requestUserInput
```

Important current behavior:

- `turn/steer` supports an expected active turn ID and should be used to prevent stale steering.
- `thread/loaded/list` means threads currently loaded in memory; it is not the same as all persisted threads.
- `thread/list` lists stored thread history and reports runtime status.
- approval requests are server-initiated requests that require a client response.
- App Server remote WebSocket support is currently experimental; local stdio is the required v1 integration.

Official references:

- Codex App Server:
  https://developers.openai.com/codex/app-server/
  (currently redirects to the ChatGPT Learn Codex App Server documentation)

- Telegram Bot API:
  https://core.telegram.org/bots/api

- Go supported platforms / source installation:
  https://go.dev/doc/install/source

- Go on ARM:
  https://go.dev/wiki/GoArm

The implementation MUST pin and record the tested Codex CLI/App Server version in CI/integration documentation.

---

# 51. Codex Implementation Prompt

When handing this document to Codex, use the following high-level instruction:

```text
Implement this specification incrementally.

Rules:
1. Treat MUST/MUST NOT statements as hard requirements.
2. Start with Phase 1 and do not implement later phases by bypassing earlier abstractions.
3. Keep Codex App Server wire protocol isolated in internal/codexadapter.
4. Keep Worker/Gateway wire protocol isolated in internal/protocol.
5. Create SQLite migrations before writing Registry persistence logic.
6. Write tests alongside every phase.
7. Use a fake Codex App Server for integration tests so tests do not require external credentials.
8. Do not introduce Redis, Kafka, Kubernetes, a web UI, or direct remote Codex WebSockets.
9. Keep the project CGO-free.
10. Before declaring a phase complete, run gofmt, go vet, go test ./..., and the relevant acceptance tests.
11. Record any necessary deviation from the specification as an ADR instead of silently changing behavior.
12. Never weaken stale-runtime, stale-turn, approval, authentication, or deterministic-routing checks to make tests easier.
```

---

# 52. Final Architectural Invariants

These invariants define the system.

```text
A Telegram message always has one immutable execution target.

Changing selected Session never changes a command already accepted.

Changing selected Session never interrupts another Session.

A Worker outage does not imply a Codex failure.

A Gateway outage does not stop local Codex work.

A Runtime restart creates a new generation.

A stale Runtime generation cannot receive control commands.

A steer/interrupt targets an exact turn.

An approval targets an exact Codex request.

A duplicate Telegram update does not create duplicate work.

A duplicate Gateway delivery does not intentionally execute duplicate work.

Only Workers talk to local Codex App Server.

Codex App Server is never directly exposed to the public Internet.

The Gateway holds the Telegram bot token; Workers do not.

Workers initiate outbound authenticated connections.

Central routing state is durable in SQLite.

Critical Worker events survive temporary Gateway disconnection.

The system never claims exactly-once execution when the outcome is ambiguous.
```

If an implementation choice violates one of these invariants, the implementation choice is wrong unless the specification is formally revised.

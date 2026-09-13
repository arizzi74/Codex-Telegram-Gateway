# Codex App Server compatibility

`internal/codexadapter` was verified against Codex CLI `0.154.0` on
2026-09-13. The version-specific JSON Schema bundle was generated with:

```text
codex app-server generate-json-schema --out ./schemas
```

The protocol reference is [Codex App Server](https://developers.openai.com/codex/app-server).

The adapter supports direct `codex app-server --listen stdio://`. Managed
worker profiles use the private shared transport below to support the requested
local CLI attachment. Both expose one JSONL reader and writer to the adapter. Each connection performs `initialize`
followed by `initialized` before every other request.

The terminal CLI cannot attach to a child whose only listener is `stdio://`:
there is one private byte stream and no attachable endpoint. Codex `0.154.0`
supports local CLI attachment through a Unix control socket instead. A managed
daemon can expose that socket to `codex --remote unix://PATH`; a worker that
needs the same daemon can keep its adapter boundary by supervising
`codex app-server proxy --sock PATH` as its JSONL stdio peer. This is an
explicit alternate runtime mode, not a second writer on the direct stdio child.

`codexadapter.StartShared` implements the owned, local variant of that mode.
It creates or requires a private `0700` socket directory, refuses an existing
socket, starts `codex app-server --listen unix://PATH`, restricts the created
socket to `0600`, and starts `codex app-server proxy --sock PATH` as the
worker's private byte stream. Unix app-server transports use a WebSocket HTTP
Upgrade handshake, and `app-server proxy` forwards bytes only; it does not
convert JSONL to WebSocket frames. The adapter therefore performs that upgrade
through the proxy and bridges one JSONL object per line to one WebSocket text
frame. The Unix socket is local only; it is never exposed by the Gateway or
over a network transport. This explicit user-requested shared profile extends
the normal direct-stdio process topology without changing the worker-to-Codex
JSONL boundary.

`codex-local attach --socket PATH [THREAD]` runs the supported local TUI form
`codex --remote unix://PATH resume [THREAD]`. `codex-local start` owns a
temporary shared runtime for the current directory and resolves the newest session in that exact directory, resumes it, then attaches
with `codex --remote unix://PATH resume THREAD_ID`. If there is no history, it
opens a fresh terminal session; it does not try to resume a new ID without a persisted rollout. A conflicting active writer produces a clear error. Its app-server and proxy stay alive
only while that interactive CLI runs; the helper then reaps both processes and
removes its socket. It never adopts an arbitrary existing Codex PID or socket.

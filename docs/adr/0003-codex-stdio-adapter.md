# Codex adapter boundary

Context: Codex's evolving wire protocol must not become the gateway protocol.

Decision: internal/codexadapter owns process invocation and every Codex wire
type. Workers consume normalized domain events and operate through JSONL
reader/writer pumps. Direct runtimes use app-server stdio.

Consequences: protocol compatibility is pinned and tested independently. The
user-requested shared CLI topology is described in ADR 0008.

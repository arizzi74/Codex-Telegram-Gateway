# Sharing an app-server with the local Codex CLI

Context: the user requires local interactive CLI attachment to supervised threads.
A direct stdio-only child has no endpoint for another client. Installed Codex
0.154.0 supports WebSocket framing over a private Unix-domain socket.

Decision: an attachable runtime owns one app-server bound to a private Unix
socket and one `codex app-server proxy` byte proxy. Inside codexadapter, a framing
bridge performs HTTP Upgrade through the proxy's stdio and translates WebSocket
text frames to the adapter's JSONL pump. The worker/gateway protocol remains
independent and WSS-only. A direct-stdio adapter mode remains available.

Consequences: this explicitly extends the spec's direct-stdio process topology
to fulfill the user's CLI requirement. There is no public Codex listener, no PID
adoption and no multiplexing of multiple JSONL writers. Closing the owned adapter
reaps both children. `codex-local attach` uses the same Unix socket. `codex-local
start` owns a fresh runtime for the lifetime of its attached terminal CLI.

Verification: the installed Codex 0.154.0 passed the opt-in initialize,
loaded-thread-list and stored-thread-list smoke test without starting a turn.

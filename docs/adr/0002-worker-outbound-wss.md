# Outbound worker connections

Context: developer machines should not require public listeners or port forwarding.

Decision: each worker connects outbound to the gateway over WSS with an
individually revocable 256-bit token. Store only its SHA-256 hash centrally.

Consequences: reconnects replace a database-fenced connection lease. Heartbeats
report connectivity independently from session execution state.

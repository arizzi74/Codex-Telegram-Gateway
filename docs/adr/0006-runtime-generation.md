# Runtime generations and uncertain outcomes

Context: PIDs can be reused and delayed controls must not reach a replacement process.

Decision: persist a stable runtime UUID and increment its generation before each
spawn. Every command and approval addresses an exact generation. Persist a local
ledger entry before ACK and a durable event before sending it.

Consequences: stale controls fail. A crash during an unconfirmed RPC becomes
outcome_unknown and is not replayed blindly. The system makes no exactly-once claim.

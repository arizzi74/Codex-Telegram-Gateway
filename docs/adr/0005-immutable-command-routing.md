# Immutable command targets

Context: selecting another session while dispatch is delayed must not retarget work.

Decision: freeze worker, runtime generation, session, thread, operation, expected
turn, payload and expiry in the acceptance transaction. A SQL trigger rejects
changes to these fields. Dispatch reads this saved command.

Consequences: selecting a session changes only the complete Telegram context
binding. Replies to mapped bot messages can still address an earlier session.

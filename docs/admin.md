# Administrator console

The passkey-only console is served at `/tgadmin/`. The gateway enables it with
the configured public origin and read-only Telegram bot status checks.
`PublicBaseURL` must be the gateway's exact public HTTPS origin, such as
`https://gateway.example.com`. WebAuthn derives its relying-party ID from that
origin's hostname; browser requests must match the full configured origin,
including its port when one is specified.

The `/tgadmin/` console and `/tgapi/v1/admin/` endpoints share that origin.
Keep the origin configuration free of a path prefix so existing passkeys remain
bound to the same hostname when URL routes change.

Run `codex-gateway admin bootstrap` locally to create a bootstrap token. It is
shown once, stored only as a SHA-256 hash, expires after 15 minutes, and is
atomically consumed when the first resident, user-verified passkey is saved.
There is no open signup path and no password fallback.

The browser uses these endpoints:

| Endpoint | Purpose |
| --- | --- |
| `POST /tgapi/v1/admin/passkeys/register/begin` | Begins bootstrap or an authenticated additional-passkey ceremony. |
| `POST /tgapi/v1/admin/passkeys/register/finish` | Verifies and persists a registration response. |
| `POST /tgapi/v1/admin/login/begin` / `finish` | Begins and completes a discoverable passkey login. |
| `GET /tgapi/v1/admin/dashboard` | Returns visible sessions and statistics, workers, runtimes, bot status, and pending counts. |
| `GET, DELETE /tgapi/v1/admin/passkeys[/{id}]` | Lists credentials or revokes a non-final credential. |
| `POST /tgapi/v1/admin/workers` | Creates a worker and returns its one-time enrollment token. |
| `DELETE /tgapi/v1/admin/workers/{id}` | Revokes a worker. |
| `POST /tgapi/v1/admin/workers/{id}/rotate-token` | Returns a replacement enrollment token. |

All state-changing requests require the exact configured `Origin`, the strict
same-site session, and a double-submit `X-CSRF-Token`. Ceremony cookies bind
the browser to a five-minute, server-persisted WebAuthn session. Admin sessions
are opaque hashed random tokens, expire after eight hours, and can be revoked.
Worker tokens are only included in the create/rotate response and are never
logged or returned later.

Login initiation allows 10 attempts per client per minute and 120 globally;
completion allows 20 per client and 240 globally. Each phase has a separate
budget, so repeated initiations do not consume an existing ceremony's finish
budget. Throttled requests return HTTP 429 with `Retry-After: 60`. The registry
atomically caps live ceremonies at 1,024, removes successful ceremonies in the
credential/session transaction, and prunes expired and old consumed rows in
indexed batches during admission and once per minute. Existing accumulated
rows are drained gradually, without a large blocking cleanup transaction.

## Session and bot information

The session count and searchable list use the same non-archived inventory as
`/tgsessions`. Retired subagent/helper records remain stored for reconciliation
but do not appear in the console. Session names wrap fully on narrow screens.

Each session shows its state, worker, directory, prompt and final-reply counts,
token usage, active-turn duration, and latest user or assistant message. Expand
its details for creation and activity timestamps, model and reasoning effort,
token breakdown, context usage, Git branch, runtime version, identifiers, and
pending approvals or commands. “Active since” refers to the current turn;
“Session created” is the start of the saved conversation.

Workers collect statistics from saved Codex conversations, including prompts
sent through terminal clients and Telegram, during discovery (normally every
30 seconds). Reasoning and tool output are excluded from the last-message
preview and reply count. Previews are redacted and limited to 600 characters.
Large histories are scanned incrementally; incomplete message counts are lower
bounds, and unavailable values appear as `—`. Older workers can continue to
connect without providing these statistics.

Total tokens are the latest cumulative usage recorded by Codex, not a sum of
repeated usage events. Cached input tokens are included in input tokens, and
reasoning tokens are included in output tokens. Context usage is the most
recent request's recorded token usage against the reported context window.

The bot panel shows its Telegram identity, username link, webhook status,
pending updates, latest reported webhook error, and configured access-list
counts. A connected status means Telegram successfully verified the bot's
identity; webhook health is reported separately. Status checks are read-only,
cached for 30 seconds, and never return the bot token or webhook secret.

The visible console refreshes every 15 seconds and also provides a manual
refresh button. Existing results remain visible if a refresh temporarily fails.

For the optional browser regression check, use an existing Playwright setup:

```sh
PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-admin-ui.cjs
```

This is a development check only; installation and the running gateway do not
require Node.js or Playwright.

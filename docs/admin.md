# Administrator console

The administrator console is served at `/tgw/admin/`. Passkeys are available by
default; an administrator can also enable username/password sign-in for both
this console and the Codex Web UI. The gateway enables the console with
the configured public origin and read-only Telegram bot status checks.
`PublicBaseURL` must be the gateway's exact public HTTPS origin, such as
`https://gateway.example.com`. WebAuthn derives its relying-party ID from that
origin's hostname; browser requests must match the full configured origin,
including its port when one is specified.

The `/tgw/admin/` console and `/tgw/api/v1/admin/` endpoints share that origin.
Keep the origin configuration free of a path prefix so existing passkeys remain
bound to the same hostname when URL routes change.

Run `codex-gateway admin bootstrap` locally to create a bootstrap token. It is
shown once, stored only as a SHA-256 hash, expires after 15 minutes, and is
atomically consumed when the first resident, user-verified passkey is saved.
There is no open signup path. Password sign-in stays disabled until the
administrator configures it after signing in.

## Optional username and password

Sign in at `/tgw/admin/` and find **Password sign-in**. Enter a username, a
password and its confirmation, then save. Both login screens will offer
**Use password** alongside the passkey button. This is another way
to access the same administrator account, rather than a separate user account.

Usernames contain 1–64 UTF-8 bytes after trimming surrounding whitespace and
cannot contain control characters. Passwords contain 12–256 UTF-8 bytes;
spaces are preserved. Use a unique password or passphrase. The gateway stores
only a salted Argon2id hash in its SQLite database, using 64 MiB of memory and
three iterations. Passwords are never returned by an API or saved in browser
storage.

Saving, changing or disabling password access requires authentication within
the previous five minutes. Changing or disabling it revokes all existing
password-authenticated browser sessions and their notification subscriptions
and recovered drafts; passkey sessions remain valid. If the current browser
used the old password, it must sign in again. At least one passkey is kept as a
recovery method. Password access does not alter Telegram user/chat allowlists
or worker tokens.

Password login allows five attempts per client per minute and 30 globally,
independently of the passkey budgets. At most two password hashes run at once;
additional concurrent attempts receive HTTP 429. Failed username/password
combinations and disabled password access return the same generic response.
Password login and configuration use the same exact-origin and CSRF checks as
the existing browser authentication.

The browser uses these endpoints:

| Endpoint | Purpose |
| --- | --- |
| `POST /tgw/api/v1/admin/passkeys/register/begin` | Begins bootstrap or an authenticated additional-passkey ceremony. |
| `POST /tgw/api/v1/admin/passkeys/register/finish` | Verifies and persists a registration response. |
| `POST /tgw/api/v1/admin/login/begin` / `finish` | Begins and completes a discoverable passkey login. |
| `GET /tgw/api/v1/admin/login/options` | Reports whether password sign-in is enabled, without disclosing the username. |
| `POST /tgw/api/v1/admin/login/password` | Verifies the configured username/password and creates or renews a browser session. |
| `GET, PUT, DELETE /tgw/api/v1/admin/password` | Reads, configures or disables optional password access for an authenticated administrator. Writes require recent authentication. |
| `GET /tgw/api/v1/admin/session` | Returns the current login's identity, authentication method, server time, expiry and last verification time. |
| `GET /tgw/api/v1/admin/sessions` | Lists this account's active browser logins, approximate browser labels and last-seen times. |
| `DELETE /tgw/api/v1/admin/sessions/{id}` | Revokes one browser login and its notification subscriptions. |
| `POST /tgw/api/v1/admin/sessions/revoke-all` | Signs out every browser belonging to the account. |
| `GET /tgw/api/v1/admin/dashboard` | Returns visible sessions and statistics, workers, runtimes, bot status, and pending counts. |
| `GET, DELETE /tgw/api/v1/admin/passkeys[/{id}]` | Lists credentials or revokes a non-final credential. |
| `POST /tgw/api/v1/admin/worker-enrollments` | Creates a one-use enrollment URL with a 10-minute expiry and the selected service access. |
| `DELETE /tgw/api/v1/admin/worker-enrollments/{id}` | Revokes an unused enrollment URL. |
| `POST /tgw/api/v1/admin/workers` | Legacy direct worker creation; returns its authentication token once. |
| `DELETE /tgw/api/v1/admin/workers/{id}` | Revokes a worker. |
| `POST /tgw/api/v1/admin/workers/{id}/rotate-token` | Returns a replacement worker authentication token. |

All state-changing admin requests require the exact configured `Origin`, the strict
same-site session, and a double-submit `X-CSRF-Token`. Ceremony cookies bind
the browser to a five-minute, server-persisted WebAuthn session. Admin sessions
are opaque hashed random tokens, expire after eight hours, and can be revoked.
Worker tokens are only included in the installer redemption or legacy
create/rotate response and are never logged or returned later.

The eight-hour lifetime is absolute: requests, WebSocket traffic and ongoing
Codex turns do not extend it. Five minutes before expiry, both interfaces show
**Continue with passkey**, with password renewal also available when enabled.
Successful verification with either method creates a new eight-hour
session, invalidates the previous token and transfers that browser's notification
subscriptions. Cancellation leaves the existing login valid until its original
expiry. Registration of a passkey itself has no automatic expiry.

Adding or removing passkeys and enrolling, revoking or rotating a worker require
authentication within the previous five minutes. The console asks for a
passkey or the configured password when needed; a rejected or cancelled
verification performs no mutation.
Both stages of additional-passkey registration enforce this check. Browser
session revocation remains available without another authentication prompt.

## Worker enrollment

Choose **Enroll worker** to obtain a URL such as
`https://gateway.example.com/tgw/enroll/#ABCD2345WXYZ`. The 12-character code
uses letters and digits without ambiguous `I`, `O`, `0`, or `1`; lowercase
letters also work. Copy the URL or type it on the worker machine. The dialog
includes the installer command and a countdown. Creation requires a passkey
or password verification within the last five minutes.

Run the worker installer as the project account, without sudo:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sh
```

A fresh setup asks for a worker display name and the enrollment URL. It
retrieves the worker ID, authentication token, gateway connection address and
service access automatically. New workers start in the account's home
directory and can access its subfolders. Codex installation and account
sign-in are handled before redemption; provider sign-in or enabling service
startup after logout may require account approval or a system password.

The dialog defaults to restricted service access. On Linux this blocks
privilege elevation, including sudo. Select full access before copying the
URL to use the account's normal permissions and sudo rules. Full access does
not grant new privileges or change Codex session permissions. macOS workers
use the account's normal permissions.

Closing the dialog keeps its URL valid until it is used or expires. **Cancel
and revoke** invalidates an unused URL. Changing service access or creating a
replacement first revokes the previous URL. A worker appears in the inventory
only after redemption, under the name entered on its machine. Revoking a used
URL does not revoke that worker; use its worker revoke action instead.

Enrollment uses these public endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /tgw/enroll/` | Shows installer instructions. Opening a link never redeems it. |
| `POST /tgw/api/v1/worker-enrollments/redeem` | Exchanges `{code, name, os, arch}` for `{worker_id, token, gateway_url, service_access}` once. |

The code is a temporary secret, valid for exactly 10 minutes from creation.
Its URL fragment is not sent in browser HTTP requests or normal access logs.
The registry persists only its SHA-256 hash. Redemption atomically creates the
worker, stores its token hash and consumes the code. Concurrent attempts can
produce only one worker. Expired, revoked, consumed and unknown codes all
return HTTP 410; malformed requests return 400. Redemption is limited to 20
requests per client per minute and 240 globally, with HTTP 429 and
`Retry-After: 60` when throttled. Browser redemption requests must match the
configured origin; command-line requests do not need an Origin header.

The installer saves returned credentials in private recovery storage before
installing the service. If installation is interrupted, rerunning the same
command resumes with the saved identity. A lost redemption response is not
automatically replayed: inspect the worker inventory, revoke any incomplete
worker created by that attempt, and generate a fresh URL. Worker credentials
remain valid after the enrollment URL expires, until rotated or revoked.

## Browser sessions

The **Browser sessions** panel shows active, unexpired logins and can revoke one
or sign out everywhere. Labels such as **Safari on iOS** are approximate browser
classifications, not authenticated device identities. **Last seen** includes
authenticated background requests. A synced passkey can be used by more than one
browser. Session cookies remain Secure, HttpOnly and SameSite=Strict; neither
session tokens nor passkey secrets are exposed in the session inventory.

Login initiation allows 10 attempts per client per minute and 120 globally;
completion allows 20 per client and 240 globally. Each phase has a separate
budget, so repeated initiations do not consume an existing ceremony's finish
budget. Throttled requests return HTTP 429 with `Retry-After: 60`. The registry
atomically caps live ceremonies at 1,024, removes successful ceremonies in the
credential/session transaction, and prunes expired and old consumed rows in
indexed batches during admission and once per minute. Existing accumulated
rows are drained gradually, without a large blocking cleanup transaction.

## Session and bot information

The session count and searchable list include non-archived sessions retained
for administration. Retired subagent/helper records remain stored for
reconciliation but do not appear in the console. Session names wrap fully on
narrow screens.

Revoked workers and their sessions disappear from the Codex Web UI sidebar and
Telegram session pickers, including after a live inventory update. Their saved
records remain available in this administrator console for inspection; revoking
a worker does not delete its sessions or files. Enabled workers without any
sessions still appear in the Web UI so a first session can be created.

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

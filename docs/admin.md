# Administrator console

The administrator console is served at `/tgw/admin/`. Passkeys are available by
default; an administrator can also enable username/password sign-in for both
this console and the Codex Web UI. The gateway enables the console with
the configured public origin and Telegram bot management.
`PublicBaseURL` must be the gateway's exact public HTTPS origin, such as
`https://gateway.example.com`. WebAuthn derives its relying-party ID from that
origin's hostname; browser requests must match the full configured origin,
including its port when one is specified.

The `/tgw/admin/` console and `/tgw/api/v1/admin/` endpoints share that origin.
Keep the origin configuration free of a path prefix so existing passkeys remain
bound to the same hostname when URL routes change.

Console sections start collapsed: Telegram bots, Audio, Sessions, Workers,
Passkeys, Browser sessions and Password sign-in. Select a heading to expand or
close its section. Refreshes preserve open sections and unsaved audio settings.

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
| `GET /tgw/api/v1/admin/telegram-bots` | Lists connected bots and pending managed-bot requests without tokens or webhook secrets. |
| `GET /tgw/api/v1/webui/telegram-bots` | Lists available bots by base display name and their private-chat session assignments, without credentials. |
| `POST /tgw/api/v1/webui/telegram-bots` | Assigns a bot using `{"session_id":"UUID","bot_id":"ID"}`; an empty bot ID disconnects the session's bot. Requires a valid browser session, exact Origin and CSRF. |
| `POST /tgw/api/v1/admin/telegram-bots/refresh` | Refreshes enabled bots' Telegram profiles and returns bots and pending requests; requires a valid session, exact Origin and CSRF, without recent authentication. |
| `POST /tgw/api/v1/admin/telegram-bots` | Connects an existing bot with its token, or replaces a token when `replace_id` is supplied. |
| `POST /tgw/api/v1/admin/telegram-bots/managed` | Creates a managed-bot request and a Telegram confirmation link using `manager_id` and `display_name`; the gateway generates and checks the username. The explicit `username`/`name` pair remains supported for existing clients. |
| `POST /tgw/api/v1/admin/telegram-bots/managed/{id}/retry` | Replaces an expired, unclaimed setup request with a fresh confirmation link; requires `{"confirmed":true}` and recent authentication. |
| `DELETE /tgw/api/v1/admin/telegram-bots/managed/{id}` | Deletes an unclaimed setup request from the gateway; requires `{"confirmed":true}` and recent authentication. It does not delete a Telegram bot. |
| `PATCH /tgw/api/v1/admin/telegram-bots/{id}` | Enables or disables a bot with `{"enabled":true}` or `{"enabled":false}`. |
| `POST /tgw/api/v1/admin/telegram-bots/{id}/profile-photo/reset` | Replaces the bot's profile photo with the purple Gateway icon; requires `{"confirmed":true}`, an enabled bot and recent authentication. |
| `DELETE /tgw/api/v1/admin/telegram-bots/{id}` | Removes an additional bot and its local records and subscriptions; requires `{"confirmed":true}`. |
| `GET /tgw/api/v1/admin/audio` | Returns audio settings, gateway CPU/RAM resources, installed models and setup progress. |
| `POST /tgw/api/v1/admin/audio/settings` | Downloads required models and applies local audio settings asynchronously; requires recent authentication. |
| `GET, DELETE /tgw/api/v1/admin/passkeys[/{id}]` | Lists credentials or revokes a non-final credential. |
| `POST /tgw/api/v1/admin/worker-enrollments` | Creates a one-use enrollment URL with a 10-minute expiry and the selected service access. |
| `DELETE /tgw/api/v1/admin/worker-enrollments/{id}` | Revokes an unused enrollment URL. |
| `POST /tgw/api/v1/admin/workers` | Legacy direct worker creation; returns its authentication token once. |
| `DELETE /tgw/api/v1/admin/workers/{id}` | Revokes a worker. |
| `DELETE /tgw/api/v1/admin/workers/{id}/purge` | Permanently deletes a revoked worker and its related gateway database records; requires `{"confirmed":true}`. |
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

Adding or removing passkeys and enrolling, revoking, deleting or rotating a
worker require authentication within the previous five minutes. The console asks for a
passkey or the configured password when needed; a rejected or cancelled
verification performs no mutation.
Both stages of additional-passkey registration enforce this check. Browser
session revocation remains available without another authentication prompt.

## Telegram bots

The **Telegram bots** panel shows the configured **Primary** bot and each
additional **Connected** or **Managed** bot in a collapsed subsection. Its header
shows the bot name and **Enable**/**Disable** and **Delete** controls. Expand the
name to see its username, connection status, **Open Telegram** link and settings.
The primary bot has no deletion control because its configuration is managed on
the gateway host. Expansion choices survive dashboard refreshes.
All bots inherit the gateway's existing Telegram
user/chat access rules and share its workers. Adding a bot does not create a
separate administrator or grant another Telegram user access. Chat bindings
and notifications belong to the bot used to create them, so the same chat can
use different bot conversations independently.

Open each bot in Telegram, send `/start`, and use `/tgsessions` in that bot to
choose its session. In your private bot conversations, each session has one
selected bot. Selecting a session already assigned to another bot disconnects
that previous bot; moving a bot to another session clears its previous
assignment. The Web UI's Telegram bot dropdown uses these same assignments and
updates across open browser views. Group and forum-topic selections remain
independent. `/tgmultisession` is configured separately for each bot chat and
is off by default.

Each bot has a **Broadcast session questions to all bots** checkbox, off by
default. When a session has a selected bot, its questions and approvals are sent
only to that bot. Enable the checkbox on the selected bot to notify all enabled
bot chats you have opened, including chats following other sessions or no
session. The selected bot's setting controls broadcasting; other bots' checkbox
values do not prevent them from receiving a broadcast. A session with no
selected bot always notifies all eligible bot chats. Disabled bots and
disallowed chats do not receive these messages.

Answer from any delivered copy without changing either bot's selection. Once a
question is answered, every delivered copy is edited to show **Question:** and
**Answer:** and its buttons are removed. For a request containing several
questions, each next question follows the same routing rule. You can still use
`/tgquestions` to open a pending request from any session manually. The preference
survives restarts, profile refreshes and token replacements; new bots default to
off.

Each bot has an **Inherit selected session name** checkbox, off by default.
When enabled, its Telegram display name follows the session selected in the
gateway owner's private chat, for example **EnkiCodex · CodeNow**. Session
switches and session renames update the name automatically. Disconnecting the
session or clearing the checkbox restores the original display name.
The `@username` and gateway routing identity stay unchanged.

Telegram display names are global to a bot, so selections in groups and topics
do not rename it. Names are shortened to Telegram's 64-character limit. Updates
run in the background and retry if Telegram is unavailable or limits requests;
the console shows pending updates or a name-sync warning. Disabled bots do not
update their profiles until enabled again. Telegram clients may cache the name.
This changes the default name; an existing language-specific name configured
in Telegram can take precedence for users of that language.
See Telegram's [`setMyName` documentation](https://core.telegram.org/bots/api#setmyname).

Choose **Connect existing bot**, obtain its token from `@BotFather`, and submit
it in the hidden token field. The gateway verifies the bot identity and
registers its webhook and command menu. If another application already owns
the webhook, the console requires a separate confirmation before moving it;
that application will stop receiving new Telegram updates. Tokens are cleared
from the form on submission or close and are never saved in browser storage or
returned by the bot list API. Errors do not display token values.

Choose **Create managed bot** to create a bot through Telegram without pasting
its token. Enable **Bot Management Mode** for your chosen manager bot in
`@BotFather`, select that enabled manager, and enter the new bot's display name.
**Create Telegram link** generates a lowercase username, replaces spaces and
unsupported characters with underscores, and appends `_bot`. For example,
**My Project** becomes `my_project_bot`. Names are shortened when necessary to
fit Telegram's 32-character username limit; a letter prefix is added when needed.

The gateway checks its connected bots and pending requests, then looks up the
candidate's public Telegram profile. If occupied, it adds a number before
`_bot`, for example `my_project1_bot`, and checks again. Lookup failures and
unrecognized Telegram pages stop creation with a retryable error. Checks have
time and attempt limits, and no bot credentials are sent to the public lookup.

The selected username appears with the confirmation link. **Open Telegram to
create bot** opens Telegram's confirmation page. Public profile checks do not
reserve a username or detect every reserved or unavailable name; Telegram makes
the final availability check during confirmation. Its authoritative
[`bots.checkUsername` method](https://core.telegram.org/method/bots.checkUsername)
requires a signed-in Telegram user and cannot be called with the gateway's bot
token. Keep the requested username when confirming so the gateway can match the
callback. If Telegram rejects it or you want another name, return to the console
and create another link.

The API returns the selected `requested_username` and
`username_availability: "telegram_confirmation_required"` while the request is
pending. Existing API clients may still supply `username` and `name` instead of
`display_name`; those explicit usernames retain their existing validation and
are checked against local reservations.

After Telegram confirms creation, the gateway connects the new bot and sets
the purple Gateway icon as its default profile photo. The creation dialog shows
a preview. The image is bundled with the gateway and uploaded as a JPEG through
[`setMyProfilePhoto`](https://core.telegram.org/bots/api#setmyprofilephoto).
If the bot already has a photo, it is kept. Temporary photo failures leave the
creation request pending so Telegram's callback can retry; the connected bot
can already handle messages. Retries and gateway restarts check for an existing
photo before uploading again. Once setup finishes, later bot updates do not
restore the default or replace a picture you choose.

To apply the icon to an existing bot, expand its entry and choose **Reset profile
photo >_**. Confirm the named bot to replace its current profile photo with the
Gateway icon. This is available for the primary bot too. Enable a disabled bot
before resetting its photo. The gateway verifies the bot's identity and its
current webhook before uploading; it does not move the webhook or change the
bot's session selection. A failed reset reports an error and can be retried.

The gateway refreshes the selected manager's Telegram profile before checking
Bot Management Mode and creating the link. **Refresh manager bots** explicitly
refreshes all enabled bots' profiles, including their current management
capability. Ordinary dashboard and pending-request polling use cached profiles.
If Telegram still reports that the selected bot cannot manage bots after the
profile refresh, check that Bot Management Mode is enabled for that bot in
`@BotFather`, then retry.

The request allows 15 minutes for automatic connection. Its pending status and
remaining confirmation window appear in the console, which polls while the
request is active; **Refresh status** checks immediately. The bot appears in
the list after Telegram confirms creation and the gateway connects it. The
gateway confirmation window is separate from Telegram's creation link: an old
link may still create a bot after automatic connection has expired.

An expired, unclaimed request has a **Create new link** button. This checks the
manager and username again, replaces the old request, and opens a fresh
15-minute confirmation link. The display name is kept, and a different username
is selected so the old link cannot match the replacement request. A failed retry
leaves the old request available for another attempt.

Use **Delete request** to remove an abandoned setup request after confirmation.
This only removes its gateway record. If you already created the bot in
Telegram, use **Connect existing bot** with its token instead. A request being
activated cannot be retried or removed through these controls; manage its bot
from the connected bot entry.

**Disable** stops a bot's gateway access while retaining its configuration and
local bindings. **Enable** restores it. For an additional bot, **Replace token**
accepts a replacement token for the same Telegram identity. **Delete**
opens a dialog with the bot's username and two separate actions:

- **Remove from gateway** confirms deletion of its local bot records, chat
  bindings and notification subscriptions. Worker sessions and project files
  remain on disk.
- **Delete in BotFather** opens Telegram's bot-deletion flow. Select the named
  bot and confirm there to permanently delete the Telegram bot. The link and
  username remain available after removing the gateway connection.

The gateway cannot complete Telegram account deletion using a bot token.
Telegram documents the owner-confirmed
[BotFather deletion link](https://core.telegram.org/api/bots/managed-bots).
Removing a gateway connection alone does not delete the bot from Telegram;
cancelling in BotFather leaves the Telegram bot intact.
The primary bot can be enabled or disabled; its token and removal remain
managed by the existing gateway configuration.

If Telegram reports that a managed bot changed owner, the gateway disables it
and blocks enabling or replacing its token until Telegram verifies the
configured owner again. You can also remove it and explicitly connect it again
using its current token. Enabling a disabled bot discards updates Telegram held
while it was disabled.

All bot configuration changes require authentication within the previous five
minutes and the usual exact-origin and CSRF checks. Refreshing manager profiles
uses the current authenticated session and the same Origin and CSRF checks,
without requiring recent authentication. If a request fails ambiguously, refresh
the list to check its outcome before retrying. Additional bot credentials are
encrypted in SQLite. Back up both the database and its private companion key,
`<database path>.telegram-bots.key`, together; the key is required to restore
those bots after a restart or recovery.

### Telegram iOS profile-photo banner

Telegram iOS shows **Set profile photo** for managed bots without a profile
picture. In the reviewed [Telegram iOS source](https://github.com/TelegramMessenger/Telegram-iOS/blob/f1dd7a2dbd02cbbf513e75d5695d8d36d1cf5838/submodules/TelegramUI/Sources/ChatInterfaceTitlePanelNodes.swift#L147),
this condition does not consult the normal dismissal state, which can explain
why the close button does not keep the banner hidden. The gateway does not send
this banner, and the bot API has no dismiss-banner operation. New bots created
through the console receive the default Gateway photo automatically, removing
the missing-photo condition. For an existing bot, use **Reset profile photo >_**
in Admin, set a custom photo from Telegram, or use
[`/setuserpic` in BotFather](https://core.telegram.org/bots/features#edit-bots)
to remove the missing-photo condition.

## Audio

In **Audio**, select **Enable voice notes**, choose a Whisper model, then choose
**Download and apply**. **Base** is the recommended multilingual model;
**Small** is an advanced option with a larger download and memory budget. The
panel shows gateway CPU cores, the CPU cores available to its process, and total
and available RAM. The recommended thread count is one fewer available CPU core,
with a minimum of 1 and a maximum of 32. Existing model and thread choices are
preserved; **Use recommended** applies the current recommendation to the form.

Applying settings requires authentication within the previous five minutes.
Download and validation progress appears in the panel. Current settings remain
active until setup succeeds, and failed setup keeps the proposed changes so they
can be corrected or retried. Manual and automatic dashboard refreshes preserve
unsaved form edits. **Discard changes** restores the currently applied settings.
Audio settings are saved on the gateway and survive browser reloads.

When the local speech engine is available, **Reply to voice notes with audio**
adds an audio version alongside the final text reply to a voice prompt. This
option is disabled by default; typed prompts continue to receive text. Checking
it shows two engine choices:

| Engine | Quality and languages | One measured sample | Model files |
| --- | --- | --- | --- |
| **Piper / Miro** — recommended for this CPU | Fast, clear Italian; English has an Italian accent. The Miro voice has a noncommercial license; check its terms before use. | 3.8 seconds total for 25.2 seconds of audio; 298 MiB peak RAM. | About 77 MiB. |
| **Qwen3-TTS 1.7B quantized** | Potential for expressive multilingual speech, but slower and needs much more RAM. Technical terms and numbers can still be misread. | About 78 seconds total for 20.3 seconds of audio; 5 GiB peak RAM. | 1.38 GiB: Q4_K_M 1.7B model and Q8 decoder. |

Both measurements used CPU inference with three threads on the four-core ARM
iaiabox. They are individual samples, not averages or universal requirements.
The Qwen measurement uses the resident release helper with its optimized CPU
build: 2.8 seconds to initialize and 74.2 seconds to synthesize the sample. Older
processors use the portable fallback and can take substantially longer.
Speed and memory vary with the hardware, text, and reply length. Compare these
estimates with the gateway resources shown in the panel.

For Qwen3-TTS, an 8 GiB host with roughly 6 GiB of RAM currently available is a
practical starting budget so the operating system, gateway, and Whisper retain
headroom beyond the observed 5 GiB speech peak. This is a recommendation,
not a fixed requirement: concurrent workloads and the text can change the
memory needed. Piper / Miro leaves much more room on smaller CPU hosts.

Choose **Italian**, **English**, or **Match the voice note (Italian or English)**.
Qwen3-TTS uses the same model download for both languages. Piper / Miro always
uses its Italian voice; the language preference is preserved for an engine
switch. Transcription can be enabled independently of audio replies.

The selected engine downloads and passes a local check before settings are
saved. Download, validation, and warmup progress appears in the panel. A failed
download, check, or settings save leaves the previous engine active. After a
successful save, the gateway activates the selected engine and removes unused
managed speech models, including the old Pocket TTS language models. Keep
enough free disk space for both engines while switching. A cleanup failure is
reported as a warning after the selected engine has been activated.

Existing saved settings without an engine selection keep Pocket TTS active,
including after a restart. The panel shows a pending engine selection and
recommends Piper / Miro; select an engine and apply settings to switch explicitly.
Loading the panel does not download, delete, or migrate anything.

The typing indicator remains active while all reply audio is prepared. Once
ready, the bot sends the complete final text first, then the audio. Long audio
replies are split into voice notes without summarizing the final answer. If
speech generation ultimately fails, the text is sent on its own.

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

A fresh setup asks for a worker display name and the enrollment URL, with the
URL visibly echoed. Linux setup then asks **Restrict this worker service?**
before redemption. It retrieves the worker ID, authentication token and gateway
connection address automatically. New workers start in the account's home
directory and can access its subfolders. Codex installation and account
sign-in are handled before redemption; provider sign-in or enabling service
startup after logout may require account approval or a system password.

The installer's Linux restriction question defaults to **yes**, setting
`PrivateTmp`, `PrivateUsers`, and `NoNewPrivileges` to `yes` and blocking
privilege elevation, including sudo. Answer **no** to set all three to `no` and
use the account's existing permissions and sudo rules. This grants no new
privileges and does not change Codex session permissions. The local answer
determines installed access, overriding the service access returned by the
enrollment API. The dialog's access selection remains for older installers.
macOS skips this question and uses normal account permissions through launchd.

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
command resumes with the saved identity and access choice. Existing installations
and prepared configurations do not receive the fresh-enrollment restriction
question. A lost redemption response is not automatically replayed: inspect the
worker inventory, revoke any incomplete
worker created by that attempt, and generate a fresh URL. Worker credentials
remain valid after the enrollment URL expires, until rotated or revoked.

## Removing a worker

**Revoke** disables the worker's credentials and connection, while retaining its
records for inspection. A revoked worker has a **Delete** button in the Workers
panel. The confirmation identifies the worker and explains that deletion
permanently purges all related gateway database records, including runtimes,
sessions, synchronized history, commands, questions, enrollment records, and
Telegram routing and delivery records. Cancellation leaves those records intact.

Deletion requires authentication within the previous five minutes and an
already revoked worker. The purge runs in one database transaction; any failure
leaves the records intact. Other workers and administrator access are preserved.
Shared chat settings and anonymous Telegram update receipts are retained; the
receipts contain no worker content or linkage and prevent old webhook retries
from executing again after deletion.
Project files and native Codex session history on the worker are untouched;
the worker service is not uninstalled. Existing database backups are not rewritten.

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
reconciliation but do not appear in the console. The Sessions panel and each
session card start collapsed. Expand a session's name to inspect it; each card
has its own expansion state, retained across refreshes, filtering and pagination.
Session names wrap fully on narrow screens.

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

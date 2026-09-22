# Telegram commands

Gateway commands begin with `/tg`. Unprefixed commands belong to Codex.
The initial Telegram `/start` button is an alias for `/tgstart`.
All commands are registered in the bot menu. Telegram requires underscores in
menu names, so `/debug_config` is the menu spelling of `/debug-config`; both work.

## Gateway controls

| Command | Action |
| --- | --- |
| `/tgstart`, `/tghelp` | Show gateway help. |
| `/tginstances` | List workers and their runtimes. |
| `/tgupdateworkers` | Queue an update check for every worker; install a newer release and restart after its turns finish. |
| `/tgsessions [runtime]` | Browse sessions, select one, or create a session with **New session**. |
| `/tgstatus [session]` | Show gateway connectivity, queued commands and approvals. |
| `/tghistory [count]` | Show saved user and Codex messages for the selected session, including Telegram input; original order with newest last, defaults to 2, maximum 50 per page. |
| `/tglastmessages [count]` | Show the last saved user or Codex message; request 1–50 messages from both sides, in original order with newest last. |
| `/tgmultisession [on\|off]` | Toggle messages from all sessions, or explicitly enable/disable them. |
| `/tgdisconnect` | Clear the selection. |
| `/tgdeletesession [runtime]` | Choose a Codex session to delete while keeping its working directory and files. |
| `/tgsteer TEXT` | Guide the exact active turn. |
| `/tginterrupt` | Interrupt the exact active turn. |
| `/tgquestions`, `/tginput` | List pending questions and approvals across all sessions; open one to answer without changing the selected session. |
| `/tginput APPROVAL_ID QUESTION_ID ANSWER` | Answer an input request; replying to its message is easier. |

Session lists are ordered by name. Each numbered entry shows the full session
name, with its state and workspace on separate lines. Selection buttons show the
number and session name, such as **1 Smart Stage**; long button labels end in
**…**. **Status 1** and the other numbered controls refer to the same entry.
Long pages continue across messages
without shortening session names; controls appear after the complete page. Long
workspace paths are shortened; **Status** shows full details. Page buttons are
bound to your user, chat, topic, and runtime generation, and do not change the
selected session or start any work. Run `/tgsessions` again if a button expires or
the worker restarts. Choosing a host, changing pages, or selecting a session
removes the previous picker, including all parts of a long session list. The
connection confirmation remains in the chat. Expired or rejected selections
leave the picker available.

Only user conversations inside the worker's allowed workspaces appear. Internal
helper-agent and ephemeral threads are excluded. Discovery also hides entries
that no longer exist after a complete successful scan; saved history is retained,
and this cleanup does not archive or delete threads in Codex. The CLI picker can
show a different count because it has its own source and directory filters.

`/tgupdateworkers` works without selecting a session. It queues every enabled
worker, including offline workers, and waits until each worker has no active
turns or pending work before installing a newer release. Workers already up to
date are not restarted. Repeated requests share an existing pending update;
offline workers receive it on reconnection, and requests survive restarts.
Completion or failure is reported to the requesting chat and topic. The command
does not change update schedules or trigger the separate daily Codex runtime
check. Gateway and worker v0.5.29 or later are required; older workers need one
local update first. See [requested updates](installation.md#request-worker-updates-from-telegram).

## Create and delete sessions

Use `/tgsessions`, choose **New session** and a runtime when prompted, then send the session name. The
folder browser starts at the worker user's `~/CODEX` if it exists, otherwise at
that user's home directory. Existing restricted workspace settings still apply.
Use numbered **Open** buttons to enter a subfolder, **Parent folder** to move up,
and Previous/More buttons to browse longer lists. Choose **Create here** to make
a new child directory and use it as the session's working directory.

The session keeps its display name; spaces become underscores in the directory
name. For example, **My Project** under `~/CODEX` creates `~/CODEX/My_Project`.
An existing file or directory with that name is preserved; choose another name
or parent folder. **Change name** returns to the name prompt, and **Cancel** ends
the setup. Name replies belong to the setup wizard and are not sent to Codex as
prompts. New-session buttons and `/new` or `/clear` use the same guided flow.

Use `/tgdeletesession` to browse the same session list as `/tgsessions`, with full
names, workspace information, and numbered delete buttons. Select a session and
confirm its name and working directory. This permanently deletes its Codex
conversation and any child sessions it spawned; the working directory and every
project file remain on disk. Sessions with an active turn or pending work, including in a child session,
cannot be deleted until that work ends. Deleting the selected session clears its
Telegram selection after Codex confirms deletion. Use `/tgsessions` to connect
to another session. `/archive` remains available for reversible archival.

The wizard and its buttons are scoped to the requesting user, chat, topic, and
runtime. Older buttons cannot change a later setup or delete a different
session. Creation and deletion require a worker with support for these actions.
During an upgrade, the gateway reports an update-required error and releases
the chat if the connected worker is too old.

After confirming creation or deletion, the bot waits for the worker's result.
**Continue chat** releases the input step without cancelling the submitted
request; its eventual result is still reported. If you send text while a step
is waiting, the bot shows fresh controls instead of sending that text to Codex.
Expired or rejected requests release the input step. Check `/tgsessions` before
retrying a request whose result is unknown.

## Codex controls

These run against the selected session and keep its immutable worker, runtime,
generation and thread target. Unknown slash commands produce help instead of
becoming model prompts.

| Command | Telegram behavior |
| --- | --- |
| `/status` | Session model, reasoning, workspace, recorded context and token counts, configuration defaults and account limits. Historical counters and policies are labeled as recorded values. |
| `/usage` | Account usage and rate limits reported by Codex. |
| `/model [MODEL [EFFORT]]` | Open model buttons followed by reasoning-effort buttons, or update the session model directly. |
| `/reasoning [EFFORT]` | Inspect or change reasoning effort. |
| `/permissions` | Open buttons for the session's Codex permission presets and allowed custom profiles. |
| `/approvals [POLICY]` | Inspect or change session approval policy. |
| `/fast [status\|on\|off]` | Inspect the configured tier or change the session tier when supported. |
| `/plan on\|off` | Enable or disable planning mode for subsequent turns. |
| `/personality [friendly\|pragmatic\|none]` | List styles or update the session's response style. |
| `/compact` | Request context compaction. |
| `/review [INSTRUCTIONS]` | Start a code-review turn. |
| `/init` | Ask Codex to prepare project instructions using its initialization prompt. |
| `/rename NAME` | Rename the session. |
| `/fork` | Branch the saved conversation into a new session. |
| `/new`, `/clear` | Name a new session and browse for its parent folder on the selected runtime. |
| `/resume [NAME_OR_ID]`, `/agent`, `/subagents` | Choose a saved session; execution resumes when its next turn is submitted. |
| `/goal [OBJECTIVE\|pause\|resume\|clear\|complete\|blocked]` | Inspect or update the persistent goal. |
| `/diff` | Show tracked changes and an untracked-file summary in the allowed workspace. |
| `/mcp [verbose]` | List MCP server status and tool counts. |
| `/apps`, `/skills`, `/plugins [SEARCH]`, `/hooks` | Inspect the corresponding Codex catalog. |
| `/memories [MODE]` | Show memory options or set the thread's memory mode. |
| `/ps` | List this session's background terminals. |
| `/stop`, `/clean` | Stop its background terminals. |
| `/copy` | Return the last assistant response as text. |
| `/archive` | Archive the session. |
| `/delete` | Open the same session-deletion picker as `/tgdeletesession`. |
| `/debug_config` | Show selected configuration diagnostics without secrets. |
| `/quit`, `/exit` | Leave the Telegram selection; the supervised runtime stays available. |
| `/help` | List Codex commands and guidance. |

The `/model` menu lists the models available to the session's Codex runtime.
Choose a model to see its supported reasoning efforts, then choose an effort to
apply both settings together. **Back to models** returns to the model list and
**Cancel** leaves the settings unchanged. Models without reasoning choices offer an
**Apply model** button. Opening and browsing the menu works during a running
turn; wait for it to finish before applying a change. The menu stays tied to the
session where you opened it, even if you select another session before answering.
Text shortcuts such as `/model MODEL high` and `/reasoning high` remain available.

The `/permissions` menu offers **Ask for approval**, **Full Access**, and
**Read Only**, subject to the runtime's managed requirements. **Approve for me**
appears when Codex's automatic approval review is enabled. Configured permission
profiles also appear when allowed. Full Access requires a second confirmation.
Opening or cancelling the menu does not change permissions. Changes apply to
future turns of that session; wait for a running turn to finish before applying
a choice. A menu remains tied to its original session if you switch sessions.

Text shortcuts `/permissions read-only` and `/permissions workspace-write`
select the corresponding presets. `/permissions full-access` opens the same
confirmation. These presets update the approval policy and reviewer together
with filesystem/network permissions, matching Codex's menu.

Commands that require a terminal picker, desktop integration, local credentials,
or an interactive confirmation show instructions for the attached CLI. This
includes `/keymap`, `/vim`, `/raw`, `/statusline`, `/title`, `/theme`, `/pets`,
`/app`, `/ide`, `/import`, `/logout`, `/feedback`, `/experimental`, `/side`,
`/btw`, `/mention`, `/approve`, `/rollout`, and Windows sandbox setup.
They remain discoverable in the menu. Telegram does not emulate a terminal UI.

The implementation uses typed app-server operations; Codex slash commands are
client actions, not prompt text. See the official
[Codex command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
and [app-server protocol](https://learn.chatgpt.com/docs/app-server).

## Existing session held by another Codex process

Selecting a saved chat does not transfer ownership from another Codex app.
If its writer lock is held elsewhere, the next turn reports `session_busy`
with recovery instructions. Close that other client before retrying, use
`/fork` to branch the saved conversation, or **New session** in `/tgsessions` to begin fresh.
The gateway never silently redirects a failed command to a different chat.

## Images

After selecting a session, send a photo or upload an image as a file. JPEG,
PNG, WebP and GIF files up to 10 MiB are supported. A caption accompanies the
image as prompt text; an image without a caption also starts a turn. Captions
beginning with `/` remain prompt text. Send slash commands separately.

For a photo with multiple resolutions, the gateway selects the largest version
that fits its size limit. Each image message is a separate submission; albums
are not combined into one turn. Video, audio, voice messages, stickers,
animations and non-image documents produce an unsupported-attachment reply.
Reply to input requests with text, and send images separately.

The gateway downloads images using Telegram's
[getFile API](https://core.telegram.org/bots/api#getfile), checks their content
type and byte size, and stores the bytes with the accepted command. The worker
passes those bytes to Codex; Telegram credentials and download URLs stay on the
gateway. Image bytes are retained with command records in the gateway and
worker databases, like prompt text. Download or validation failures produce a
reply with instructions to retry or use a supported file.

Both gateway and worker need v0.5.13 or later. During a rolling upgrade, an old
worker produces an update-required reply instead of receiving only the caption.
Images sent to earlier gateway versions were ignored before command storage;
resend them after the upgrade.

## Conversation history

Use `/tghistory` to display the selected session's last two saved messages in
their original conversation order, with the newest last. This includes your
prompts from Telegram or the CLI and final Codex replies. `/tghistory 25`
requests a larger page; use **Older messages** to continue backwards. Each
message is labelled with its session, role, and date
and time. The bot remains the Telegram sender; these are copies of saved
conversation messages. Commentary, reasoning and tool calls are not included.

Use `/tglastmessages` for the last saved message, or `/tglastmessages 10` for the
last ten. Both commands include the same user and Codex messages and show each
recent page oldest first. Both commands accept counts from 1 to 50.
Reading history never submits prompts again, starts a turn, resumes a cold
thread, or interrupts work already running. An unavailable history or an older
worker produces an explanatory error.

History is fetched from the newest turns backwards and stops when the requested
page and its older-page check are complete. Older pages are bounded instead of
loading the whole conversation. This requires a Codex runtime with
`thread/turns/list` support; older runtimes receive an update-required error.
The **Older messages** button is restricted to the requesting user, chat, topic,
session, and runtime. After selecting a different session or restarting the
runtime, run the command again. Repeating the command deliberately displays the
newest page again. Delivery retries preserve already-sent messages instead of
repeating the whole page.

Every displayed message, including continuations of long messages, includes a
saved timestamp in UTC. A verified timestamp from the local conversation record
is labelled **Message time**. When the exact message timestamp is unavailable,
the stored turn start or completion time is labelled **Turn time**. This fallback
can be much earlier than a follow-up prompt inside a long-running turn. If neither
is available, the message says **Turn date/time unavailable**. The gateway never
substitutes the time you requested history.
Official Codex builds can regenerate user-message IDs when reading history, so
those prompts use the turn-time fallback. Exact times require matching saved
turn and message identities in the bounded recent log; text is never guessed.

Images and attachments use placeholders; local attachment paths and file
contents are not read. Long messages are shortened with an explicit notice, and
large pages may contain fewer entries to stay within message limits. Configured
output redaction also applies to history. During a rolling upgrade, workers
that already support `/tglastmessages` can supply the full conversation; the
gateway also orders their history pages oldest first. Exact message timestamps
require the updated worker.

This is an on-demand view of stored conversation messages. CLI-only slash
commands may not be stored, and ephemeral or unavailable Codex history cannot
be reconstructed. See the [Codex history API](https://learn.chatgpt.com/docs/app-server#read-a-stored-thread-without-resuming).

## Pending questions and approvals

Use `/tgquestions` even when no session is selected. The list shows each session's
full name and whether the request is a question or an approval. Choose **Open**
to show fresh answer buttons or reply to that message with text. For requests
with several questions, it resumes at the first unanswered question and keeps
answers already submitted. **Refresh**, **Previous**, and **Next** update the list.
Opening or answering a request does not change the current session selection.

For a text answer, tap **Reply with text**. In a private chat, the bot sends an
**Answer for <session>** prompt and requests Telegram's reply composer. Type in
the normal message box at the bottom, then press **Send**. If Telegram does not
select the reply automatically, long-press that prompt and choose **Reply**
first; use the same steps in a group. You can also reply directly to the original
question. Keep the reply attached to the bot's question so the answer reaches
that question's session, even when another session is selected. Ordinary messages
without a reply continue to use the currently selected session.

Only tapping **Reply with text** opens the composer; incoming questions and
opening `/tgquestions` do not interrupt a message you are already typing. Use
the original question's buttons or `/tgquestions` to choose an option or dismiss
the request after opening text reply mode.

Answered requests and requests for archived sessions or superseded runtimes are
excluded. Ordinary blocking questions expire when their turn ends. Asynchronous
questions remain answerable while Codex continues working, including across
turns; answering one sends the response to its originating session. Old buttons
are checked again before any response is sent.

Only questions received by the running worker enter its pending list. After a
restart, saved history reconciles existing records; it does not import old
questions as new requests. Replies to specific questions clear those questions
and preserve the others. An ordinary new user prompt supersedes earlier pending
questions in that session, including plain replies such as "Proceed". This does
not send inferred answers. Environment and instruction metadata do not clear
questions. Records imported by v0.5.25 are reconciled against later replies on
the next worker restart.

Answering a question edits its original Telegram message in place:

```text
Question: Does the pointer disappear when captured?

Answer: The pointer disappears when captured.
```

The answer replaces the pending instructions and buttons. A separate **Reply
with text** prompt for that question is updated too. Each answered field in a
multi-question request is updated separately, while the remaining questions
stay answerable. Edits keep working when another session is selected and are
retried after a temporary Telegram failure.

For tracked asynchronous questions, the worker recognizes answers submitted
through the terminal's native question UI and preserves their response text.
It also reconciles answers from saved history when recovering a pending
request. An ordinary prompt or a dismissal is not treated as an answer.
Blocking prompts answered by another client may only emit
`serverRequest/resolved`, which contains request identifiers but no answer
text; those cannot receive a question-and-answer edit from that notification
alone. See the [official App Server documentation](https://learn.chatgpt.com/docs/app-server#toolrequestuserinput).

Asynchronous questions also have a **Dismiss question** button. This clears the
gateway's pending request without sending an answer or changing your selection.

Official Codex 0.155.1 maintains a separate question list inside each TUI client.
An answer sent from Telegram reaches the model and clears the gateway request,
but it does not clear an already-open TUI's question badge. Conversely, skipping
a question locally in the TUI does not notify the gateway. Use **Dismiss
question** in Telegram for such a request. Close and reopen the TUI to discard
its stale local entries without answering again. The gateway keeps the official
Codex distribution and its normal runtime updates; it does not install a custom
TUI build. `/tgquestions` is the gateway's pending list, not a mirror of the
TUI's local list.

## Menu and typing indicator

The gateway refreshes the bot command menu at startup, including after automatic
updates, and retries automatically if Telegram is temporarily unavailable.
Publish or inspect it manually when needed:

```sh
sudo /usr/local/sbin/codex-gateway-admin menu set
sudo /usr/local/sbin/codex-gateway-admin menu status
```

Typing is transient and refreshed every four seconds while work is pending or
running in the selected session, including turns started from the CLI. In
multisession mode, it follows all visible sessions. State is reconstructed after
a gateway restart. Refresh stops on completion, failure, expiry, a pending input
request, or a switch to an idle session. Disconnecting stops it in single-session
mode. Telegram can retain the last indicator for up to five seconds after
refreshing stops. API failures do not fail the underlying command.

## Session focus and multisession mode

By default, turn progress and completion messages follow the selected session.
Switching from A to B removes A's temporary messages and restores the latest
commentary and tool message for B if its turn is running. The **Connected to**
confirmation is delivered before B's commentary or tool messages, including
progress arriving while the confirmation is being retried. A subsequent
completion from A stays hidden. Final answers already displayed remain in the chat; use
`/tglastmessages` to read a session's saved responses later.

Questions and approvals are an exception: they arrive even when another session
is selected or the chat is disconnected. Each names its originating session.
Use its buttons or reply directly to its message; the answer is routed to the
exact request without changing the selected session. A reply to a question also
works while a new-session wizard is open. A delayed new/fork result cannot replace
a later selection or undo a disconnect. Explicit command responses, such as a
requested history page, return to their requesting chat.

`/tgmultisession` toggles multisession mode; `/tgmultisession on` and
`/tgmultisession off` set it explicitly. The setting is stored per user, chat and
topic and survives a restart. When enabled, user sessions can all send progress,
responses and live CLI user prompts. Accepted Telegram prompts are not echoed.
Each message starts with the full session name and a stable colored square.
Telegram offers no custom message background colors, so the square supplies the
color cue. Extremely long imported names are shortened in message headers to
leave room for the message; the session picker keeps the complete name. Internal
helper sessions remain hidden. See [Telegram formatting](https://core.telegram.org/bots/api#formatting-options).

Type `/_` for session command suggestions in either mode; `/tgmultisession`
controls which sessions' messages you receive, independently of these shortcuts.
The mode response lists the same shortcuts, such as `/_my_project`. Use a shortcut
alone to select that session, or `/_my_project message` to send there without
changing the selection. If it has exactly one unanswered Codex question, the text
answers that question; with several questions, reply to the specific question
message. Ordinary text still goes to the selected session.

After the `/_` prefix, shortcut names use lowercase ASCII letters, digits and
underscores, have unique suffixes when necessary, and remain stable across
renames. Native menu entries remain available in both modes and are refreshed
within five seconds. The manual compatibility spelling `/-my_project`
still works, but cannot autocomplete because Telegram menu commands do not permit hyphens.
Telegram permits 100 commands in a menu, so a large installation may have more
shortcuts in the mode response than fit in the menu. All listed shortcuts work.
Menus are scoped to a private chat or a group member. Telegram cannot vary a menu
by forum topic; message routing still uses the current topic.

## Temporary progress messages

Plain prompts are accepted without a “Queued for…” reply. The typing indicator
shows that work is pending or running. As Codex completes each commentary
message, Telegram silently updates a temporary message with an hourglass icon.
Each update replaces the previous commentary, so only the latest one is visible.
Tool calls appear as they start in a separate monospace message. Each subsequent
tool call replaces that message independently of commentary. This keeps at most
two progress messages visible for each turn: the latest commentary and the
latest tool call. Both messages continue to be updated after a gateway restart.
Long progress text is shortened to fit one message; final responses retain their
full text across as many messages as necessary.

After you answer a Codex question, the gateway removes the temporary commentary
and tool messages from above the question and reposts the latest progress at the
bottom of the chat, after any next question or answer acknowledgement. Questions
and your answers stay in the chat. This applies to the selected session, or each
visible session in multisession mode; answering a different session's question
does not change your selection. Only progress from still-active turns is restored.
Later updates replace the newly posted messages, with tool calls still in monospace.
If Telegram repeatedly refuses deletion, fresh progress can continue while the
gateway keeps retrying cleanup of the old copies.

After all chunks of the final response have been delivered, the gateway removes
the temporary messages for that turn. The final answer remains in the chat.
Interrupted or failed turns leave their terminal notice and remove their
progress messages. Progress received after a turn has ended is skipped.

Message IDs and deletion retries are stored in SQLite, so gateway restarts
and temporary Telegram errors do not lose cleanup work or resend the final
answer. Cleanup follows the original chat/topic and turn. Switching sessions in
single-session mode also removes the previous session's progress without waiting
for its turn to end. Telegram's Bot API permits deletion of these outgoing
messages within 48 hours of sending; a longer outage or turn can exceed that
limit. See [Telegram deleteMessage](https://core.telegram.org/bots/api#deletemessage).

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
| `/tgsessions [runtime]` | Browse sessions, 10 per page, with selection and Previous/Next buttons. |
| `/tgconnect NAME_OR_ID` | Select a session without starting a turn. |
| `/tgstatus [session]` | Show gateway connectivity, queued commands and approvals. |
| `/tghistory [count]` | Show saved Codex prompts for the selected session; defaults to 10, maximum 50 per page. |
| `/tgdisconnect` | Clear the selection. |
| `/tgnew [runtime]` | Create a session in the runtime's configured workspace. |
| `/tgsteer TEXT` | Guide the exact active turn. |
| `/tginterrupt` | Interrupt the exact active turn. |
| `/tginput APPROVAL_ID QUESTION_ID ANSWER` | Answer an input request; replying to its message is easier. |

Session lists are ordered by name. Each numbered entry shows the full session
name, with its state and workspace on separate lines. Use the matching
**Connect 1**, **Status 1**, and other numbered buttons. Long pages continue across messages
without shortening session names; controls appear after the complete page. Long
workspace paths are shortened; **Status** shows full details. Page buttons are
bound to your user, chat, topic, and runtime generation, and do not change the
selected session or start any work. Run `/tgsessions` again if a button expires or
the worker restarts.

Only user conversations inside the worker's allowed workspaces appear. Internal
helper-agent and ephemeral threads are excluded. Discovery also hides entries
that no longer exist after a complete successful scan; saved history is retained,
and this cleanup does not archive or delete threads in Codex. The CLI picker can
show a different count because it has its own source and directory filters.

## Codex controls

These run against the selected session and keep its immutable worker, runtime,
generation and thread target. Unknown slash commands produce help instead of
becoming model prompts.

| Command | Telegram behavior |
| --- | --- |
| `/status` | Session model, reasoning, workspace, recorded context and token counts, configuration defaults and account limits. Historical counters and policies are labeled as recorded values. |
| `/usage` | Account usage and rate limits reported by Codex. |
| `/model [MODEL [EFFORT]]` | List available models or update the session model. |
| `/reasoning [EFFORT]` | Inspect or change reasoning effort. |
| `/permissions [read-only\|workspace-write]` | Inspect defaults or change session sandbox settings. |
| `/approvals [POLICY]` | Inspect or change session approval policy. |
| `/fast [status\|on\|off]` | Inspect the configured tier or change the session tier when supported. |
| `/plan on\|off` | Enable or disable planning mode for subsequent turns. |
| `/personality [friendly\|pragmatic\|none]` | List styles or update the session's response style. |
| `/compact` | Request context compaction. |
| `/review [INSTRUCTIONS]` | Start a code-review turn. |
| `/init` | Ask Codex to prepare project instructions using its initialization prompt. |
| `/rename NAME` | Rename the session. |
| `/fork` | Branch the saved conversation into a new session. |
| `/new`, `/clear` | Create a new session in the selected session's workspace. |
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
| `/debug_config` | Show selected configuration diagnostics without secrets. |
| `/quit`, `/exit` | Leave the Telegram selection; the supervised runtime stays available. |
| `/help` | List Codex commands and guidance. |

Commands that require a terminal picker, desktop integration, local credentials,
or an interactive confirmation show instructions for the attached CLI. This
includes `/keymap`, `/vim`, `/raw`, `/statusline`, `/title`, `/theme`, `/pets`,
`/app`, `/ide`, `/import`, `/logout`, `/feedback`, `/experimental`, `/side`,
`/btw`, `/mention`, `/approve`, `/delete`, `/rollout`, and Windows sandbox setup.
They remain discoverable in the menu. Telegram does not emulate a terminal UI.

The implementation uses typed app-server operations; Codex slash commands are
client actions, not prompt text. See the official
[Codex command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
and [app-server protocol](https://learn.chatgpt.com/docs/app-server).

## Existing session held by another Codex process

Selecting a saved chat does not transfer ownership from another Codex app.
If its writer lock is held elsewhere, the next turn reports `session_busy`
with recovery instructions. Close that other client before retrying, use
`/fork` to branch the saved conversation, or `/tgnew` to begin fresh.
The gateway never silently redirects a failed command to a different chat.

## Saved prompt history

Use `/tghistory` after selecting a session to display prompts previously entered
in Codex. Each prompt appears as a separate bot message labelled **You · Codex**.
The bot remains the Telegram sender; these messages are copies of saved input.
Reading history never submits the prompts again, starts a turn, resumes a cold
thread, or interrupts work already running.

The newest page contains up to 10 prompts by default, shown oldest first within
that page. `/tghistory 25` requests a larger page. Use **Older prompts** to read
earlier pages. The button is restricted to the requesting user, chat, topic,
session, and runtime; select a different session or restart the runtime and use
`/tghistory` again. Repeating the command deliberately displays the newest page
again. Delivery retries preserve already-sent messages instead of repeating the
whole page.

Prompts matched to accepted Telegram submissions in the worker's command ledger
are omitted, since they are already available in Telegram. If that ledger has
been replaced or the original submission could not be correlated, a saved
Telegram prompt may also appear. Images and attachments use placeholders; local
attachment paths and file contents are not read. Long prompts are shortened with
an explicit notice, and large pages may contain fewer entries to stay within
message limits. Configured output redaction also applies to history.

This is an on-demand view of the selected session's stored user prompts, not a
capture of every terminal action. CLI-only slash commands may not be stored as
prompts, and ephemeral or unavailable Codex history cannot be reconstructed.
See the [Codex history API](https://learn.chatgpt.com/docs/app-server#read-a-stored-thread-without-resuming).

## Menu and typing indicator

Publish or inspect the menu locally:

```sh
sudo /usr/local/sbin/codex-gateway-admin menu set
sudo /usr/local/sbin/codex-gateway-admin menu status
```

Typing is transient and refreshed every four seconds while accepted work waits
for Codex. It is reconstructed from durable command/turn state after a gateway
restart. It stops on completion, failure, expiry, disconnection, or a pending
approval/input request. Telegram can retain the last indicator briefly after
refreshing stops. API failures do not fail the underlying command.

Replies retain their originating session even if selection changes. A delayed
new/fork completion cannot replace a later selection or undo a disconnect.
Command output is delivered to the requesting chat; session lifecycle events
retain the gateway's normal subscriptions.

## Temporary progress messages

Plain prompts are accepted without a “Queued for…” reply. The typing indicator
shows that work is pending or running. As Codex completes each commentary
message, Telegram displays it silently as a temporary progress message.
Tool calls appear as they start in a separate monospace message. Each subsequent
tool call replaces that message, keeping only the latest call visible while
commentary messages remain available until the turn ends. The same tool message
continues to be updated after a gateway restart.

After all chunks of the final response have been delivered, the gateway removes
the temporary messages for that turn. The final answer remains in the chat.
Interrupted or failed turns leave their terminal notice and remove their
progress messages. Progress received after a turn has ended is skipped.

Message IDs and deletion retries are stored in SQLite, so gateway restarts
and temporary Telegram errors do not lose cleanup work or resend the final
answer. Cleanup follows the original chat/topic and turn even if you change
the selected session. Telegram's Bot API permits deletion of these outgoing
messages within 48 hours of sending; a longer outage or turn can exceed that
limit. See [Telegram deleteMessage](https://core.telegram.org/bots/api#deletemessage).

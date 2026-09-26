# Codex web interface

Open `https://gateway.example.com/tgw/webui/` and sign in with the gateway's admin passkey. Select a worker/session in the sidebar. On a phone, use the menu button to open the session picker. Admin enrollment and worker management remain at `/tgw/admin/`.

The sidebar reserves its space for sessions. **Settings** at the bottom opens a dialog with draft recovery, notification controls, and **Open operations console**. Close it with its close button, Escape, or a tap outside the dialog. On phones, opening Settings closes the session drawer and keeps the keyboard closed.

The existing Go gateway serves the interface and relays its authenticated connection to the selected worker. The worker attaches to the existing Codex app-server. No additional Codex CLI, pseudo-terminal, Python service, Node.js service, CDN, or public worker port is needed. Browser JavaScript and CSS are embedded in the gateway binary.

## Sign-in and recovery

Browser logins expire eight hours after the last successful passkey sign-in.
Five minutes before expiry, **Continue with passkey** lets you renew access in
place. If access expires, the interface locks and removes private conversation
data. Signing in again restores the selected conversation and reading position;
running turns continue on the worker. On phones, returning to the foreground
checks authentication again instead of relying on background timers. Other open
tabs recheck their login after renewal or sign-out.

**Recover encrypted text drafts after sign-in**, available in **Settings**, is optional and off by default.
When enabled, the browser encrypts per-session text drafts with AES-GCM before
sending them to the gateway. The gateway stores only ciphertext, with a maximum
of 256 KiB per recovery copy and a 30-minute expiry after its latest save. The
encryption key stays in this tab's session storage; no plaintext draft or
conversation is written to browser storage. Recovery requires signing in as the
same account and retaining this tab's key. Closing the tab can lose the key.
Image attachments are excluded and remain in memory only.

Recovered copies are cleared after recovery; disabling recovery or explicitly
signing out clears the associated recovery state. Expired copies are removed
periodically. Failed or uncertain saves show a notice, and recovery does not
guarantee the last keystroke was saved before expiry or a network interruption.
Restoring a draft never sends it. If a previous send's acknowledgment was lost,
check the conversation before submitting again.

## Conversation controls

- An accepted **Steer** message shows **Message queued** above the input for four seconds. This temporary confirmation also applies to **/tgsteer** and disappears when the turn ends, the session changes or the viewer disconnects. It is not added to conversation history. Failed or unconfirmed sends do not display this confirmation.
- Connection and history-loading errors disappear after the session successfully reloads. A successful retry also clears an older-history loading error. Codex errors marked as retryable clear when output resumes for that same turn or it completes successfully. Merely opening a socket does not clear an unresolved error; other Codex errors and uncertain-send warnings remain visible.
- On phones, the status bar uses two lines: model/reasoning with turn state, then context percentage with account limits. The numeric token count is omitted. Long model names are shortened to keep the rows compact.
- Selecting a session loads its latest 20 conversation entries in chronological order. Scroll to the top to retrieve 20 earlier entries without losing your reading position, or use **Load 20 earlier messages**. Tool calls and reasoning summaries count as entries; a long turn does not cause the browser to load its entire history.
- Opening the session menu on a phone closes an existing keyboard and leaves search unfocused. Tap the search field when you want to type.
- Every sidebar session has a delete button on desktop and mobile. Confirmation names the conversation and explains that its child conversations are also removed, while its working directory and all files are kept. Deletion uses the same worker operation and idle checks as **/tgdeletesession**, including checks for running child sessions. Deleting another session keeps your current session selected. A row remains until deletion is confirmed; a lost acknowledgment triggers read-only status checks, never an automatic repeat of the deletion request.
- Send a prompt with **Send**, or desktop Enter. Shift+Enter inserts a newline. Touch keyboards use Enter for a newline; use the send button to submit. During a running turn, **Steer** submits follow-up input to that turn and **Stop** interrupts it.
- Paste an image into the prompt, drop one onto the composer, or tap **＋** to choose an image on desktop or mobile. A thumbnail appears before sending; **×** removes it. You can send the image by itself or with text. Each message supports one PNG, JPEG or GIF up to 10 MiB, 16 million pixels and 8,192 pixels per side. If a mobile clipboard does not expose image files to the browser, use **＋** and the photo/file picker. Image drafts stay with their session in browser memory and are cleared only after acknowledgment; they are never resent automatically. Reloading or signing out clears them. Saved conversation and Telegram history show an **[Image]** placeholder.
- Prompts sent through Telegram appear live in an open web UI for the same session when Codex accepts them; prompts queued behind a running turn appear when that queued turn starts. Prompts sent through the web UI or Codex CLI appear in Telegram as a bot message labeled **You · Codex**, following the selected session or multisession mode. Accepted Telegram prompts are not echoed back, and replayed native events do not duplicate prompts. The worker subscribes before enabling a newly opened web session, including sessions restored from disk.
- Model and reasoning names in the status bar are plain text. Use **/model** to choose a model and then its reasoning effort, or **/reasoning** to change effort. These settings apply to subsequent turns; changes require an idle session. Confirmed model and effort changes from a browser, Telegram or attached native client update other open web views of the same session through WebSocket notifications. Historical usage data and delayed command acknowledgments cannot overwrite the confirmed selection. Missing runtime model metadata leaves the session defaults available.
- Worker headings have a tinted background and accent border, with runtime names underneath. On desktop, the **− / +** controls in the header set the conversation and prompt font to 12–22 pixels and scale the status text with it. This display preference is saved in the browser; the phone layout keeps its compact text size.
- A session name turns green while its turn runs, with a pulsing green dot beside a steady **Working** label, and returns to its normal color when the turn ends. Only the dot pulses, using the same animation as the purple working dot in the conversation. Switching to another session keeps the running session green; it continues updating in the background. Pending questions or approvals take priority: the name glows yellow until they are resolved. On mobile, any pending session question also changes the menu button to a glowing yellow **?**. These indicators update through a separate WebSocket, without polling conversation history. **Live updates** below the session count confirms that the feed is connected; reconnection is shown explicitly rather than silently leaving stale indicators.
- On a wide desktop, one full-width status row combines model/effort controls, turn state, context usage, Codex account allowances (for example **5h 74% left · Weekly 58% left**), and working directory. It wraps when larger fonts or a narrower window need more room; phones keep model controls on their own row. Hover over an allowance for its reset date and local time. Like the Codex statusline, this uses the default Codex limit bucket; labels follow the reported window duration. Limits load when connecting and update from runtime events without periodic quota polling. Older workers or accounts that do not report quotas show **Limits unavailable**; disconnecting clears the values.
- Tool calls are collapsible, with blue labels and output and gray command text such as **Ran …**; failed tools retain an error color. File changes show green additions and red deletions. Assistant Markdown supports headings, lists, emphasis, links, quotes, tables, fenced code, and basic code coloring. Tables become labeled records on narrow screens; long code and paths wrap.
- Completed reasoning is labeled **Reasoning complete**. An expandable line appears only when Codex provides a nonempty public summary; otherwise only the label remains. Private reasoning content is not forwarded to the browser.
- Subagent activity markers show the reported action and agent path, such as **Started /root/review** or **Completed /root/review**. These markers carry lifecycle metadata, not the child agent's transcript, so they are displayed without an empty expandable tool block.
- Questions, command/file approvals, and additional-permission requests appear above the composer, including requests older than the latest 20 history entries. The worker restores current pending requests separately from history and forwards replies through the native connection that owns each request. Asynchronous questions use Codex's quoted-title answer format and remain available after a turn ends. Later explicit answers or ordinary prompts update that pending list; loading older history does not resurrect old questions. Permission grants from this interface are scoped to the current turn. Unsupported request types ask you to respond in Codex or Telegram.
- Type `/` in the prompt or open **/ Commands** in the header for searchable command suggestions. Use ↑/↓ and Enter to choose, Tab to complete, or Escape to close. Touch users can tap a command. The header menu also works before selecting a session, including `/tgupdateworkers`.
- Commands with choices use a terminal-style menu with a highlighted row and a marker for the current setting. Use ↑/↓, Home/End and Enter, press 1–9 to pick a numbered row, or tap a row on mobile. Escape goes back from a submenu or closes the menu. Session commands such as **/resume** and **/tgsessions** use the same menu with full names and worker/workspace details. Confirmation menus initially focus Cancel.
- Successful setting selections close the command panel after their final confirmation, without focusing the prompt or reopening the mobile keyboard. Intermediate menus, errors and requested information such as **/status** remain visible.

The relay preserves the gateway's complete-message secret-redaction boundary: it suppresses raw content deltas that could expose a secret split across chunks. Turn status, tool starts/completions, completed commentary, and questions arrive live. Assistant text appears when its item is complete rather than token by token. This is intentional; the interface must not bypass protections applied to Telegram traffic.

## Command menu

The catalog recognizes the user-facing slash-command names and aliases in official Codex **0.156.0**, plus gateway commands. It distinguishes runnable worker operations, browser controls, and features that require the native client. Unknown commands and unavailable commands are never submitted as model prompts. A command requiring an idle session is checked again on the worker even when its browser menu is already open.

| Commands | Browser behavior |
| --- | --- |
| `/model`, `/reasoning` | Model and reasoning buttons, with changes applied through the worker to the selected session. |
| `/permissions`, `/approvals` | Permission presets and approval-policy choices. Full Access retains the explicit second confirmation. |
| `/status`, `/usage`, `/pwd` (`/cwd`), `/debug-config` | Read session configuration, context, account limits, working directory or effective settings. |
| `/new`, `/clear`, `/resume`, `/rename`, `/fork`, `/archive`, `/delete` | Create, browse, rename, fork, archive or delete sessions. New sessions use a named, existing allowed directory. Archive and delete ask for confirmation; deletion preserves working-directory files. |
| `/review`, `/compact`, `/init` | Start a review, compact history or generate project instructions. |
| `/plan`, `/fast`, `/personality`, `/memories`, `/goal` | Configure planning, service tier, communication style, session memory and goals using forms/buttons or inline arguments. |
| `/mcp`, `/apps`, `/skills`, `/plugins`, `/hooks`, `/ps`, `/stop` (`/clean`) | Inspect integrations or background terminals; stop the selected session’s background terminals. Supported inline operations follow the worker’s existing command handlers. |
| `/copy`, `/export`, `/raw`, `/mention` | Copy the latest loaded Codex response, download loaded history as Markdown, toggle plain Markdown rendering or insert a file reference. Export explicitly includes only loaded history; scroll to the top or use **Load 20 earlier messages** first to include more. |
| `/theme`, `/title`, `/statusline`, `/tui` | Adjust this browser’s colors, tab title or status details, or show its keyboard/display controls. These do not change native terminal preferences. |
| `/agents` (`/agent`), `/quit` (`/exit`) | Browse primary sessions or detach the browser. Running work continues. |
| `/tginstances`, `/tgstatus`, `/tgupdateworkers` | Inspect the gateway and workers or queue worker and Codex runtime update checks for every enabled worker, including offline workers. No selected session is required. The update panel shows results automatically; `/tgstatus` shows them later. |
| `/tgsessions`, `/tgdisconnect`, `/tgdeletesession` | Browse sessions, detach, or confirm deletion of the selected session. These act on the browser selection, independently of Telegram. |
| `/tghistory [count]`, `/tglastmessages [count]` | Show recent user/Codex messages in original order with timestamps; defaults are two and one respectively, maximum 50. Turn timestamps are labelled when exact message timestamps are unavailable. |
| `/tgsteer TEXT`, `/tginterrupt`, `/tgquestions`, `/tginput` | Guide or interrupt the selected turn; show and answer its currently visible questions/approvals. The browser request list is scoped to the selected session. |
| `/help`, `/tghelp` (`/tgstart`) | Open all commands or gateway command suggestions. |

Some native features cannot be reproduced with the current browser/runtime bridge. These remain visible with an availability explanation: native IDE/desktop integration, voice transport, configuration import, managed worktree creation, native recap generation, temporary side conversations, child-agent switching, automatic-review retries, worker-account logout, diagnostics/feedback, terminal keymaps/Vim/pets, Windows sandbox setup and experimental configuration. `/cd` also remains a terminal operation: official Codex 0.156 reports live and saved working directories differently until another turn, so the browser does not change this security-sensitive setting. The menu does not claim to perform these actions. `/tgmultisession` belongs to a Telegram chat; use separate browser tabs for multiple web sessions. Internal debugging commands are recognized but hidden from normal suggestions.

Worker commands use a typed, session-bound `gateway/command` bridge and reuse the worker’s existing command handlers. Requests are serialized with native session events and fenced against updater maintenance. Browser command results do not become Telegram command replies or alter Telegram’s selected session. New native turns and accepted user input keep their existing cross-client event behavior. Runtime commands depend on the selected worker/runtime supporting the operation; older workers need updating.

Gateway maintenance commands use the passkey-protected API with exact Origin and CSRF validation. Repeated worker-update requests share a pending request; browser requests create no Telegram notification subscriptions. `/tgupdateworkers` checks both worker releases and the latest stable Codex runtime, reporting installed, running, and available versions. Updates retain the normal idle checks. Runtime updates restart app servers and their worker supervisor; when both versions are current, no restart is needed. Explicit runtime checks do not change the daily background schedule. Runtime reports require the v0.5.61 updater; repeat the command after upgrading an older updater.

The update panel follows each queued request through read-only status requests, including while a worker restarts. Observation pauses in hidden tabs and resumes when they become visible. Closing the panel, choosing another command, changing sessions, or explicitly disconnecting stops observation; queued updates continue. Lost command acknowledgments are never replayed automatically; use `/tgstatus` before retrying. Gateway commands can also be submitted before selecting a session.

## Mobile notifications

Turn-completion notifications cover all visible user sessions, including sessions on other workers. They use Web Push and can arrive while the installed web app is closed. Enable notifications separately on each device from **Settings** at the bottom of the session sidebar.

On iPhone or iPad with iOS/iPadOS 16.4 or newer:

1. Open the web UI in Safari, choose **Share → Add to Home Screen**, then open the new icon.
2. Sign in with your gateway passkey.
3. Open **Settings**, tap **Enable notifications**, and allow the system permission request.

Apple requires a Home Screen web app and a direct button press before requesting notification permission. Supported Android and desktop browsers use the same notification control. See [WebKit's Web Push requirements](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/).

Alerts use generic text without prompts, answers, session titles or filesystem paths. Tapping one opens its session; normal passkey authentication still applies. Notifications are optional, and the browser or operating system can delay or silence them through its notification and Focus settings. Use **Disable notifications** to stop alerts on this device. Explicitly signing out disables its subscription; revoking a passkey disables subscriptions registered with that passkey. Normal login-cookie expiry does not stop background alerts.

The existing gateway sends encrypted pushes through the browser's push provider. Its SQLite database retains subscriptions, signing keys and a bounded delivery queue, so there is no additional daemon or manually configured push-provider account. The service worker does not cache conversation data or authenticated API responses. The application stores only an opaque device subscription ID in local storage; the browser manages its own push subscription.

## Connection and privacy behavior

The selected conversation uses `/tgw/api/v1/webui/connect`; session-list notifications use `/tgw/api/v1/webui/activity?v=2`. The activity connection belongs to the signed-in browser, independently of which conversation is selected or disconnected. Reverse proxies must forward WebSocket Upgrade headers for **both** endpoints, as well as the worker connection endpoint. The supplied NGINX template and installer configure all three.

The versioned activity protocol starts with an `activity_snapshot`, then sends ordered `activity_event` notifications for `turn_started`, `turn_ended`, `question_requested`, `question_resolved`, `session_changed`, `session_removed` and `session_settings_changed`. Each event includes the affected session ID and its complete indicator state. It contains no prompt, answer or transcript text. Sequence numbers detect missing notifications; reconnecting obtains a fresh snapshot. A bounded notification buffer preserves rapid transitions and falls back to a snapshot if a slow viewer falls behind. A heartbeat arrives every 15 seconds; after 45 seconds without a valid frame, the browser reconnects automatically. Initial connection attempts have a 20-second deadline. No conversation history is fetched for these updates.

Version 2 adds confirmed `model`, `reasoning_effort` and `settings_revision` fields. The worker records native `thread/settings/updated` notifications with a monotonic revision per runtime generation; a small gateway SQLite table keeps these preferences separate from historical usage statistics. The revision is `generation:revision`, or empty when current settings are unconfirmed. An empty effort with a confirmed revision selects the model default. The browser applies fresh revisions independently of its conversation connection and fences late resume, inventory and command responses. Selected conversations can also consume native settings notifications from older workers until the versioned feed has confirmed settings; updating workers enables the durable all-session feed.

The activity feed is authoritative for sidebar colors, including the selected session. The mobile menu is green while at least one visible session has an active turn, even if another session is selected or a search hides the running row. It returns to its normal color after all turns finish. A pending question takes priority with the yellow **?** indicator. A delayed event from an old conversation connection cannot overwrite a newer global status. An inventory change arriving during a session-list fetch schedules another fetch, so new sessions are not missed. Older open browser pages can keep using version 1 or the unversioned snapshot protocol until refreshed; version 1 receives settings changes as ordinary session changes.

Disconnecting or switching sessions detaches only the browser viewer. Existing work continues on the worker. Session switching clears the previous conversation and its pending controls before attaching to the next session. A network interruption retries with bounded exponential backoff, then reloads the latest persisted history and live requests. Resume uses `excludeTurns` and paginated history, avoiding an unbounded conversation payload.

Connection progress separates the live transport from session restoration and history loading. The latest messages are displayed as soon as their page arrives; submitting input waits for the required session and active-turn state. Loading questions, model choices and account limits is tracked separately. Switching sessions fences late responses so an old connection cannot replace the new conversation or its controls.

Worker observer admission is cancellation-aware, including update admission and session-actor snapshot waits. An acknowledged subscription can be reused on the same runtime connection; a discovery reservation alone is not treated as ready. The observer is established before accepting browser input, preserving Telegram delivery of the first prompt. Slow legacy history reads run outside the relay's native event reader. These changes bound avoidable waits; they do not guarantee a fixed load time for an unloaded or very large Codex conversation.

The worker prefers native item pagination. Official Codex 0.156 can reject that method for older conversation formats, so the worker falls back to reading individual source turns and returns only the requested 20 entries. Completed turns are cached in bounded worker memory for up to ten minutes; this cache does not change the existing SQLite synchronization backend or add an idle polling loop. A cold legacy read can still require one complete source turn from Codex. These reads use a temporary connection to the existing app-server so an oversized response cannot close the worker's durable observer connection. Cache limits are 64 MiB and 32 snapshots, with bounded message/page sizes and an explicit notice for oversized entries. If a paging cursor expires, **Reload recent messages** returns to the latest 20 entries while preserving the unsent draft.

By default, drafts remain in memory per session while the page stays open, and
reloading or closing it loses an unsent draft. Optional encrypted text recovery is
described above. There is no persistent browser transcript or plaintext draft
cache. If a prompt's acknowledgment is lost, its draft stays visible with an
explicit warning; **the browser never automatically resends it**. Authentication
expiry disconnects the viewer and clears loaded conversation data and plaintext
drafts; successful sign-in can recover an opted-in encrypted text copy.

The interface follows new output only while you are near the bottom. Selecting a session scrolls fully to its latest entry and keeps it visible while status text, questions, font metrics and image previews settle above the composer. Reading earlier messages keeps your scroll position; **Latest messages** returns to the end. Mobile layouts account for the visible keyboard viewport. Reduced-motion preferences disable working and question animations while keeping their status colors. The official runtime's existing limitation with clearing TUI question displays after answers from another client still applies; this interface does not patch the Codex runtime.

Embedded stylesheet and script URLs include a content fingerprint so a page reload after deployment loads a matching set of assets. An already open tab needs a browser refresh to use the new interface.

The web UI uses the admin authentication boundary and therefore grants the same trusted operator access. HTTPS, secure session cookies, exact WebSocket Origin checks, selected-session authorization, and the worker's existing execution permissions apply. Message rendering builds DOM text nodes rather than accepting HTML. Remote image embeds are not fetched. Only HTTP, HTTPS, and mail links become clickable; local paths remain text. Third-party assets are not loaded.

## Upstream formatting reference and attribution

The command catalog was also checked against [official Codex 0.156.0 slash commands](https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/tui/src/slash_command.rs).

This is an independent JavaScript/DOM implementation informed by the actual OpenAI Codex TUI source, reviewed at immutable commit [`5121aeff13aa6615081f9467c8051289201893fc`](https://github.com/openai/codex/tree/5121aeff13aa6615081f9467c8051289201893fc). It is not a Rust-to-JavaScript port or an official OpenAI web client.

| Upstream source | Browser adaptation |
| --- | --- |
| [`codex-rs/tui/styles.md`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/styles.md) | Default text, readable secondary text, magenta Codex identity, accent controls/links, green success/additions, red failures/deletions; bold keyboard hints. Browser light/dark colors are chosen for readable contrast rather than literal terminal palette indexes. |
| [`src/markdown_render.rs`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/src/markdown_render.rs) | Visible `#` heading markers, heading emphasis, accent code/links, green quotes, table structure and narrow-layout key/value fallback. Native browser flow replaces terminal character-width wrapping. |
| [`src/history_cell/messages.rs`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/src/history_cell/messages.rs) | Separate user/assistant/activity cells, user prompt accent, expandable reasoning summaries, removal of terminal control sequences from visible text. |
| [`src/diff_render.rs`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/src/diff_render.rs) | Per-line add/delete markers, readable foregrounds on subdued addition/deletion backgrounds, and distinct diff metadata. |
| [`src/style.rs`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/src/style.rs) | Semantic accent, status, and muted text roles rather than hard-coded terminal escape sequences. |
| [`src/chatwidget/rate_limits.rs`](https://github.com/openai/codex/blob/5121aeff13aa6615081f9467c8051289201893fc/codex-rs/tui/src/chatwidget/rate_limits.rs) | Remaining percentage and window-duration labels for the default Codex account limit bucket; sparse limit updates preserve the other known window. |

OpenAI Codex is copyright 2025 OpenAI, licensed under Apache License 2.0. Its upstream notice also identifies Ratatui-derived code and the Ratatui copyright holders. Copies of the upstream [LICENSE](../third_party/codex/LICENSE) and [NOTICE](../third_party/codex/NOTICE) are included for attribution. The browser renderer is new code adapted to browser accessibility, responsive layout, and the gateway's security requirements; no native terminal renderer is bundled.

## Browser regression checks

Install the pinned development browser tooling and run all browser checks:

```sh
npm ci
npx playwright install chromium
npm run test:browser
```

With an existing Playwright installation, `PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-webui.cjs` runs just the Web UI checks. See [website maintenance](website.md) for reproducible public desktop/mobile screenshots using fictional demo data.

Set `WEBUI_SCREENSHOTS=/tmp/webui-screenshots` to save desktop and mobile screenshots. These development tools are optional; they are not dependencies of an installed gateway. The browser checks use a simulated app-server transport and verify safe rendering, mobile wrapping, pagination, model controls, questions/approvals, session switching, disconnection/reconnection, draft preservation, no automatic prompt replay, and authentication expiry. Go integration tests exercise the authenticated gateway/worker relay separately.

The mobile shell uses the visual viewport's document coordinates (`pageTop`/`pageLeft`) with absolute positioning. Safari can pan the layout viewport too, so a fixed shell using only `offsetTop` is insufficient; WebKit's [viewport implementation](https://github.com/WebKit/WebKit/blob/main/Source/WebCore/page/VisualViewport.cpp) distinguishes those coordinates. Focus and keyboard transitions receive a brief, bounded geometry recheck because [WebKit can report stale offsets during an event](https://bugs.webkit.org/show_bug.cgi?id=237851). There is no continuous viewport polling while idle, and keyboard panning does not repeatedly resize the focused textarea. The browser checks simulate keyboard resizing, panning, document scrolling, and delayed metrics; they do not emulate an iPhone's native keyboard or Safari compositor. Verify keyboard opening, dismissal, question entry, and session search on an actual iPhone when changing this behavior.

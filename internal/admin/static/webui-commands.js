'use strict';
(() => {
  // User-facing command names checked against the official Codex 0.156.0 TUI:
  // https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/tui/src/slash_command.rs
  // These are browser actions or explicit worker operations, never model prompts.
  // Terminal-only actions stay discoverable with an explanation of their scope.
  const command = (name, description, kind, args = '', options = {}) => Object.freeze({
    name, description, kind, args, busy: true, aliases: [], source: 'codex', ...options,
    aliases: Object.freeze(options.aliases || [])
  });
  const unavailable = (name, description, reason, args = '', options = {}) =>
    command(name, description, 'unavailable', args, { reason, ...options });
  const catalog = Object.freeze([
    command('model', 'Choose the model and reasoning effort', 'worker', '[model] [effort]'),
    command('permissions', 'Choose the permissions for this session', 'worker', '[read-only|workspace-write|full-access]', { busy: false }),
    command('status', 'Inspect session settings, context and account limits', 'worker'),
    command('usage', 'View account usage and rate-limit resets', 'worker'),
    command('review', 'Review changes, a branch or a commit', 'worker', '[instructions]', { busy: false }),
    command('plan', 'View or change planning mode', 'worker', '[on|off]', { busy: false }),
    command('goal', 'View, set, pause or clear the current goal', 'worker', '[objective|pause|resume|clear]'),
    command('rename', 'Give the selected session a new name', 'worker', '<name>'),
    command('new', 'Create a session and choose its working directory', 'local', '[name]', { busy: false }),
    command('clear', 'Start a fresh session', 'local', '[name]', { busy: false }),
    command('resume', 'Find and connect to a saved session', 'local', '[name]'),
    command('archive', 'Archive the selected conversation', 'worker', '', { busy: false }),
    command('delete', 'Delete a conversation while keeping its files', 'local', '', { busy: false }),
    command('fork', 'Create a new session from this conversation', 'worker', '', { busy: false }),
    command('init', 'Create project instructions in AGENTS.md', 'worker', '', { busy: false }),
    command('compact', 'Compact the conversation context', 'worker', '', { busy: false }),
    command('agents', 'Choose a session from the sidebar', 'local', '[name]', { aliases: ['agent'] }),
    command('copy', 'Copy the latest Codex response as Markdown', 'local'),
    command('export', 'Download the conversation as Markdown', 'local'),
    command('raw', 'Switch between rendered Markdown and plain text', 'local', '[on|off]'),
    command('diff', 'Show changes in the working directory', 'worker'),
    command('mention', 'Add a file reference to your prompt', 'local', '<path>'),
    command('pwd', 'Show this session’s working directory', 'worker', '', { aliases: ['cwd'] }),
    unavailable('cd', 'Change this session’s working directory', 'Change directory in the Codex terminal, then reconnect here after the next turn. The current runtime reports live and saved directories differently; this browser requires a verified workspace.', '[path]'),
    command('debug-config', 'Inspect effective settings and their sources', 'worker', '', { aliases: ['debug_config'] }),
    command('title', 'Set or restore the browser tab title', 'local', '[title|reset]'),
    command('statusline', 'Show or hide the browser status line', 'local', '[on|off]'),
    command('theme', 'Choose the browser color theme', 'local', '[dark|light|system]'),
    command('tui', 'Show browser display controls and keyboard shortcuts', 'local'),
    command('skills', 'List the skills available to this workspace', 'worker'),
    command('hooks', 'Inspect configured lifecycle hooks', 'worker'),
    command('memories', 'View or change session memory mode', 'worker', '[enabled|disabled]', { busy: false }),
    command('mcp', 'Inspect configured MCP servers and tools', 'worker', '[verbose]'),
    command('apps', 'List available connected apps', 'worker'),
    command('plugins', 'List plugins available to this workspace', 'worker'),
    command('ps', 'List this session’s background terminals', 'worker'),
    command('stop', 'Stop this session’s background terminals', 'worker', '', { aliases: ['clean'] }),
    command('quit', 'Disconnect this browser from the session', 'local', '', { aliases: ['exit'] }),
    command('help', 'Show available commands and keyboard shortcuts', 'local', '', { source: 'extension' }),
    command('reasoning', 'View or change the reasoning effort', 'local', '[effort]', { source: 'extension' }),
    command('approvals', 'View or change the approval policy', 'worker', '[untrusted|on-request|never]', { busy: false, source: 'extension' }),
    command('fast', 'View or change the service tier', 'worker', '[status|on|off]', { busy: false, source: 'extension' }),
    command('personality', 'View or change the response style', 'worker', '[friendly|pragmatic|none]', { busy: false, source: 'extension' }),
    unavailable('recap', 'Summarize the current conversation', 'This Codex runtime command is not yet available through the worker command bridge.'),
    unavailable('worktree', 'Continue in a new Git worktree', 'Worktree creation requires the Codex CLI workspace flow. Run /worktree in a terminal attached to this session.'),
    unavailable('subagents', 'Switch to a child agent thread', 'The web session list contains primary sessions. Use /subagents in the Codex CLI to inspect child agent threads.'),
    unavailable('side', 'Start a temporary side conversation', 'Ephemeral side conversations require the Codex CLI. Use /fork for a persistent branch in the browser.', '[question]', { aliases: ['btw'] }),
    unavailable('approve', 'Retry an automatic review denial', 'This action requires the original automatic review denial in the Codex CLI. Ordinary pending approvals can be answered with /tgquestions.'),
    unavailable('ide', 'Include context from an attached editor', 'IDE context is supplied by the Codex editor integration. Use /mention to add a file reference in this browser.', '[on|off]'),
    unavailable('import', 'Import configuration and conversations', 'Import changes the worker’s local Codex setup. Run /import in the Codex CLI on that worker.'),
    unavailable('app', 'Continue in the Codex desktop application', 'This opens an application on the worker’s desktop. Open the session from your local Codex desktop app instead.'),
    unavailable('voice', 'Use Codex voice input', 'Native Codex voice transport is not available in this browser. Your device’s keyboard dictation can enter prompt text.', '[settings]'),
    unavailable('daemon', 'Manage the local Codex background server', 'The worker manages its app-server. Use /tgstatus to inspect it or /tgupdateworkers to request a safe update.'),
    unavailable('warnings', 'Show retained Codex diagnostics', 'The runtime’s local warning buffer is not exposed through the worker. Connection and request failures appear in the browser notice.'),
    unavailable('experimental', 'Manage experimental Codex features', 'Experimental feature settings belong to the worker’s Codex configuration. Change them using its local CLI.'),
    unavailable('keymap', 'Configure terminal keyboard shortcuts', 'The native terminal keymap does not control this browser. Use /tui to view the web keyboard shortcuts.', '[shortcut]'),
    unavailable('vim', 'Enable the terminal’s Vim composer', 'Vim input mode belongs to the native Codex terminal composer.'),
    unavailable('pets', 'Configure the terminal pet', 'The native Codex terminal pet is not rendered in the web interface.', '[name|off]', { aliases: ['pet'] }),
    unavailable('setup-default-sandbox', 'Set up the Windows agent sandbox', 'This command configures the local Windows sandbox and must run in the Codex CLI on that worker.', '', { aliases: ['setup_default_sandbox'] }),
    unavailable('logout', 'Sign out of the worker’s Codex account', 'The worker account can serve other sessions. Sign out from the worker’s local Codex CLI; /quit only disconnects this web viewer.'),
    unavailable('feedback', 'Send Codex diagnostic feedback', 'Native feedback requires the local Codex logs and consent flow. Use /feedback in the Codex CLI.'),
    // Internal debug commands are recognized so they cannot become model prompts,
    // but omitted from suggestions, as in a production Codex build.
    unavailable('rollout', 'Inspect the local transcript path', 'Transcript paths remain local to the worker. Use /export to download the browser conversation.', '', { hidden: true }),
    unavailable('test-approval', 'Internal approval diagnostic', 'This is an internal Codex debug command.', '', { hidden: true }),
    unavailable('debug-m-drop', 'Internal memory diagnostic', 'This is an internal Codex debug command.', '', { hidden: true }),
    unavailable('debug-m-update', 'Internal memory diagnostic', 'This is an internal Codex debug command.', '', { hidden: true }),
    command('tghelp', 'Show gateway commands for the web interface', 'local', '', { source: 'gateway', aliases: ['tgstart'] }),
    command('tgsessions', 'Find and connect to a session', 'local', '[name]', { source: 'gateway' }),
    command('tginstances', 'List workers and their runtimes', 'gateway', '', { source: 'gateway' }),
    command('tgstatus', 'Inspect gateway connections and worker state', 'gateway', '', { source: 'gateway' }),
    command('tgupdateworkers', 'Queue worker updates after active turns finish', 'gateway', '', { source: 'gateway' }),
    command('tgdisconnect', 'Disconnect this web viewer', 'local', '', { source: 'gateway' }),
    command('tghistory', 'Show recent messages in conversation order', 'local', '[count]', { source: 'gateway' }),
    command('tglastmessages', 'Show the last message or last N messages', 'local', '[count]', { source: 'gateway' }),
    command('tgsteer', 'Send guidance to the running turn', 'local', '<message>', { source: 'gateway' }),
    command('tginterrupt', 'Interrupt the running turn', 'local', '', { source: 'gateway' }),
    command('tgquestions', 'Show pending questions and approvals', 'local', '', { source: 'gateway' }),
    command('tginput', 'Answer a pending question or approval', 'local', '', { source: 'gateway' }),
    command('tgdeletesession', 'Choose a conversation to delete, keeping its files', 'local', '', { busy: false, source: 'gateway' }),
    unavailable('tgmultisession', 'Toggle Telegram messages from all sessions', 'This option controls the Telegram chat. The browser displays one selected session; use separate tabs for multiple sessions.', '[on|off]', { source: 'gateway' })
  ]);
  const lookup = new Map();
  for (const entry of catalog) {
    lookup.set(entry.name, entry);
    for (const alias of entry.aliases) lookup.set(alias, entry);
  }
  function find(name) {
    return lookup.get(String(name || '').trim().replace(/^\//, '').toLowerCase()) || null;
  }
  function parse(text) {
    const value = String(text || '').trimStart();
    if (!value.startsWith('/')) return null;
    const match = /^\/([^\s]*)(?:\s+([\s\S]*))?$/.exec(value);
    if (!match) return { name: '', args: '', command: null };
    const name = match[1].toLowerCase();
    return { name, args: (match[2] || '').trim(), command: find(name) };
  }
  function search(query = '') {
    const value = String(query).trim().replace(/^\//, '').toLowerCase();
    const matches = [];
    for (let index = 0; index < catalog.length; index++) {
      const entry = catalog[index];
      if (entry.hidden) continue;
      const names = [entry.name, ...entry.aliases];
      const score = !value || names.some(name => name.startsWith(value)) ? 0
        : names.some(name => name.includes(value)) ? 1
        : entry.description.toLowerCase().includes(value) ? 2 : -1;
      if (score >= 0) matches.push({ entry, score, index });
    }
    return matches.sort((a, b) => a.score - b.score || a.index - b.index).map(match => match.entry);
  }
  window.CodexWebCommands = Object.freeze({ catalog, find, parse, search });
})();

#!/usr/bin/env node
// Optional development regression checks, with no production Node dependency.
// PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-webui.cjs
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const hostile = '<img src=x onerror="window.injected=true">';
const longPath = '/projects/' + 'long-directory-'.repeat(24);
const sessions = [
  { session_id: 'a', codex_thread_id: 'thread-a', worker_id: 'w', worker_name: 'Linux worker', runtime_name: 'Primary Codex', name: 'Gateway workspace', cwd: longPath, state: 'idle', stats: { model: 'codex-model', reasoning_effort: 'high', context_tokens: 17000, context_window: 200000 } },
  { session_id: 'b', codex_thread_id: 'thread-b', worker_id: 'w', worker_name: 'Linux worker', runtime_name: 'Primary Codex', name: 'Second session ' + hostile, cwd: '/projects/mobile', state: 'running' },
  { session_id: 'c', codex_thread_id: 'thread-c', worker_id: 'w', worker_name: 'Linux worker', runtime_name: 'Primary Codex', name: 'Large conversation', cwd: '/projects/history', state: 'idle' },
  { session_id: 'd', codex_thread_id: 'thread-d', worker_id: 'w', worker_name: 'Linux worker', runtime_name: 'Primary Codex', name: 'Short visible page', cwd: '/projects/short-history', state: 'idle' },
];
const now = Math.floor(Date.now() / 1000);
const rateLimits = {
  limitId: 'codex',
  primary: { usedPercent: 26, windowDurationMins: 300, resetsAt: now + 3600 },
  secondary: { usedPercent: 42, windowDurationMins: 10080, resetsAt: now + 3 * 86400 },
};
const turns = {
  'thread-a': [
    { id: 'new-a', status: 'completed', startedAt: now - 100, items: [
      { id: 'user-a', type: 'userMessage', content: [{ type: 'text', text: 'Build a responsive workspace.' }] },
      { id: 'dismissed-async-a', type: 'agentMessage', phase: 'commentary', delivery: 'async', text: 'This historical question was dismissed.', questions: [{ title: 'Dismissed Telegram question?' }] },
      { id: 'reason-a', type: 'reasoning', content: [], summary: ['Checking keyboard behavior and reconnects.'] },
      { id: 'reason-empty-a', type: 'reasoning', summary: [], content: [{ text: 'UNEXPOSED_RAW_REASONING' }], text: 'UNEXPOSED_REASONING_TEXT' },
      { id: 'reason-blank-a', type: 'reasoning', summary: [' \n\u001b[31m\u001b[0m\t'] },
      { id: 'agent-started-a', type: 'subAgentActivity', kind: 'started', agentThreadId: 'child-thread', agentPath: '/root/reviewer ' + hostile },
      { id: 'agent-interacted-a', type: 'subAgentActivity', kind: 'interacted', agentThreadId: 'child-thread', agentPath: '/root/reviewer' },
      { id: 'agent-interrupted-a', type: 'subAgentActivity', kind: 'interrupted', agentThreadId: 'child-thread', agentPath: '/root/reviewer' },
      { id: 'agent-completed-a', type: 'subAgentActivity', kind: 'completed', agentThreadId: 'child-thread', agentPath: '/root/reviewer' },
      { id: 'agent-fallback-a', type: 'subAgentActivity', kind: 'completed', agentThreadId: 'child-without-path' },
      { id: 'agent-future-a', type: 'subAgentActivity', kind: 'future-action', agentThreadId: 'child-thread', agentPath: '/root/future-agent', output: 'UNEXPOSED_AGENT_OUTPUT' },
      { id: 'tool-a', type: 'commandExecution', command: 'go test ./...', status: 'completed', exitCode: 0, aggregatedOutput: 'All checks passed.' },
      { id: 'reply-a', type: 'agentMessage', text: '# Ready to continue\n\nThe **responsive layout** is ready.\n\n| Control | Behavior |\n| --- | --- |\n| Send | Starts a turn |\n| Disconnect | Work continues |\n\n```go\nconst ready = true\n```\n\n```diff\n-old UI\n+responsive UI\n```\n\n[Unsafe](javascript:alert(1)) ' + hostile + '\n\n[Documentation](https://example.com/docs)' },
    ] },
    { id: 'old-a', status: 'completed', startedAt: now - 3600, items: [{ id: 'old-user', type: 'userMessage', content: [{ type: 'text', text: 'The earlier prompt.' }] }] },
  ],
  'thread-b': [{ id: 'running-b', status: 'inProgress', startedAt: now - 60, items: [{ id: 'user-b', type: 'userMessage', content: [{ type: 'text', text: 'Keep working in session B.' }] }, { id: 'comment-b', type: 'agentMessage', phase: 'commentary', text: 'Checking the mobile layout.' }] }],
};
const historyItem = index => {
  const common = { id: 'history-' + index, createdAt: now - 1000 + index };
  if (index % 4 === 0) return { ...common, type: 'userMessage', content: [{ type: 'text', text: 'History prompt ' + index }] };
  if (index % 4 === 1) return { ...common, type: 'agentMessage', text: 'History answer ' + index + '\n\nA complete response with useful details.' };
  if (index % 4 === 2) return { ...common, type: 'reasoning', summary: ['Public summary ' + index], content: [] };
  return { ...common, type: 'commandExecution', command: 'echo history-' + index, status: 'completed', exitCode: 0, aggregatedOutput: 'History output ' + index };
};
turns['thread-c'] = [
  { id: 'recent-c', status: 'completed', startedAt: now - 500, items: Array.from({ length: 45 }, (_, index) => historyItem(index + 40)) },
  { id: 'old-c', status: 'completed', startedAt: now - 1000, items: Array.from({ length: 40 }, (_, index) => historyItem(index)) },
];
turns['thread-d'] = [{ id: 'short-d', status: 'completed', startedAt: now - 500, items: Array.from({ length: 22 }, (_, index) => ({ id: 'short-' + index, type: 'agentMessage', text: 'Short message ' + index, createdAt: now - 500 + index })) }];

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, colorScheme: 'dark' });
  await page.clock.install({ time: Date.now() });
  const touchEmulation = await page.context().newCDPSession(page);
  await touchEmulation.send('Emulation.setTouchEmulationEnabled', { enabled: false });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'browser-test-csrf', url: 'https://webui.test/', secure: true, sameSite: 'Strict' }]);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let authStatus = 200;
  const paths = [];
  const gatewayCommands = [];
  let holdGatewayCommand = false;
  const heldGatewayCommands = [];
  let gatewayStatusCode = 200;
  let gatewayStatusText = 'Linux worker: waiting for active turns. Remote worker: checking update.';
  let gatewayUpdateWorkers = [
    { worker_id: 'w', request_id: 'update-local', name: 'Linux worker', state: 'pending' },
    { worker_id: 'remote', request_id: 'update-remote', name: 'Remote worker', state: 'pending' },
  ];
  let holdSessionInventory = false;
  let notifySessionInventoryHeld = () => {};
  const heldSessionInventories = [];
  const sessionDeletes = [];
  const deleteStatuses = new Map();
  let rejectNextDelete = false;
  let dropNextDeleteAcknowledgement = false;
  await page.route('https://webui.test/**', async route => {
    const url = new URL(route.request().url()); paths.push(url.pathname);
    if (url.pathname === '/tgw/api/v1/webui/push/config') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ supported: false, scope: 'all', subscribed: false }) });
    if (url.pathname === '/tgw/api/v1/webui/sessions/delete') {
      if (route.request().method() === 'POST') {
        const request = route.request().postDataJSON();
        sessionDeletes.push({ ...request, csrf: route.request().headers()['x-csrf-token'] });
        if (rejectNextDelete) {
          rejectNextDelete = false;
          return route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ message: 'This worker is offline. Reconnect it before deleting this session.' }) });
        }
        const status = { command_id: request.request_id, session_id: request.session_id, status: 'pending', pending: true, deleted: false, message: 'Session deletion queued. The working directory and files will be kept.' };
        deleteStatuses.set(request.request_id, status);
        if (dropNextDeleteAcknowledgement) { dropNextDeleteAcknowledgement = false; return route.abort('failed'); }
        return route.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify(status) });
      }
      const status = deleteStatuses.get(url.searchParams.get('request_id') || url.searchParams.get('command_id'));
      return route.fulfill({ status: status ? 200 : 404, contentType: 'application/json', body: JSON.stringify(status || { error: 'Deletion request not found. Check the session list before retrying.' }) });
    }
    if (url.pathname === '/tgw/api/v1/webui/sessions') {
      if (holdSessionInventory) { heldSessionInventories.push(route); notifySessionInventoryHeld(); return; }
      return route.fulfill({ status: authStatus, contentType: 'application/json', body: JSON.stringify({ sessions }) });
    }
    if (url.pathname === '/tgw/api/v1/admin/session') return route.fulfill({ status: authStatus, contentType: 'application/json', body: '{}' });
    if (url.pathname === '/tgw/api/v1/webui/commands') {
      const request = route.request().method() === 'POST' ? route.request().postDataJSON() : Object.fromEntries(url.searchParams);
      gatewayCommands.push({ method: route.request().method(), csrf: route.request().headers()['x-csrf-token'], ...request });
      if (holdGatewayCommand) { heldGatewayCommands.push(route); return; }
      const command = request.command;
      return route.fulfill({ status: authStatus !== 200 ? authStatus : command === 'tgstatus' ? gatewayStatusCode : 200, contentType: 'application/json', body: JSON.stringify({ command, text: command === 'tgupdateworkers' ? 'Linux worker: update queued after active turns finish.' : command === 'tginstances' ? 'Linux worker / Primary Codex · online' : gatewayStatusText, workers: command === 'tgupdateworkers' ? gatewayUpdateWorkers.map(worker => ({ ...worker, state: 'queued' })) : gatewayUpdateWorkers, gateway_version: '0.5.test' }) });
    }
    const staticFiles = ['webui-format.js', 'webui-commands.js', 'webui-command-ui.js', 'webui-notifications.js', 'webui.js', 'webui.css', 'webui-icon.svg', 'webui-icon-180.png', 'webui-icon-192.png', 'webui-icon-512.png'];
    const filename = url.pathname.endsWith('/manifest.webmanifest') ? 'webui-manifest.webmanifest' : staticFiles.find(name => url.pathname.endsWith('/' + name)) || 'webui.html';
    const contentType = filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.png') ? 'image/png' : filename.endsWith('.svg') ? 'image/svg+xml' : filename.endsWith('.webmanifest') ? 'application/manifest+json' : 'text/html';
    await route.fulfill({ contentType, headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(({ turns, rateLimits, sessions }) => {
    // Desktop automation has no native phone keyboard. Keep ordinary viewport
    // changes real, but allow keyboard resize/pan events to be delivered
    // independently, as mobile Safari does during input focus.
    const nativeViewport = window.visualViewport;
    const viewport = new EventTarget();
    let viewportOverride = {};
    for (const key of ['width', 'height', 'offsetTop', 'offsetLeft', 'pageTop', 'pageLeft', 'scale']) {
      Object.defineProperty(viewport, key, { get: () => {
        if (viewportOverride[key] !== undefined) return viewportOverride[key];
        if (key === 'pageTop' && viewportOverride.offsetTop !== undefined) return scrollY + viewportOverride.offsetTop;
        if (key === 'pageLeft' && viewportOverride.offsetLeft !== undefined) return scrollX + viewportOverride.offsetLeft;
        return nativeViewport?.[key] ?? ({ width: innerWidth, height: innerHeight, scale: 1 }[key] || 0);
      } });
    }
    for (const type of ['resize', 'scroll']) nativeViewport?.addEventListener(type, () => viewport.dispatchEvent(new Event(type)));
    Object.defineProperty(window, 'visualViewport', { value: viewport });
    window.testViewport = (values, type = 'resize') => { viewportOverride = { ...values }; if (type) viewport.dispatchEvent(new Event(type)); };
    const nativeAnimationFrame = requestAnimationFrame.bind(window);
    window.testAnimationFrames = 0;
    window.requestAnimationFrame = callback => { window.testAnimationFrames++; return nativeAnimationFrame(callback); };
    window.testSockets = []; window.testSent = []; window.testHoldResume = false; window.testFailResume = false; window.testDropSubmit = false; window.testRejectAnswers = false;
    window.testHoldLimits = false; window.testFailLimits = false; window.testMissingLimits = false;
    window.testDropCommand = false; window.testRejectCommand = false; window.testHoldCommand = false; window.testHoldOlderItems = false; window.testFailOlderItems = false;
    window.testHoldInitialItems = false; window.testFailInitialItems = false;
    window.testHoldMetadata = false; window.testHoldQuestions = false; window.testFailQuestions = false;
    window.testHoldSteer = false; window.testRejectSteer = false; window.testDropSteer = false;
    window.testHoldGatewayAnswer = false; window.testAnsweredOldQuestion = false; window.testReplacementQuestion = false;
    window.testHoldImageRead = false;
    const NativeFileReader = window.FileReader;
    window.FileReader = class extends NativeFileReader {
      readAsDataURL(file) {
        if (window.testHoldImageRead) { window.testHeldImageRead = () => super.readAsDataURL(file); return; }
        return super.readAsDataURL(file);
      }
    };
    window.testActivityRevision = 1; window.testActivitySockets = []; window.testPauseActivityHeartbeats = false; window.testHoldActivitySnapshot = false;
    window.testActivityRows = sessions.map(session => ({ session_id: session.session_id, state: session.state, pending_questions: 0, worker_connectivity: 'connected', runtime_state: 'running', active_turn_id: session.state === 'running' ? 'running-' + session.session_id : '' }));
    window.testSettingsRevision = 0;
    window.testPublishSettings = (id, model, effort, revision) => {
      const row = { ...window.testActivityRows.find(row => row.session_id === id), session_id: id, model, reasoning_effort: effort, settings_revision: revision || '1:' + (++window.testSettingsRevision) };
      window.testActivityRows = window.testActivityRows.map(current => current.session_id === id ? row : current);
      const socket = window.testActivitySocket;
      socket.emit({ type: 'activity_event', version: 2, sequence: ++socket.sequence, revision: window.testActivityRevision++, event: 'session_settings_changed', session_id: id, session: row });
    };
    class FakeSocket {
      static OPEN = 1;
      constructor(url) {
        this.url = url; this.readyState = 0; this.session = new URL(url).searchParams.get('session_id');
        this.activity = new URL(url).pathname.endsWith('/activity');
        this.sequence = 0;
        if (this.activity) { window.testActivitySocket = this; window.testActivitySockets.push(this); } else window.testSockets.push(this);
        setTimeout(() => {
          if (this.readyState === 3) return;
          this.readyState = 1; this.onopen?.({});
          const initial = this.activity ? { type: 'activity_snapshot', version: 2, sequence: this.sequence, revision: window.testActivityRevision++, sessions: window.testActivityRows } : { type: 'ready' };
          if (this.activity && window.testHoldActivitySnapshot) this.heldInitialActivity = initial; else this.emit(initial);
          if (this.activity) this.heartbeatTimer = setInterval(() => { if (!window.testPauseActivityHeartbeats && this.readyState === 1) this.emit({ type: 'heartbeat', version: 2, sequence: this.sequence }); }, 15000);
        }, 5);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; clearInterval(this.heartbeatTimer); this.onclose?.({ code }); }
      drop(code = 1006) { this.close(code); }
      send(raw) {
        const frame = JSON.parse(raw); window.testSent.push({ session: this.session, ...frame });
        if (!frame.method) {
          setTimeout(() => this.emit(window.testRejectAnswers ? { id: frame.id, error: { message: 'Answer rejected by runtime' } } : { method: 'serverRequest/resolved', params: { requestId: frame.id } }), 15);
          return;
        }
        let result = {};
        if (frame.method === 'thread/resume') {
          if (window.testHoldResume) { this.held = frame; return; }
          if (window.testFailResume) { this.emit({ id: frame.id, error: { message: 'Thread unavailable' } }); return; }
          result = { thread: { id: frame.params.threadId }, model: 'codex-model', reasoningEffort: 'high' };
        } else if (frame.method === 'thread/turns/list') {
          result = frame.params.itemsView === 'notLoaded' ? { data: (turns[frame.params.threadId] || []).slice(0, frame.params.limit || 1).map(({ items, ...turn }) => ({ ...turn, items: [] })), nextCursor: null }
            : frame.params.cursor ? { data: [{ id: 'earliest', status: 'completed', startedAt: 1600000000, items: [{ id: 'earliest-message', type: 'agentMessage', text: 'The first conversation.' }] }], nextCursor: null } : { data: turns[frame.params.threadId], nextCursor: frame.params.threadId === 'thread-a' ? 'older' : null };
          if (window.testHoldMetadata && frame.params.itemsView === 'notLoaded') { this.heldMetadata = { frame, result }; return; }
        } else if (frame.method === 'thread/items/list') {
          const items = (turns[frame.params.threadId] || []).flatMap(turn => [...turn.items].reverse().map(item => ({ turnId: turn.id, item: { ...item, createdAt: item.createdAt || turn.startedAt } })));
          if (frame.params.threadId === 'thread-a') items.push({ turnId: 'earliest', item: { id: 'earliest-message', type: 'agentMessage', text: 'The first conversation.', createdAt: 1600000000 } });
          const offset = Number(frame.params.cursor || 0);
          const limit = frame.params.limit || 20;
          // Keep the small fixture's old item on a second native page so
          // existing chronology and malformed-history checks still cover paging.
          const size = frame.params.threadId === 'thread-a' && !offset ? Math.min(limit, items.length - 1) : limit;
          result = { data: items.slice(offset, offset + size), nextCursor: offset + size < items.length ? String(offset + size) : null, backwardsCursor: null };
          if (window.testFailInitialItems && !frame.params.cursor) { window.testFailInitialItems = false; setTimeout(() => this.emit({ id: frame.id, error: { message: 'Initial conversation history unavailable' } }), 1); return; }
          if (window.testHoldInitialItems && !frame.params.cursor) { this.heldInitialItems = { frame, result }; return; }
          if (window.testFailOlderItems && frame.params.cursor) { window.testFailOlderItems = false; setTimeout(() => this.emit({ id: frame.id, error: { message: 'Saved history cursor expired' } }), 1); return; }
          if (window.testHoldOlderItems && frame.params.cursor) { this.heldOlderItems = { frame, result }; return; }
        } else if (frame.method === 'model/list') result = { data: [{ id: 'codex-model', model: 'codex-model', displayName: 'Codex model', supportedReasoningEfforts: [{ reasoningEffort: 'low' }, { reasoningEffort: 'high' }], defaultReasoningEffort: 'high' }] };
        else if (frame.method === 'account/rateLimits/read') {
          if (window.testHoldLimits) { this.heldLimits = frame; return; }
          if (window.testFailLimits) { setTimeout(() => this.emit({ id: frame.id, error: { message: 'Rate limits unavailable' } }), 1); return; }
          result = window.testMissingLimits ? { rateLimits: null } : { rateLimits };
        }
        else if (frame.method === 'gateway/questions') {
          result = { questions: this.session === 'c' && !window.testAnsweredOldQuestion ? [{ approvalId: window.testReplacementQuestion ? 'approval-replacement-c' : 'approval-old-c', async: false, method: 'item/tool/requestUserInput', params: { threadId: 'thread-c', turnId: 'old-c', questions: [{ id: 'question-c', question: window.testReplacementQuestion ? 'Replacement question with the same pending count?' : 'Pending question outside the latest 20 items?' }] } }] : [] };
          if (window.testHoldQuestions) { this.heldQuestions = { frame, result }; return; }
          if (window.testFailQuestions) { setTimeout(() => this.emit({ id: frame.id, error: { message: 'Question inventory unavailable' } }), 1); return; }
        }
        else if (frame.method === 'gateway/answer') {
          if (window.testHoldGatewayAnswer) { this.heldGatewayAnswer = frame; return; }
          window.testAnsweredOldQuestion = true; result = { state: 'completed' };
        }
        else if (frame.method === 'gateway/command') {
          if (window.testDropCommand) { this.drop(); return; }
          if (window.testRejectCommand) { window.testRejectCommand = false; setTimeout(() => this.emit({ id: frame.id, error: { message: 'Setting rejected by runtime' } }), 1); return; }
          if (window.testHoldCommand) { this.heldCommand = frame; return; }
          const name = frame.params.name; const args = frame.params.args || '';
          if (name === 'permissions' && !args) result = { text: 'Choose session permissions.', permissions: { options: [
            { id: 'read-only', label: 'Read Only', description: 'Inspect files without changing them.' },
            { id: 'ask-for-approval', label: 'Ask for approval', description: 'Allow changes in the working directory.' },
            { id: 'full-access', label: 'Full Access', description: 'Allow unrestricted system access.' }
          ] } };
          else if (name === 'permissions' && args === 'full-access') result = { text: 'Enable Full Access for this session?', permissions: { options: [{ id: 'confirm-full-access', label: 'Enable Full Access' }, { id: 'cancel', label: 'Cancel' }] } };
          else if (name === 'model') result = !args
            ? { text: 'Choose a model.', model_menu: { options: [{ args: 'codex-model', label: 'Codex model' }] } }
            : args === 'codex-model'
              ? { text: 'Choose reasoning effort.', model_menu: { options: [{ args: 'codex-model low', label: 'Low' }, { args: 'codex-model high', label: 'High' }] } }
              : { text: 'Model updated: ' + args };
          else if (name === 'new') { const options = JSON.parse(args); result = { text: 'Created ' + options.name, session: { session_id: 'created', codex_thread_id: 'thread-created', worker_id: 'w', runtime_id: 'runtime', name: options.name, cwd: options.cwd || '/projects/new' } }; }
          else if (name === 'status') result = { text: 'Codex session\nModel: codex-model\nWorkspace: /projects/gateway\nPrimary limit used: 26%' };
          else result = { text: name === 'permissions' ? 'Permissions updated: ' + args : '/' + name + (args ? ' ' + args : '') + ' completed.' };
        }
        else if (frame.method === 'turn/start') {
          if (window.testDropSubmit) { this.drop(); return; }
          result = { turn: { id: 'started-web', status: 'inProgress', items: [] } };
          setTimeout(() => { this.emit({ method: 'turn/started', params: { threadId: frame.params.threadId, turn: result.turn } }); this.emit({ method: 'item/completed', params: { threadId: frame.params.threadId, turnId: 'started-web', item: { id: 'new-user', type: 'userMessage', content: frame.params.input } } }); }, 1);
        } else if (frame.method === 'turn/steer') {
          if (window.testHoldSteer) { this.heldSteer = frame; return; }
          if (window.testRejectSteer) { window.testRejectSteer = false; setTimeout(() => this.emit({ id: frame.id, error: { message: 'Steering rejected by runtime' } }), 1); return; }
          if (window.testDropSteer) { this.drop(); return; }
        } else if (frame.method === 'turn/interrupt') setTimeout(() => this.emit({ method: 'turn/completed', params: { threadId: frame.params.threadId, turn: { id: frame.params.turnId, status: 'interrupted' } } }), 1);
        setTimeout(() => {
          if (frame.method === 'gateway/command' && !result.model_menu && frame.params.args) {
            if (frame.params.name === 'model' && !frame.params.args.startsWith('--')) {
              const [model, effort] = frame.params.args.split(/\s+/); window.testPublishSettings(this.session, model, effort || '');
            } else if (frame.params.name === 'reasoning') {
              window.testPublishSettings(this.session, window.testActivityRows.find(row => row.session_id === this.session)?.model || 'codex-model', frame.params.args);
            }
          }
          this.emit({ id: frame.id, result });
        }, 1);
      }
    }
    window.WebSocket = FakeSocket;
  }, { turns, rateLimits, sessions });
  const current = async value => page.evaluate(value => window.testSockets.at(-1).emit(value), value);
  const activity = async values => page.evaluate(values => {
    const socket = window.testActivitySocket; window.testActivityRows = values;
    socket.emit({ type: 'activity_snapshot', version: 2, sequence: ++socket.sequence, revision: window.testActivityRevision++, sessions: values });
  }, values);
  const activityEvent = (event, session) => page.evaluate(({ event, session }) => {
    const socket = window.testActivitySocket; const id = typeof session === 'string' ? session : session.session_id;
    window.testActivityRows = window.testActivityRows.filter(row => row.session_id !== id);
    if (typeof session !== 'string') window.testActivityRows.push(session);
    socket.emit({ type: 'activity_event', version: 2, sequence: ++socket.sequence, revision: window.testActivityRevision++, event, session_id: id, session: typeof session === 'string' ? null : session });
  }, { event, session });
  const connected = () => page.waitForSelector('#connection[data-state="connected"]');
  const touchTap = async locator => {
    await locator.waitFor(); await locator.scrollIntoViewIfNeeded();
    const bounds = await locator.boundingBox();
    await touchEmulation.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 }] });
    await touchEmulation.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  };
  const pasteImage = (name = 'clipboard.png', switchTo = '') => page.evaluate(({ name, switchTo }) => {
    const canvas = document.createElement('canvas'); canvas.width = 4; canvas.height = 3;
    canvas.getContext('2d').fillRect(0, 0, canvas.width, canvas.height);
    const dataURL = canvas.toDataURL('image/png');
    const bytes = Uint8Array.from(atob(dataURL.split(',')[1]), char => char.charCodeAt(0));
    const clipboard = new DataTransfer(); clipboard.items.add(new File([bytes], name, { type: 'image/png' }));
    const event = new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: clipboard });
    document.querySelector('#prompt').dispatchEvent(event);
    // Switching within this task guarantees it precedes image-load events.
    if (switchTo) document.querySelector('[data-session-id="' + switchTo + '"]').click();
    return { dataURL, handled: event.defaultPrevented };
  }, { name, switchTo });
  const atTranscriptEnd = async context => {
    await page.waitForFunction(() => {
      const scroller = document.querySelector('#transcript');
      return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= 2;
    }, null, { timeout: 5000 }).catch(async () => {
      const metrics = await page.locator('#transcript').evaluate(scroller => ({ top: scroller.scrollTop, height: scroller.scrollHeight, visible: scroller.clientHeight, trace: window.testScrollTrace?.slice(-25) }));
      assert.fail(context + ': conversation did not stay at its end: ' + JSON.stringify(metrics));
    });
    const geometry = await page.evaluate(() => ({ last: document.querySelector('#messages article:last-child')?.getBoundingClientRect().bottom, composer: document.querySelector('#composer').getBoundingClientRect().top }));
    assert.ok(geometry.last <= geometry.composer + 1, context + ': the latest message remains above the composer');
  };
  const assertToolColors = async context => {
    for (const selector of ['.role', 'pre']) {
      const rgb = await page.evaluate(selector => getComputedStyle(document.querySelector('[data-item-id="tool-a"] ' + selector)).color.match(/\d+/g).map(Number), selector);
      assert.ok(rgb[2] > rgb[0], 'Tool ' + selector + ' uses blue text in ' + context);
    }
    for (const selector of ['summary', '.tool-title']) {
      const colors = await page.evaluate(selector => ({ command: getComputedStyle(document.querySelector('[data-item-id="tool-a"] ' + selector)).color, muted: getComputedStyle(document.querySelector('#session-location')).color }), selector);
      assert.equal(colors.command, colors.muted, 'Tool command ' + selector + ' uses muted gray text in ' + context);
    }
  };
  const choose = async id => {
    if (await page.locator('#show-sessions').isVisible() && await page.locator('#show-sessions').getAttribute('aria-expanded') !== 'true') await page.locator('#show-sessions').click();
    await page.locator('[data-session-id="' + id + '"]').click(); await connected();
    await page.waitForFunction(() => ['Ready', 'Working'].includes(document.querySelector('#turn-state').textContent));
  };
  const gatewayStatusReads = () => gatewayCommands.filter(request => request.command === 'tgstatus' && request.method === 'GET').length;
  const nextGatewayStatus = () => page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.pathname === '/tgw/api/v1/webui/commands' && url.searchParams.get('command') === 'tgstatus' && response.request().method() === 'GET';
  });
  try {
    await page.goto('https://webui.test/tgw/webui/');
    await page.waitForSelector('[data-session-id="a"]');
    if (process.env.WEBUI_SCREENSHOTS) {
      fs.mkdirSync(process.env.WEBUI_SCREENSHOTS, { recursive: true });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-connect.png') });
    }
    // Gateway operations remain available before selecting a Codex session.
    await page.getByRole('button', { name: 'Open command menu', exact: true }).click();
    await page.getByLabel('Find a command', { exact: true }).fill('tgupdateworkers');
    await page.locator('#command-suggestions .command-option').first().click();
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('update queued'));
    assert.equal(gatewayCommands.filter(request => request.command === 'tgupdateworkers' && request.method === 'POST').length, 1, 'Worker updates can be requested without an active session');
    assert.equal(gatewayCommands.find(request => request.command === 'tgupdateworkers').csrf, 'browser-test-csrf', 'Update requests include the same-origin CSRF token');
    assert.equal(await page.evaluate(() => window.testSockets.length), 0, 'Gateway commands do not create a Codex connection');
    await page.waitForFunction(() => !document.querySelector('#show-commands').disabled);
    await page.evaluate(() => { window.updateStatusFocus = document.activeElement; });
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('waiting for active turns'));
    assert.equal(await page.locator('#command-title').textContent(), '/tgupdateworkers', 'Automatic progress remains in the update command panel');
    assert.equal(await page.evaluate(() => document.activeElement === window.updateStatusFocus), true, 'Automatic progress never steals focus');
    gatewayUpdateWorkers[0].state = 'completed';
    gatewayStatusText = 'Linux worker: updated and restarted · 0.5.test. Remote worker: waiting for active turns.';
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('updated and restarted'));
    assert.doesNotMatch(await page.locator('#command-content').textContent(), /All worker update checks finished/, 'One completed worker does not hide another pending update');
    // Hidden tabs stop polling, then read the latest result when visible.
    await page.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, value: true }); document.dispatchEvent(new Event('visibilitychange')); });
    const hiddenUpdateReads = gatewayStatusReads();
    await page.clock.fastForward(180000);
    assert.equal(gatewayStatusReads(), hiddenUpdateReads, 'A hidden tab does not poll maintenance status');
    await Promise.all([nextGatewayStatus(), page.evaluate(() => { delete document.hidden; document.dispatchEvent(new Event('visibilitychange')); }), page.clock.fastForward(1)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('waiting for active turns'));
    const longWaitReads = gatewayStatusReads();
    await page.clock.fastForward(5100);
    assert.equal(gatewayStatusReads(), longWaitReads, 'Long-running worker updates use a slower polling interval');
    gatewayUpdateWorkers[1].state = 'up_to_date';
    gatewayStatusText = 'Linux worker: updated and restarted · 0.5.test. Remote worker: up to date · no restart. Codex: up to date.';
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(15000)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('All worker update checks finished'));
    assert.match(await page.locator('#command-content').textContent(), /Codex: up to date/, 'The panel displays the server runtime report without a separate mutation');
    const completedUpdateReads = gatewayStatusReads();
    await page.clock.fastForward(30000);
    assert.equal(gatewayStatusReads(), completedUpdateReads, 'Completion stops automatic status reads');
    assert.equal(gatewayCommands.filter(request => request.command === 'tgupdateworkers').length, 1, 'Progress checks never replay the update POST');
    assert.ok(gatewayCommands.filter(request => request.command === 'tgstatus').every(request => request.method === 'GET' && request.session_id === undefined), 'Automatic status reads are read-only and independent of the selected session');
    gatewayUpdateWorkers.forEach(worker => { worker.state = 'pending'; });
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    // Connection phases describe the actual wait. Recent messages appear as
    // soon as their page arrives, while the active-turn check still gates sends.
    await page.evaluate(() => { window.testHoldResume = true; window.testHoldMetadata = true; window.testHoldQuestions = true; });
    await page.locator('[data-session-id="a"]').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).held));
    assert.match(await page.locator('#connection-text').textContent(), /Connected · Restoring session/, 'Transport readiness is visible before the slow resume returns');
    await page.locator('#prompt').fill('A draft kept while this session loads');
    assert.equal(await page.locator('#send').isDisabled(), true);
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1); window.testHoldResume = false;
      socket.emit({ id: socket.held.id, result: { thread: { id: 'thread-a' }, model: 'codex-model', reasoningEffort: 'high' } });
    });
    await page.waitForSelector('[data-item-id="reply-a"]');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldMetadata));
    assert.equal(await page.evaluate(() => window.testSent.some(frame => frame.session === 'a' && frame.method === 'gateway/questions')), false, 'Optional worker lookup does not start until essential history/state reads finish');
    assert.match(await page.locator('#connection-text').textContent(), /Checking active turn/, 'History renders before delayed turn-state metadata');
    assert.equal(await page.locator('#send').isDisabled(), true, 'A prompt cannot guess whether to start or steer before active-turn metadata arrives');
    assert.equal(await page.locator('#older').isDisabled(), true, 'Earlier paging waits for the initial state checkpoint');
    await page.evaluate(() => { const socket = window.testSockets.at(-1), reply = socket.heldMetadata; window.testHoldMetadata = false; socket.emit({ id: reply.frame.id, result: reply.result }); });
    await connected();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldQuestions));
    assert.match(await page.locator('#connection-text').textContent(), /Checking questions/, 'Independent question restoration has visible progress');
    assert.equal(await page.locator('#send').isDisabled(), false, 'A slow question inventory does not prevent using the ready session');
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft kept while this session loads');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length), 0, 'Connection and loading never resend a draft');
    await current({ id: 'bootstrap-live-question', method: 'item/tool/requestUserInput', params: { threadId: 'thread-a', turnId: 'live-bootstrap', questions: [{ id: 'live', question: 'A new question after the history became ready?' }] } });
    await page.evaluate(() => { const socket = window.testSockets.at(-1), reply = socket.heldQuestions; window.testHoldQuestions = false; socket.emit({ id: reply.frame.id, result: reply.result }); });
    await page.waitForFunction(() => !document.querySelector('#connection-text').textContent.includes('Checking questions'));
    assert.equal(await page.getByLabel('A new question after the history became ready?', { exact: true }).count(), 1, 'A late initial snapshot cannot delete a newer native question');
    await current({ method: 'serverRequest/resolved', params: { threadId: 'thread-a', requestId: 'bootstrap-live-question' } });
    await page.locator('#prompt').fill('');
    await page.evaluate(() => { window.testHoldInitialItems = true; window.testHoldMetadata = true; window.testHoldQuestions = true; });
    await page.locator('[data-session-id="c"]').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldInitialItems && window.testSockets.at(-1).heldMetadata));
    await page.evaluate(() => { const old = window.testSockets.at(-1); window.testOldBootstrap = { old, callback: old.onmessage }; window.testHoldInitialItems = false; window.testHoldMetadata = false; window.testHoldQuestions = false; });
    await choose('b');
    await page.evaluate(() => {
      const { old, callback } = window.testOldBootstrap;
      for (const reply of [old.heldInitialItems, old.heldMetadata]) callback({ data: JSON.stringify({ id: reply.frame.id, result: reply.result }) });
    });
    assert.equal(await page.locator('#session-title').textContent(), sessions[1].name);
    assert.equal(await page.locator('[data-item-id^="history-"]').count(), 0, 'A late history page from the prior session cannot replace the selected conversation');
    assert.equal(await page.getByLabel('Pending question outside the latest 20 items?', { exact: true }).count(), 0, 'Late question restoration stays scoped to its old connection');
    assert.match(await page.locator('#send').textContent(), /Steer/, 'Old idle metadata cannot erase the selected session’s running turn');
    await page.evaluate(() => { window.testHoldQuestions = true; });
    await choose('c');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldQuestions));
    await page.evaluate(() => { const old = window.testSockets.at(-1); window.testOldQuestions = { old, callback: old.onmessage }; window.testHoldQuestions = false; });
    await choose('b');
    await page.evaluate(() => { const { old, callback } = window.testOldQuestions; callback({ data: JSON.stringify({ id: old.heldQuestions.frame.id, result: old.heldQuestions.result }) }); });
    assert.equal(await page.getByLabel('Pending question outside the latest 20 items?', { exact: true }).count(), 0, 'Late optional question responses cannot add requests to a new session');
    await page.evaluate(() => { window.testFailQuestions = true; });
    await choose('a');
    await page.waitForFunction(() => document.querySelector('#connection-text').textContent.includes('Questions unavailable'));
    assert.equal(await page.locator('[data-item-id="reply-a"]').isVisible(), true, 'An unavailable question inventory leaves the readable conversation connected');
    await page.locator('#prompt').fill('Keep this draft across a question lookup retry');
    assert.equal(await page.locator('#send').isDisabled(), false, 'Failed optional question lookup does not disable the composer');
    await page.locator('#disconnect').click();
    await page.evaluate(() => { window.testFailQuestions = false; });
    await page.locator('#reconnect').click(); await connected();
    await page.waitForFunction(() => !/Checking questions|Questions unavailable/.test(document.querySelector('#connection-text').textContent));
    assert.equal(await page.locator('#prompt').inputValue(), 'Keep this draft across a question lookup retry', 'Optional lookup recovery preserves the draft');
    await page.locator('#prompt').fill('');
    await choose('a');
    await page.waitForSelector('[data-item-id="reply-a"]');
    assert.equal(await page.locator('#messages article').first().getAttribute('data-item-id'), 'old-user', 'History stays chronological');
    assert.equal(await page.locator('#messages img').count(), 0, 'Untrusted HTML cannot become a DOM element');
    assert.equal(await page.evaluate(() => window.injected), undefined);
    assert.equal(await page.getByLabel('Dismissed Telegram question?', { exact: true }).count(), 0, 'An empty authoritative question snapshot prevents dismissed historical async questions from resurfacing');
    assert.equal(await page.locator('a[href^="javascript:"]').count(), 0, 'Unsafe link schemes remain text');
    assert.equal(await page.locator('#messages .diff-add').count(), 1);
    assert.equal(await page.locator('#messages .diff-delete').count(), 1);
    assert.equal(await page.locator('#messages table').count(), 1);
    assert.match(await page.locator('[data-item-id="reason-a"]').textContent(), /Checking keyboard behavior/, 'Reasoning summary is preserved when raw content is empty');
    await page.locator('[data-item-id="reason-a"] summary').click();
    assert.equal(await page.locator('[data-item-id="reason-a"]').getByText('Checking keyboard behavior and reconnects.', { exact: true }).isVisible(), true, 'Expanding reasoning shows the native public summary');
    assert.match(await page.locator('[data-item-id="reason-empty-a"]').textContent(), /Reasoning complete/i, 'Completed reasoning without a public summary has a simple completion label');
    assert.equal(await page.locator('[data-item-id="reason-empty-a"] details, [data-item-id="reason-empty-a"] summary').count(), 0, 'Empty reasoning has no expander that opens an empty box');
    assert.doesNotMatch(await page.locator('[data-item-id="reason-empty-a"]').textContent(), /No reasoning summary/i, 'Empty reasoning does not add a missing-content explanation');
    assert.equal(await page.locator('[data-item-id="reason-blank-a"] details, [data-item-id="reason-blank-a"] summary').count(), 0, 'Whitespace and ANSI controls do not create an empty reasoning expander');
    assert.doesNotMatch(await page.locator('#messages').textContent(), /UNEXPOSED_RAW_REASONING|UNEXPOSED_REASONING_TEXT/, 'Raw reasoning is never substituted for a missing public summary');
    assert.match(await page.locator('[data-item-id="reason-a"] summary').textContent(), /Reasoning complete/i, 'Completed public reasoning keeps an accurately labelled expander');
    for (const [id, action] of [['started', 'Started'], ['interacted', 'Interacted with'], ['interrupted', 'Interrupted'], ['completed', 'Completed']]) {
      const item = page.locator('[data-item-id="agent-' + id + '-a"]');
      assert.match(await item.textContent(), new RegExp(action), 'Native sub-agent activity displays its ' + id + ' action');
      assert.match(await item.textContent(), /\/root\/reviewer/, 'Sub-agent activity identifies the originating agent');
      assert.equal(await item.locator('details, summary').count(), 0, 'A native sub-agent activity without a body has no empty expander');
    }
    assert.equal(await page.locator('[data-item-id="agent-started-a"] img').count(), 0, 'Agent paths remain escaped text even when they contain HTML');
    assert.match(await page.locator('[data-item-id="agent-fallback-a"]').textContent(), /child-without-path/, 'An agent without a path is identified by its native thread ID');
    assert.match(await page.locator('[data-item-id="agent-future-a"]').textContent(), /Agent activity.*\/root\/future-agent/s, 'An unfamiliar agent action still identifies the agent clearly');
    assert.doesNotMatch(await page.locator('[data-item-id="agent-future-a"]').textContent(), /UNEXPOSED_AGENT_OUTPUT/, 'Agent activity never falls back to unrecognized raw output fields');
    await current({ method: 'turn/started', params: { threadId: 'thread-a', turn: { id: 'format-live-a', status: 'inProgress' } } });
    await current({ method: 'item/started', params: { threadId: 'thread-a', turnId: 'format-live-a', item: { id: 'live-reasoning-a', type: 'reasoning', summary: [] } } });
    await page.waitForSelector('[data-item-id="live-reasoning-a"]');
    assert.equal((await page.locator('[data-item-id="live-reasoning-a"] .role').textContent()).trim(), '• Reasoning', 'An incomplete live item is not labelled complete');
    assert.equal(await page.locator('[data-item-id="live-reasoning-a"] details').count(), 0, 'A live reasoning item without a summary has no empty expander');
    await current({ method: 'item/completed', params: { threadId: 'thread-a', turnId: 'format-live-a', item: { id: 'live-reasoning-a', type: 'reasoning', summary: ['A public summary became available.'] } } });
    await page.waitForSelector('[data-item-id="live-reasoning-a"] summary');
    assert.match(await page.locator('[data-item-id="live-reasoning-a"] .role').textContent(), /Reasoning complete/);
    await page.locator('[data-item-id="live-reasoning-a"] summary').click();
    assert.equal(await page.locator('[data-item-id="live-reasoning-a"]').getByText('A public summary became available.', { exact: true }).isVisible(), true, 'A newly completed public summary becomes expandable');
    await current({ method: 'turn/completed', params: { threadId: 'thread-a', turn: { id: 'format-live-a', status: 'completed' } } });
    // The activity channel includes sessions that are not selected. A pending
    // background question must be visible even while this viewer stays on A.
    const activitySnapshot = sessions.map(session => ({ session_id: session.session_id, state: session.state, active_turn_id: session.state === 'running' ? 'running-' + session.session_id : '', pending_questions: 0, worker_connectivity: 'connected', runtime_state: 'running' }));
    await page.evaluate(() => { window.initialActivitySocket = window.testActivitySocket; });
    assert.equal(await page.evaluate(() => new URL(window.testActivitySocket.url).searchParams.get('v')), '2', 'The browser explicitly requests the versioned global activity protocol');
    const backgroundConnections = await page.evaluate(() => window.testSockets.length);
    const sessionActivityRow = id => activitySnapshot.find(session => session.session_id === id);
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'running');
    await activityEvent('turn_ended', { ...sessionActivityRow('b'), state: 'idle', active_turn_id: '' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'idle');
    await activityEvent('turn_started', { ...sessionActivityRow('b'), active_turn_id: 'background-b' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'running');
    const runningColor = await page.locator('[data-session-id="b"] .name').evaluate(element => getComputedStyle(element).color.match(/\d+/g).map(Number));
    assert.ok(runningColor[1] > runningColor[0] && runningColor[1] > runningColor[2], 'Running session names are green');
    const workingStatus = page.locator('[data-session-id="b"] .session-status-label');
    assert.match(await workingStatus.textContent(), /● Working/, 'Running sidebar status includes a dot and Working label');
    const workingStyle = await workingStatus.evaluate(element => ({ color: getComputedStyle(element).color.match(/\d+/g).map(Number), shadow: getComputedStyle(element).textShadow, animation: getComputedStyle(element).animationName }));
    assert.ok(workingStyle.color[1] > workingStyle.color[0] && workingStyle.color[1] > workingStyle.color[2], 'The working label and dot use green text');
    assert.equal(workingStyle.shadow, 'none', 'The Working label has no text glow');
    assert.equal(workingStyle.animation, 'none', 'The Working label stays static');
    const workingDot = page.locator('[data-session-id="b"] .session-working-dot');
    const dotStyle = await workingDot.evaluate(element => {
      const dot = getComputedStyle(element), activity = getComputedStyle(document.querySelector('#activity .activity-dot'));
      return { color: dot.color.match(/\d+/g).map(Number), name: dot.animationName, duration: dot.animationDuration, timing: dot.animationTimingFunction, activity: { name: activity.animationName, duration: activity.animationDuration, timing: activity.animationTimingFunction } };
    });
    assert.ok(dotStyle.color[1] > dotStyle.color[0] && dotStyle.color[1] > dotStyle.color[2], 'Only the sidebar dot pulses green');
    assert.deepEqual({ name: dotStyle.name, duration: dotStyle.duration, timing: dotStyle.timing }, { name: 'pulse', duration: '1.8s', timing: 'ease-in-out' }, 'The sidebar dot uses the main activity pulse');
    assert.deepEqual(dotStyle.activity, { name: dotStyle.name, duration: dotStyle.duration, timing: dotStyle.timing }, 'Sidebar and main activity dots share the same animation');
    await workingDot.evaluate(element => { window.previousWorkingDot = element; });
    await activityEvent('turn_ended', sessionActivityRow('a'));
    assert.equal(await workingDot.evaluate(element => element === window.previousWorkingDot), true, 'Unrelated activity preserves the running dot node and its animation phase');
    assert.equal(await page.locator('[data-session-id="b"] .session-workspace').evaluate(element => getComputedStyle(element).textShadow), 'none', 'The workspace path does not inherit the working glow');
    await page.emulateMedia({ reducedMotion: 'reduce' });
    assert.equal(await workingDot.evaluate(element => getComputedStyle(element).animationName), 'none', 'Reduced-motion preferences disable the dot animation');
    await page.emulateMedia({ reducedMotion: 'no-preference' });
    await activityEvent('question_requested', { ...sessionActivityRow('b'), state: 'waiting_input', active_turn_id: 'background-b', pending_questions: 2, pending_revision: 'background-question-b' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'question');
    const questionStyle = await page.locator('[data-session-id="b"]').evaluate(element => ({ box: getComputedStyle(element).boxShadow, text: getComputedStyle(element.querySelector('.name')).textShadow, rgb: getComputedStyle(element.querySelector('.name')).color.match(/\d+/g).map(Number) }));
    assert.ok(questionStyle.box !== 'none' || questionStyle.text !== 'none', 'A session awaiting an answer has a visible glow');
    assert.ok(questionStyle.rgb[0] > questionStyle.rgb[2] && questionStyle.rgb[1] > questionStyle.rgb[2], 'Pending question names are yellow');
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('aria-current'), 'true', 'Background questions never change the selected session');
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await page.locator('#show-sessions').getAttribute('data-activity'), 'question');
    assert.equal((await page.locator('#show-sessions').textContent()).trim(), '?', 'Mobile session access signals a pending question in another session');
    await page.locator('#show-sessions').click();
    assert.equal(await page.locator('#session-search').evaluate(element => document.activeElement === element), false, 'Opening the mobile session menu does not open the keyboard by focusing search');
    const questionColors = await page.evaluate(() => ({ button: getComputedStyle(document.querySelector('#show-sessions')).color, session: getComputedStyle(document.querySelector('[data-session-id="b"] .name')).color }));
    assert.equal(questionColors.button, questionColors.session, 'The mobile question indicator matches the session’s yellow text');
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-background-question.png') });
    await page.locator('#close-sessions').click();
    await activityEvent('question_resolved', { ...sessionActivityRow('b'), active_turn_id: 'background-b' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'running');
    assert.equal((await page.locator('#show-sessions').textContent()).trim(), '☰', 'Resolving background questions restores the mobile menu icon');
    assert.equal(await page.locator('#show-sessions').getAttribute('data-activity'), 'running', 'The mobile menu signals running work in a different session after its question resolves');
    const mobileRunningColor = await page.locator('#show-sessions').evaluate(element => getComputedStyle(element).color.match(/\d+/g).map(Number));
    assert.ok(mobileRunningColor[1] > mobileRunningColor[0] && mobileRunningColor[1] > mobileRunningColor[2], 'The mobile hamburger uses green while any session is running');
    await activityEvent('turn_ended', { ...sessionActivityRow('b'), state: 'idle', active_turn_id: '' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'idle');
    assert.equal(await page.locator('#show-sessions').getAttribute('data-activity'), 'idle', 'The mobile menu returns to normal after the last background turn finishes');
    assert.equal(await page.locator('#show-sessions').evaluate(element => getComputedStyle(element).color), await page.locator('#disconnect').evaluate(element => getComputedStyle(element).color), 'Idle mobile menu restores the normal text color');
    assert.equal(await workingDot.count(), 0, 'Idle sessions remove the animated dot');
    assert.equal(await workingStatus.evaluate(element => getComputedStyle(element).textShadow), 'none', 'The working glow stops when the session becomes idle');
    assert.equal(await workingStatus.evaluate(element => getComputedStyle(element).animationName), 'none', 'Idle sessions do not keep the working animation');
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await page.locator('[data-session-id="b"] .name').evaluate(element => getComputedStyle(element).color), await page.locator('[data-session-id="a"] .name').evaluate(element => getComputedStyle(element).color), 'Idle session names return to the regular text color');
    assert.equal(await page.evaluate(() => window.testSockets.length), backgroundConnections, 'Background turn and question events never open another transcript connection');
    await activity(activitySnapshot);
    await current({ method: 'turn/started', params: { threadId: 'thread-a', turn: { id: 'status-test-a', status: 'inProgress' } } });
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('data-activity'), 'idle', 'A native hint cannot replace an authoritative activity snapshot');
    await choose('b');
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('data-activity'), 'idle', 'Switching preserves the independent feed’s last state until its turn event arrives');
    assert.equal(await page.evaluate(() => window.testActivitySocket === window.initialActivitySocket && window.testActivitySocket.readyState === 1), true, 'Changing the selected session keeps the global activity stream open');
    await activityEvent('turn_started', { ...sessionActivityRow('a'), state: 'running', active_turn_id: 'status-test-a' });
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('data-activity'), 'running', 'The subsequent gateway frame preserves the background running state');
    await activityEvent('turn_ended', sessionActivityRow('a'));
    await page.waitForFunction(() => document.querySelector('[data-session-id="a"]').dataset.activity === 'idle');
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('aria-current'), 'true', 'A background turn completion updates its indicator without changing the selected session');
    await choose('a');
    await activityEvent('turn_started', { ...sessionActivityRow('a'), state: 'running', active_turn_id: 'newer-global-a' });
    await current({ method: 'turn/completed', params: { threadId: 'thread-a', turn: { id: 'status-test-a', status: 'completed' } } });
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('data-activity'), 'running', 'An old native completion cannot override a newer global turn start');
    await activityEvent('turn_ended', sessionActivityRow('a'));
    await page.waitForFunction(() => document.querySelector('[data-session-id="a"]').dataset.activity === 'idle');
    await page.locator('#disconnect').click();
    assert.equal(await page.evaluate(() => window.testActivitySocket === window.initialActivitySocket && window.testActivitySocket.readyState === 1), true, 'Detaching the transcript keeps the global activity stream open');
    await activityEvent('question_requested', { ...sessionActivityRow('b'), state: 'waiting_input', pending_questions: 1, pending_revision: 'while-detached' });
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'question');
    assert.equal(await page.locator('#send').isDisabled(), true, 'Background activity does not reconnect a detached transcript');
    await page.locator('#reconnect').click(); await connected();
    assert.equal(await page.evaluate(() => window.testActivitySocket === window.initialActivitySocket), true);
    await activity(activitySnapshot);
    // Inventory changes are independent of the selected transcript, including
    // changes announced while an older inventory request is still in flight.
    const originalSessionName = sessions[0].name;
    sessions[0].name = 'Renamed by another client';
    await activityEvent('session_changed', sessionActivityRow('a'));
    await page.waitForFunction(() => document.querySelector('[data-session-id="a"] .name').textContent === 'Renamed by another client');
    sessions[0].name = originalSessionName;
    await activityEvent('session_changed', sessionActivityRow('a'));
    await page.waitForFunction(name => document.querySelector('[data-session-id="a"] .name').textContent === name, originalSessionName);
    const staleInventory = JSON.stringify({ sessions });
    holdSessionInventory = true;
    const pendingInventory = new Promise(resolve => { notifySessionInventoryHeld = resolve; });
    await page.locator('#refresh-sessions').click(); await pendingInventory;
    const addedSession = { session_id: 'activity-added', codex_thread_id: 'thread-added', worker_id: 'w', worker_name: 'Linux worker', runtime_name: 'Primary Codex', name: 'Created elsewhere', cwd: '/projects/created-elsewhere', state: 'idle' };
    sessions.push(addedSession);
    await activityEvent('session_changed', { ...sessionActivityRow('a'), session_id: addedSession.session_id });
    holdSessionInventory = false;
    for (const route of heldSessionInventories.splice(0)) await route.fulfill({ status: 200, contentType: 'application/json', body: staleInventory });
    await page.waitForSelector('[data-session-id="activity-added"]');
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('aria-current'), 'true', 'A new background session appears without changing the active session');
    sessions.pop();
    await activityEvent('session_removed', addedSession.session_id);
    await page.waitForSelector('[data-session-id="activity-added"]', { state: 'detached' });
    // Duplicates cannot roll an indicator back. A skipped event or heartbeat
    // sequence closes only the activity transport and resynchronizes it.
    await activityEvent('turn_started', { ...sessionActivityRow('b'), active_turn_id: 'sequence-test-b' });
    await page.evaluate(() => {
      const socket = window.testActivitySocket; window.activityBeforeGap = socket;
      socket.emit({ type: 'activity_event', version: 2, sequence: socket.sequence, revision: window.testActivityRevision++, event: 'turn_ended', session_id: 'b', session: { ...window.testActivityRows.find(row => row.session_id === 'b'), state: 'idle', active_turn_id: '' } });
    });
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('data-activity'), 'running', 'A duplicate event sequence cannot revert a newer indicator');
    const nativeConnectionsBeforeGap = await page.evaluate(() => window.testSockets.length);
    await page.evaluate(() => {
      const socket = window.testActivitySocket; window.oldActivityHandler = socket.onmessage;
      const row = { ...window.testActivityRows.find(row => row.session_id === 'b'), state: 'idle', active_turn_id: '' };
      window.testActivityRows = window.testActivityRows.map(current => current.session_id === 'b' ? row : current);
      socket.emit({ type: 'activity_event', version: 2, sequence: socket.sequence + 2, revision: window.testActivityRevision++, event: 'turn_ended', session_id: 'b', session: row });
    });
    await page.waitForFunction(() => window.activityBeforeGap.readyState === 3);
    await page.clock.fastForward(1100);
    await page.waitForFunction(() => window.testActivitySocket !== window.activityBeforeGap && window.testActivitySocket.readyState === 1);
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"]').dataset.activity === 'idle');
    assert.equal(await page.evaluate(() => window.testActivitySocket.sequence), 0, 'A recovered stream starts from a fresh authoritative snapshot');
    assert.equal(await page.evaluate(() => window.testSockets.length), nativeConnectionsBeforeGap, 'Activity recovery does not replace the selected transcript connection');
    await page.evaluate(() => window.oldActivityHandler({ data: JSON.stringify({ type: 'activity_event', version: 2, sequence: window.activityBeforeGap.sequence + 1, revision: window.testActivityRevision++, event: 'turn_started', session_id: 'b', session: { ...window.testActivityRows.find(row => row.session_id === 'b'), state: 'running', active_turn_id: 'stale-old-transport' } }) }));
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('data-activity'), 'idle', 'Late events from the closed stream cannot affect the replacement stream');
    await page.evaluate(() => {
      const socket = window.testActivitySocket; window.activityBeforeHeartbeatGap = socket;
      socket.emit({ type: 'heartbeat', version: 2, sequence: socket.sequence + 1 });
    });
    await page.waitForFunction(() => window.activityBeforeHeartbeatGap.readyState === 3);
    await page.clock.fastForward(1100);
    await page.waitForFunction(() => window.testActivitySocket !== window.activityBeforeHeartbeatGap && window.testActivitySocket.readyState === 1);
    // An in-stream snapshot also replaces state after a bounded event ring
    // overflows; it uses the next sequence rather than starting a new socket.
    const beforeResync = await page.evaluate(() => window.testActivitySockets.length);
    await activity(activitySnapshot.map(session => session.session_id === 'b' ? { ...session, state: 'waiting_input', pending_questions: 1, pending_revision: 'resynchronized-b' } : session));
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('data-activity'), 'question');
    await activity(activitySnapshot);
    assert.equal(await page.evaluate(() => window.testActivitySockets.length), beforeResync, 'A consecutive authoritative resync does not reconnect the stream');
    await page.evaluate(() => { window.testPauseActivityHeartbeats = true; window.activityBeforeStall = window.testActivitySocket; });
    await page.clock.fastForward(30000);
    await page.evaluate(() => { const socket = window.testActivitySocket; socket.emit({ type: 'heartbeat', version: 2, sequence: socket.sequence }); });
    await page.clock.fastForward(30000);
    assert.equal(await page.evaluate(() => window.testActivitySocket === window.activityBeforeStall && window.testActivitySocket.readyState === 1), true, 'A valid heartbeat keeps an otherwise idle global stream alive');
    await page.clock.fastForward(15001);
    await page.waitForFunction(() => window.activityBeforeStall.readyState === 3);
    assert.match(await page.locator('#activity-status').textContent(), /reconnect|connecting|retry/i, 'A stalled activity stream exposes recovery status');
    await page.evaluate(() => { window.testPauseActivityHeartbeats = false; });
    await page.clock.fastForward(1100);
    await page.waitForFunction(() => window.testActivitySocket !== window.activityBeforeStall && window.testActivitySocket.readyState === 1);
    assert.match(await page.locator('#activity-status').textContent(), /live|connected/i, 'The sidebar confirms the recovered global stream');
    assert.equal(await page.evaluate(() => window.testSockets.length), nativeConnectionsBeforeGap, 'A stalled global stream recovers without opening or replaying transcript sessions');
    // Opening a TCP/WebSocket connection is insufficient: a missing initial
    // authoritative snapshot must also time out and retry independently.
    await page.evaluate(() => {
      window.testHoldActivitySnapshot = true; window.testPauseActivityHeartbeats = true;
      window.activityBeforeHandshake = window.testActivitySocket; window.testActivitySocket.drop(1006);
    });
    await page.clock.fastForward(1100);
    await page.waitForFunction(() => window.testActivitySocket !== window.activityBeforeHandshake && Boolean(window.testActivitySocket.heldInitialActivity));
    await page.evaluate(() => { window.heldActivityHandshake = window.testActivitySocket; });
    await page.clock.fastForward(20001);
    await page.waitForFunction(() => window.heldActivityHandshake.readyState === 3);
    assert.match(await page.locator('#activity-status').textContent(), /reconnect|connecting|retry/i, 'An uninitialized stream cannot remain indefinitely connected');
    await page.evaluate(() => { window.testHoldActivitySnapshot = false; window.testPauseActivityHeartbeats = false; });
    await page.clock.fastForward(2100);
    await page.waitForFunction(() => window.testActivitySocket !== window.heldActivityHandshake && window.testActivitySocket.readyState === 1 && !window.testActivitySocket.heldInitialActivity);
    assert.equal(await page.evaluate(() => window.testSockets.length), nativeConnectionsBeforeGap, 'An activity handshake timeout leaves the selected transcript untouched');
    await page.waitForTimeout(120); // Allow the coalesced native turn render to finish before taking screenshots.
    for (const scheme of ['dark', 'light']) {
      await page.emulateMedia({ colorScheme: scheme });
      await assertToolColors('desktop ' + scheme + ' theme');
    }
    await page.emulateMedia({ colorScheme: 'dark' });
    if (process.env.WEBUI_SCREENSHOTS) {
      await page.locator('[data-item-id="tool-a"] details').evaluate(element => { element.open = true; });
      await page.locator('[data-item-id="tool-a"]').scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-desktop-tool-colors.png') });
      await page.locator('[data-item-id="tool-a"] details').evaluate(element => { element.open = false; });
    }
    assert.ok(await page.locator('#messages time[datetime]').count() > 0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    const groupStyle = await page.locator('.session-group').first().evaluate(element => {
      const worker = getComputedStyle(element.querySelector('.worker-name'));
      const runtime = getComputedStyle(element.querySelector('.runtime-name'));
      const group = getComputedStyle(element);
      return { workerColor: worker.color, runtimeColor: runtime.color, workerSize: parseFloat(worker.fontSize), runtimeSize: parseFloat(runtime.fontSize), background: group.backgroundColor, sidebar: getComputedStyle(element.closest('#sidebar')).backgroundColor };
    });
    assert.notEqual(groupStyle.workerColor, groupStyle.runtimeColor, 'Worker names have a distinct sidebar color');
    assert.ok(groupStyle.workerSize > groupStyle.runtimeSize, 'Worker names are larger than their runtime labels');
    assert.notEqual(groupStyle.background, 'rgba(0, 0, 0, 0)', 'Worker headings have a visible background');
    assert.notEqual(groupStyle.background, groupStyle.sidebar, 'The worker heading background separates session groups from the sidebar');
    const conversationFont = () => page.locator('#messages').evaluate(element => parseFloat(getComputedStyle(element).fontSize));
    const promptFont = () => page.locator('#prompt').evaluate(element => parseFloat(getComputedStyle(element).fontSize));
    assert.equal(await conversationFont(), 14, 'Desktop conversation starts at 14px');
    // A full-width container alone is insufficient: the previous two-row
    // footer still left most of a wide desktop unused. Check actual placement.
    await page.setViewportSize({ width: 1920, height: 1000 });
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('Weekly'));
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const footerItems = await page.locator('#session-status').evaluate(element => {
      return [...element.children].filter(child => getComputedStyle(child).display !== 'none').map(child => {
        const rect = child.getBoundingClientRect();
        return { label: child.id || child.className, middle: rect.top + rect.height / 2, font: parseFloat(getComputedStyle(child).fontSize) };
      });
    });
    assert.ok(footerItems.some(item => item.label === 'model-controls'), 'Model controls share the status bar');
    assert.equal(await page.locator('.sidebar-footer').getByText('Powered by Codex', { exact: true }).count(), 0, 'The sidebar omits the redundant Powered by Codex caption');
    assert.equal(await page.locator('.sidebar-footer').getByRole('link', { name: /Operations console/ }).count(), 1, 'The sidebar keeps the operations console link');
    for (const item of footerItems) assert.ok(Math.abs(item.middle - footerItems[0].middle) < 2, `Wide desktop status item ${item.label} shares one row`);
    assert.ok(footerItems.find(item => item.label === 'usage').font >= 12, 'Desktop metrics use readable text rather than tiny metadata');
    assert.equal(await page.locator('#usage').textContent(), '9% context', 'Desktop status retains the context percentage without displaying a token count');
    assert.doesNotMatch(await page.locator('#session-status').textContent(), /tokens/i, 'The status bar omits token counts on every viewport');
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-desktop-status-row.png') });
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await page.locator('#font-increase').isVisible(), true);
    await page.locator('#prompt').fill('A draft survives font changes.');
    for (let i = 0; i < 8; i++) await page.locator('#font-increase').click();
    assert.equal(await conversationFont(), 22, 'Desktop font controls increase the conversation up to 22px');
    assert.equal(await promptFont(), 22, 'The prompt follows the desktop conversation font size');
    assert.equal(await page.locator('#font-increase').isDisabled(), true, 'Font increase stops at the readable maximum');
    assert.match(await page.locator('#font-size').textContent(), /22/);
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft survives font changes.');
    assert.equal(await page.locator('.worker-name').first().evaluate(element => parseFloat(getComputedStyle(element).fontSize)), groupStyle.workerSize, 'The main-window controls leave the sidebar size unchanged');
    // Full-width status must remain readable at the largest text size, even on
    // a compact desktop and with a workspace path much wider than the screen.
    for (const width of [1440, 900]) {
      await page.setViewportSize({ width, height: 1000 });
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const status = await page.locator('#session-status').evaluate(element => {
        const bounds = element.getBoundingClientRect();
        const composer = document.querySelector('#composer').getBoundingClientRect();
        return { width: bounds.width, composerWidth: composer.width, right: bounds.right, children: [...element.children].filter(child => getComputedStyle(child).display !== 'none').map(child => ({ id: child.id, left: child.getBoundingClientRect().left, right: child.getBoundingClientRect().right })) };
      });
      assert.ok(status.width >= status.composerWidth - 64, 'Status line uses the desktop composer width at ' + width);
      for (const child of status.children) assert.ok(child.right <= status.right + 1, `Status item ${child.id} fits at ${width}px with 22px text`);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Large-font desktop layout does not overflow at ' + width);
    }
    await page.setViewportSize({ width: 1440, height: 1000 });
    for (let i = 0; i < 10; i++) await page.locator('#font-decrease').click();
    assert.equal(await conversationFont(), 12);
    assert.equal(await page.locator('#font-decrease').isDisabled(), true, 'Font decrease stops at 12px');
    for (let i = 0; i < 6; i++) await page.locator('#font-increase').click();
    assert.equal(await conversationFont(), 18);
    assert.equal(await page.evaluate(() => localStorage.getItem('codex-webui-font-size')), '18');
    assert.deepEqual(await page.evaluate(() => Object.keys(localStorage)), ['codex-webui-font-size'], 'Only the font preference is persisted, never the prompt draft');
    await page.reload();
    await page.waitForSelector('[data-session-id="a"]');
    await choose('a');
    await page.waitForSelector('[data-item-id="reply-a"]');
    assert.equal(await conversationFont(), 18, 'Desktop font preference survives a reload');
    assert.equal(await promptFont(), 18);
    assert.equal(await page.locator('#prompt').inputValue(), '', 'Drafts are not persisted across a reload');
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('5h 74% left'));
    assert.match(await page.locator('#rate-limits').textContent(), /Weekly 58% left/, 'The weekly quota follows the Codex status-line convention');
    assert.equal(await page.locator('#rate-limits .rate-limit').count(), 2);
    for (const quota of await page.locator('#rate-limits .rate-limit').all()) assert.match(await quota.getAttribute('title'), /reset/i, 'Each quota has a reset-time tooltip');
    await page.locator('#transcript').evaluate(element => { element.scrollTop = 0; });
    await page.locator('[data-item-id="earliest-message"]').waitFor({ state: 'attached' });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.evaluate(() => { window.quotaArticle = document.querySelector('#messages article'); window.quotaScroll = document.querySelector('#transcript').scrollTop; window.quotaReadCount = window.testSent.filter(frame => frame.method === 'account/rateLimits/read').length; });
    await current({ method: 'account/rateLimits/updated', params: { rateLimits: { ...rateLimits, primary: { ...rateLimits.primary, usedPercent: 31 }, secondary: { ...rateLimits.secondary, usedPercent: 49 } } } });
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('5h 69% left'));
    assert.match(await page.locator('#rate-limits').textContent(), /Weekly 51% left/);
    await current({ method: 'account/rateLimits/updated', params: { rateLimits: { limitId: 'other-model', primary: { ...rateLimits.primary, usedPercent: 99 } } } });
    assert.match(await page.locator('#rate-limits').textContent(), /5h 69% left/, 'An unrelated model quota must not replace the default Codex quota');
    await current({ method: 'account/rateLimits/updated', params: { rateLimits: { limitId: 'codex', primary: null, secondary: { ...rateLimits.secondary, usedPercent: 49 } } } });
    assert.match(await page.locator('#rate-limits').textContent(), /5h 69% left/, 'Sparse updates preserve a previously reported primary quota');
    await page.waitForTimeout(1100);
    assert.equal(await page.evaluate(() => window.quotaArticle === document.querySelector('#messages article')), true, 'Quota updates do not re-render the transcript');
    assert.equal(await page.evaluate(() => document.querySelector('#transcript').scrollTop === window.quotaScroll), true, 'Quota updates preserve reading position');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'account/rateLimits/read').length === window.quotaReadCount), true, 'Quota events do not trigger polling or another quota read');
    // Missing or failed quota reads must never disable an otherwise connected
    // session. A delayed result from a different session must not restore it.
    await page.locator('#disconnect').click();
    assert.doesNotMatch(await page.locator('#rate-limits').textContent(), /69%|51%/, 'Disconnect clears the previous account quotas');
    await page.evaluate(() => { window.testFailLimits = true; });
    await page.locator('#reconnect').click(); await connected();
    await page.waitForFunction(() => /unavailable|not available/i.test(document.querySelector('#rate-limits').textContent));
    await page.locator('#prompt').fill('Quota errors do not block this prompt.');
    assert.equal(await page.locator('#send').isDisabled(), false);
    await page.locator('#disconnect').click();
    await page.evaluate(() => { window.testFailLimits = false; window.testMissingLimits = true; });
    await page.locator('#reconnect').click(); await connected();
    await page.waitForFunction(() => /unavailable|not available/i.test(document.querySelector('#rate-limits').textContent));
    assert.equal(await page.locator('#send').isDisabled(), false, 'An account without quotas remains usable');
    await page.evaluate(() => { window.testMissingLimits = false; window.testHoldLimits = true; });
    await choose('b');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldLimits));
    assert.doesNotMatch(await page.locator('#rate-limits').textContent(), /74%|58%/, 'A newly selected session does not show previous quotas');
    await current({ method: 'account/rateLimits/updated', params: { rateLimits: { limitId: 'codex', primary: { ...rateLimits.primary, usedPercent: 30 }, secondary: null } } });
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('5h 70% left'));
    await page.evaluate(rateLimits => { const socket = window.testSockets.at(-1); socket.emit({ id: socket.heldLimits.id, result: { rateLimits } }); }, rateLimits);
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('Weekly 58% left'));
    assert.match(await page.locator('#rate-limits').textContent(), /5h 70% left/, 'A delayed initial snapshot fills missing quotas without replacing a newer live value');
    await page.evaluate(() => { window.staleQuotaSocket = window.testSockets.at(-1); window.staleQuotaHandler = window.staleQuotaSocket.onmessage; window.testHoldLimits = false; });
    await choose('a');
    await page.waitForFunction(() => document.querySelector('#rate-limits').textContent.includes('5h 74% left'));
    await page.evaluate(rateLimits => window.staleQuotaHandler({ data: JSON.stringify({ id: window.staleQuotaSocket.heldLimits.id, result: { rateLimits: { ...rateLimits, primary: { ...rateLimits.primary, usedPercent: 99 } } } }) }), rateLimits);
    await page.evaluate(rateLimits => window.staleQuotaHandler({ data: JSON.stringify({ method: 'account/rateLimits/updated', params: { rateLimits: { ...rateLimits, primary: { ...rateLimits.primary, usedPercent: 99 } } } }) }), rateLimits);
    await page.waitForTimeout(100);
    assert.match(await page.locator('#rate-limits').textContent(), /5h 74% left/, 'An old socket cannot overwrite the selected session quotas');
    await page.locator('#prompt').fill('');
    // A request can outlive its HTTP acknowledgement. Switching sessions must
    // abort this browser request and release the new session's controls without
    // submitting a second update request or displaying a stale result.
    holdGatewayCommand = true;
    const updatesBeforeStall = gatewayCommands.filter(request => request.command === 'tgupdateworkers').length;
    await page.locator('#prompt').fill('/tgupdateworkers');
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#show-commands').disabled && document.querySelector('#command-content').textContent.includes('Working'));
    const abortedUpdate = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).pathname === '/tgw/api/v1/webui/commands' });
    await choose('b');
    const failedUpdate = await abortedUpdate;
    assert.match(failedUpdate.failure().errorText, /abort/i, 'Session switching aborts the outstanding gateway request');
    await page.locator('#prompt').fill('A different session stays usable after an update request.');
    assert.equal(await page.locator('#send').isDisabled(), false, 'The old HTTP operation does not keep the new composer busy');
    assert.equal(await page.locator('#show-commands').isDisabled(), false, 'The new session can open commands');
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'The old command panel is removed on session change');
    assert.equal(gatewayCommands.filter(request => request.command === 'tgupdateworkers').length, updatesBeforeStall + 1, 'Session switching never replays the update POST');
    holdGatewayCommand = false;
    for (const route of heldGatewayCommands.splice(0)) await route.abort().catch(() => {});
    await page.locator('#prompt').fill('');
    const requestWorkerUpdates = async () => {
      await page.locator('#prompt').fill('/tgupdateworkers');
      await page.locator('#send').click();
      await page.waitForFunction(() => !document.querySelector('#show-commands').disabled && document.querySelector('#command-content').textContent.includes('update queued'));
    };
    // Gateway commands work through the Send button even after disconnecting;
    // ordinary prompts still require a session transport.
    await page.locator('#disconnect').click();
    await page.locator('#prompt').fill('An ordinary disconnected prompt');
    assert.equal(await page.locator('#send').isDisabled(), true);
    await page.locator('#prompt').fill('/tgupdateworkers');
    assert.equal(await page.locator('#send').isDisabled(), false, 'Authenticated gateway commands do not require a Codex connection');
    await requestWorkerUpdates();
    await page.locator('#prompt').fill('A draft written while updates are pending');
    gatewayStatusText = 'Linux worker: pending. Remote worker: pending.';
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('Remote worker: pending'));
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft written while updates are pending', 'Progress checks preserve a new draft');
    assert.equal(await page.locator('#prompt').evaluate(element => document.activeElement === element), true, 'Progress checks do not move focus from a new draft');
    gatewayUpdateWorkers[0].state = 'failed'; gatewayUpdateWorkers[1].state = 'up_to_date';
    gatewayStatusText = 'Linux worker: update request failed. Remote worker: up to date.';
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('Update checks finished. See worker results above.'));
    const manualStatusBefore = gatewayStatusReads();
    await Promise.all([nextGatewayStatus(), page.getByRole('button', { name: /Check worker update status/ }).click()]);
    await page.waitForFunction(() => document.querySelector('#command-title').textContent === '/tgstatus' && !document.querySelector('#show-commands').disabled);
    assert.equal(gatewayStatusReads(), manualStatusBefore + 1, 'Manual status uses a read-only GET too');
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    gatewayUpdateWorkers.forEach(worker => { worker.state = 'pending'; });
    await choose('b');
    // An HTTP error stops observation with an explicit retry-status action,
    // never replaying the mutation or disabling the session composer.
    await requestWorkerUpdates();
    assert.equal(await page.locator('#command-title').textContent(), '/tgupdateworkers');
    gatewayStatusCode = 503;
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('Automatic status checks stopped'));
    const erroredReads = gatewayStatusReads(), updatesAfterReadError = gatewayCommands.filter(request => request.command === 'tgupdateworkers').length;
    await page.clock.fastForward(31000);
    assert.equal(gatewayStatusReads(), erroredReads, 'Failed observations do not create an unbounded error loop');
    assert.equal(gatewayCommands.filter(request => request.command === 'tgupdateworkers').length, updatesAfterReadError, 'A failed status read never repeats the update mutation');
    gatewayStatusCode = 200;
    // A later request for the same worker cannot masquerade as completion of
    // the request this panel is observing.
    await requestWorkerUpdates();
    gatewayUpdateWorkers[0].request_id = 'newer-local-update'; gatewayUpdateWorkers.forEach(worker => { worker.state = 'completed'; });
    gatewayStatusText = 'UNRELATED REQUEST COMPLETED';
    await Promise.all([nextGatewayStatus(), page.clock.fastForward(2100)]);
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('A newer worker update request replaced this status'));
    assert.doesNotMatch(await page.locator('#command-content').textContent(), /UNRELATED REQUEST COMPLETED|All worker update checks finished/, 'A superseding request is not reported as this request succeeding');
    gatewayUpdateWorkers.forEach(worker => { worker.state = 'pending'; });
    await requestWorkerUpdates();
    holdGatewayCommand = true;
    const statusRequestStarted = page.waitForRequest(request => new URL(request.url()).searchParams.get('command') === 'tgstatus');
    await Promise.all([statusRequestStarted, page.clock.fastForward(2100)]);
    const statusReadAborted = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).searchParams.get('command') === 'tgstatus' });
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    assert.match((await statusReadAborted).failure().errorText, /abort/i, 'Closing the update panel aborts its in-flight status read');
    holdGatewayCommand = false;
    for (const route of heldGatewayCommands.splice(0)) await route.abort().catch(() => {});
    const closedReads = gatewayStatusReads();
    await page.clock.fastForward(31000);
    assert.equal(gatewayStatusReads(), closedReads, 'A closed command panel never polls');
    await requestWorkerUpdates();
    const readsBeforeDisconnect = gatewayStatusReads();
    await page.locator('#disconnect').click();
    await page.clock.fastForward(31000);
    assert.equal(gatewayStatusReads(), readsBeforeDisconnect, 'Explicit session disconnect cancels pending update observation');
    await requestWorkerUpdates();
    const readsBeforeSwitch = gatewayStatusReads();
    await choose('a');
    await page.clock.fastForward(31000);
    assert.equal(gatewayStatusReads(), readsBeforeSwitch, 'Selecting another session cancels pending update observation');
    await page.locator('#prompt').fill('');
    // Heartbeats/status-only traffic must leave the idle transcript DOM intact.
    await page.evaluate(() => { window.originalArticle = document.querySelector('#messages article'); });
    await current({ type: 'heartbeat' });
    await page.waitForTimeout(120);
    assert.equal(await page.evaluate(() => window.originalArticle === document.querySelector('#messages article')), true, 'Idle heartbeat must not re-render');
    // Prompts sent through Telegram or another API client arrive unsolicited.
    // Both item lifecycle events must update one entry in the selected session.
    const externalPrompt = { threadId: 'thread-a', turnId: 'telegram-turn-a', item: { id: 'telegram-user-a', type: 'userMessage', content: [{ type: 'text', text: 'This prompt was sent through Telegram.' }] } };
    await current({ method: 'item/started', params: externalPrompt });
    await page.waitForSelector('[data-item-id="telegram-user-a"]');
    assert.match(await page.locator('[data-item-id="telegram-user-a"]').textContent(), /This prompt was sent through Telegram\./, 'External prompt appears live before completion');
    await current({ method: 'item/completed', params: externalPrompt });
    await page.waitForTimeout(100);
    assert.equal(await page.locator('[data-item-id="telegram-user-a"]').count(), 1, 'External prompt is deduplicated across start and completion');
    const foreignPrompt = { threadId: 'thread-b', turnId: 'telegram-turn-b', item: { id: 'telegram-user-b', type: 'userMessage', content: [{ type: 'text', text: 'Another session must stay private.' }] } };
    await current({ method: 'item/started', params: foreignPrompt });
    await current({ method: 'item/completed', params: foreignPrompt });
    await page.waitForTimeout(100);
    assert.equal(await page.locator('[data-item-id="telegram-user-b"]').count(), 0, 'External prompts from another thread remain hidden');
    if (await page.locator('#older').isVisible()) await page.locator('#older').click();
    await page.waitForSelector('[data-item-id="earliest-message"]');
    assert.equal(await page.locator('#messages article').first().getAttribute('data-item-id'), 'earliest-message');
    for (const [command, expectedCount] of [['/tghistory', 2], ['/tglastmessages', 1], ['/tghistory 3', 3]]) {
      await page.locator('#prompt').fill(command);
      await page.locator('#send').click();
      await page.waitForFunction(count => document.querySelectorAll('#command-content .command-history').length === count, expectedCount);
      const history = await page.locator('#command-content .command-history').allTextContents();
      assert.match(history.at(-1), /Ready to continue/, 'History ends with the most recent Codex response');
      for (const item of history) assert.match(item, /Turn time:.*\d.*\d:\d/, 'Every historical message includes a date and time');
      if (expectedCount === 3) assert.match(history[0], /The earlier prompt/, 'Explicit history counts preserve chronological order');
    }
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    // Suggestions are keyboard accessible and completion alone sends no RPC.
    await page.locator('#prompt').fill('/');
    await page.waitForSelector('#command-panel:not([hidden])');
    assert.ok(await page.locator('#command-suggestions .command-option').count() > 10, 'A slash opens the command catalog');
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-desktop-autocomplete.png') });
    await page.locator('#prompt').fill('/sta');
    await page.locator('#prompt').press('ArrowDown');
    await page.locator('#prompt').press('Tab');
    assert.match(await page.locator('#prompt').inputValue(), /^\/sta\w+\s*$/, 'Tab completes the selected command');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length), 0, 'Completing a suggestion does not execute it');
    await page.locator('#prompt').fill('/status');
    await page.locator('#prompt').press('Escape');
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'Escape closes the menu and keeps the draft');
    assert.equal(await page.locator('#prompt').inputValue(), '/status');
    await page.locator('#prompt').press('Enter');
    await page.waitForFunction(() => document.querySelector('#command-content').textContent.includes('Primary limit used: 26%'));
    assert.equal(await page.locator('#command-panel').isVisible(), true, 'Read-only status output stays open for reading');
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.method === 'gateway/command' && frame.params.name === 'status')), 'Status executes through the worker command bridge');
    for (const text of ['/not-a-command keep this text', '/voice']) {
      await page.locator('#prompt').fill(text);
      await page.locator('#send').click();
      await page.waitForFunction(() => !document.querySelector('#command-panel').hidden);
      assert.match(await page.locator('#command-content').textContent(), /unknown|not (?:a )?recognized|not available|not supported|native|voice/i, 'Unavailable commands explain their scope');
    }
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' || frame.method === 'turn/steer').length), 0, 'Unknown and terminal-only slash commands never become prompts');
    await current({ method: 'thread/settings/updated', params: { threadId: 'thread-a', threadSettings: { model: 'native-live-model', effort: 'medium' } } });
    await page.waitForFunction(() => document.querySelector('#model').textContent.includes('native-live-model') && document.querySelector('#effort').textContent === 'medium');
    await current({ method: 'thread/settings/updated', params: { threadId: 'thread-b', threadSettings: { model: 'wrong-thread-model', effort: 'low' } } });
    assert.match(await page.locator('#model').textContent(), /native-live-model/, 'A native settings event is scoped to its thread');
    await current({ method: 'thread/settings/updated', params: { threadId: 'thread-a', threadSettings: { model: 'codex-model', effort: 'high' } } });
    await page.locator('#prompt').fill('/model'); await page.locator('#send').click();
    const modelChoice = page.locator('#command-content').getByRole('button', { name: 'Codex model', exact: true });
    await modelChoice.waitFor();
    assert.equal(await modelChoice.getAttribute('aria-current'), 'true', 'The model menu marks the current session model');
    assert.equal(await modelChoice.getAttribute('data-active'), 'true', 'The current model begins as the keyboard selection');
    await modelChoice.press('Enter');
    const highChoice = page.locator('#command-content').getByRole('button', { name: 'High', exact: true });
    await highChoice.waitFor();
    assert.equal(await highChoice.getAttribute('aria-current'), 'true', 'The effort menu marks the current reasoning effort');
    await highChoice.press('Home');
    assert.equal(await page.locator('#command-content .command-choice[data-active="true"]').textContent().then(text => text.includes('Low')), true);
    await page.keyboard.press('End');
    assert.equal(await page.locator('#command-content').getByRole('button', { name: 'Back to models', exact: true }).getAttribute('data-active'), 'true', 'End selects the last menu row');
    await page.keyboard.press('ArrowUp');
    assert.equal(await highChoice.getAttribute('data-active'), 'true', 'ArrowUp moves between CLI-style menu choices');
    await page.keyboard.press('Escape');
    await modelChoice.waitFor();
    await modelChoice.press('Enter');
    await highChoice.waitFor(); await highChoice.press('1');
    await page.waitForFunction(() => document.querySelector('#effort').textContent === 'low');
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden);
    assert.equal(await page.locator('#command-content').textContent(), '', 'Final model selection closes and clears its result panel');
    await page.locator('#prompt').fill('/reasoning'); await page.locator('#send').click();
    const currentEffort = page.locator('#command-content .command-choice[aria-current="true"]');
    assert.match(await currentEffort.textContent(), /low/i, 'The reasoning command reflects the persisted slash selection');
    await currentEffort.press('ArrowDown');
    await page.keyboard.press('Space');
    await page.waitForFunction(() => document.querySelector('#effort').textContent === 'high');
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden);
    await page.locator('#prompt').fill('/tgsessions'); await page.locator('#send').click();
    const currentSessionChoice = page.locator('#command-content .command-choice[aria-current="true"]');
    await currentSessionChoice.waitFor();
    assert.match(await currentSessionChoice.textContent(), /Gateway workspace/, 'The session command marks the selected session');
    await currentSessionChoice.press('ArrowDown'); await page.keyboard.press('Enter'); await connected();
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('aria-current'), 'true', 'Session command rows support keyboard selection');
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'Selecting a session closes its command menu');
    await choose('a');
    await page.locator('#prompt').fill('/permissions');
    await page.locator('#send').click();
    await page.locator('#command-content').getByRole('button', { name: /^Full Access/ }).click();
    assert.equal(await page.locator('#command-panel').isVisible(), true, 'Full access remains open until its separate confirmation is accepted');
    await page.locator('#command-content').getByRole('button', { name: 'Enable Full Access', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden && !document.querySelector('#show-commands').disabled);
    assert.equal(await page.locator('#command-content').textContent(), '', 'Confirmed permissions close without retaining explanatory text');
    assert.deepEqual(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command' && frame.params.name === 'permissions').map(frame => frame.params.args || '')), ['', 'full-access', 'confirm-full-access'], 'Full access retains the worker’s separate confirmation step');
    await page.evaluate(() => { window.testRejectCommand = true; });
    await page.locator('#prompt').fill('/permissions read-only');
    await page.locator('#send').click();
    await page.waitForSelector('#command-content .command-result-error');
    assert.equal(await page.locator('#command-panel').isVisible(), true, 'Rejected settings remain visible so the error can be read');
    assert.match(await page.locator('#command-content').textContent(), /Setting rejected by runtime/);
    assert.equal(await page.locator('#prompt').inputValue(), '/permissions read-only', 'A rejected setting preserves the command draft');
    for (const command of ['/fast status', '/usage']) {
      await page.locator('#prompt').fill(command); await page.locator('#send').click();
      await page.waitForFunction(() => !document.querySelector('#show-commands').disabled && document.querySelector('#command-content').textContent.includes('completed.'));
      assert.equal(await page.locator('#command-panel').isVisible(), true, command + ' keeps read-only output open');
    }
    for (const command of ['/plan on', '/plan off', '/fast off', '/personality friendly', '/memories enabled', '/approvals on-request', '/goal pause']) {
      await page.locator('#prompt').fill(command); await page.locator('#send').click();
      await page.waitForFunction(() => document.querySelector('#command-panel').hidden && !document.querySelector('#show-commands').disabled);
      assert.equal(await page.locator('#command-content').textContent(), '', command + ' closes its successful setting result');
    }
    for (const command of ['/theme light', '/theme dark', '/theme system', '/statusline off', '/statusline on', '/title Temporary browser title', '/title reset', '/raw on', '/raw off']) {
      await page.locator('#prompt').fill(command); await page.locator('#send').click();
      assert.equal(await page.locator('#command-panel').isHidden(), true, command + ' closes after updating the browser setting');
    }
    await page.evaluate(() => { window.testHoldCommand = true; });
    await page.locator('#prompt').fill('/model codex-model low');
    await page.locator('#send').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldCommand));
    assert.equal(await page.locator('#show-commands').isDisabled(), true, 'Commands cannot replace a pending worker mutation');
    assert.equal(await page.locator('#close-commands').isDisabled(), true, 'A pending worker command cannot discard its result panel');
    await page.locator('#prompt').press('Escape');
    await page.locator('#show-commands').evaluate(button => button.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    assert.equal(await page.locator('#command-title').textContent(), '/model', 'Even a programmatic menu click cannot replace an in-flight result');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1);
      window.testHoldCommand = false;
      window.testPublishSettings('a', 'codex-model', 'low');
      socket.emit({ id: socket.heldCommand.id, result: { text: 'Model updated: codex-model low' } });
    });
    await page.waitForFunction(() => document.querySelector('#effort').textContent === 'low' && !document.querySelector('#show-commands').disabled);
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'The acknowledged setting closes after an attempted menu replacement');
    await page.evaluate(() => { window.testHoldCommand = true; delete window.testSockets.at(-1).heldCommand; });
    await page.locator('#prompt').fill('/model codex-model high'); await page.locator('#send').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldCommand));
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1); window.testHoldCommand = false;
      window.testPublishSettings('a', 'newer-terminal-model', 'low');
      socket.emit({ id: socket.heldCommand.id, result: { text: 'An earlier model choice was accepted.' } });
    });
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden && !document.querySelector('#show-commands').disabled);
    assert.match(await page.locator('#model').textContent(), /newer-terminal-model/, 'A delayed command acknowledgement cannot overwrite a newer setting notification');
    assert.equal(await page.locator('#effort').textContent(), 'low');
    await page.evaluate(() => window.testPublishSettings('a', 'codex-model', 'low'));
    const beforeDelete = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command' && frame.params.name === 'delete').length);
    await page.locator('#prompt').fill('/delete');
    await page.locator('#send').click();
    assert.match(await page.locator('#command-content').textContent(), /working directory and files will be kept/i, 'Delete explains which filesystem data is preserved');
    await page.locator('#command-content').getByRole('button', { name: 'Cancel', exact: true }).click();
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command' && frame.params.name === 'delete').length), beforeDelete, 'Cancelling deletion never sends the destructive command');
    await page.locator('#prompt').fill('/new');
    await page.locator('#send').click();
    assert.equal(await page.getByLabel('Existing working directory on this worker', { exact: true }).inputValue(), longPath, 'New sessions default to the selected workspace');
    await page.getByLabel('Session name', { exact: true }).fill('Session 12');
    await page.getByLabel('Session name', { exact: true }).press('ArrowLeft');
    await page.getByLabel('Session name', { exact: true }).press('3');
    assert.equal(await page.getByLabel('Session name', { exact: true }).inputValue(), 'Session 132', 'Command form inputs retain normal arrow and digit editing');
    await page.getByLabel('Session name', { exact: true }).fill('Do not create from a stale form');
    await page.locator('#command-content .command-form').evaluate(form => { window.staleCommandForm = form; window.staleCommandCancel = form.querySelector('button[type="button"]'); });
    const commandsBeforeStaleForm = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length);
    await page.getByRole('button', { name: 'Open command menu', exact: true }).click();
    await page.evaluate(() => window.staleCommandForm.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
    await page.evaluate(() => window.staleCommandCancel.click());
    assert.equal(await page.locator('#command-panel').isVisible(), true, 'A stale form Cancel cannot dismiss the new command menu');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length), commandsBeforeStaleForm, 'A detached old form cannot execute after another menu opens in the same session');
    assert.equal(await page.getByLabel('Find a command', { exact: true }).isVisible(), true, 'The stale form cannot replace the current menu');
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    // A newly created session is acknowledged before its durable inventory
    // arrives. Switching away while that inventory refresh waits must not let
    // the old success continuation close the new session's command menu.
    await page.locator('#prompt').fill('/new');
    await page.locator('#send').click();
    await page.getByLabel('Session name', { exact: true }).fill('Created while inventory waits');
    holdSessionInventory = true;
    const inventoryHeld = new Promise(resolve => { notifySessionInventoryHeld = resolve; });
    await page.locator('#command-content').getByRole('button', { name: 'Create session', exact: true }).click();
    await inventoryHeld;
    await choose('b');
    await page.getByRole('button', { name: 'Open command menu', exact: true }).click();
    await page.getByLabel('Find a command', { exact: true }).fill('tgstatus');
    holdSessionInventory = false;
    for (const route of heldSessionInventories.splice(0)) await route.fulfill({ status: authStatus, contentType: 'application/json', body: JSON.stringify({ sessions }) });
    await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.locator('#command-panel').isVisible(), true, 'A delayed new-session result cannot close another session’s menu');
    assert.equal(await page.getByLabel('Find a command', { exact: true }).inputValue(), 'tgstatus', 'The current command search survives an old inventory response');
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('aria-current'), 'true', 'The newly selected session stays selected');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command' && frame.params.name === 'new').length), 1, 'A delayed inventory response cannot replay session creation');
    await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    await choose('a');
    const commandsBeforeCD = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length);
    for (const command of ['/cd', '/cd /projects/another-directory']) {
      await page.locator('#prompt').fill(command);
      await page.locator('#send').click();
      assert.match(await page.locator('#command-content').textContent(), /working directory|workspace/i, 'Directory changes explain the runtime limitation');
    }
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length), commandsBeforeCD, 'Unsupported directory changes never reach the worker');
    assert.equal(await page.locator('#workspace-path').textContent(), longPath, 'An unavailable directory command leaves the current workspace intact');
    await page.locator('#refresh-sessions').click();
    await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
    for (const name of ['Renamed from the browser', 'Gateway workspace']) {
      await page.locator('#prompt').fill('/rename ' + name);
      await page.locator('#send').click();
      await page.waitForFunction(value => document.querySelector('#session-title').textContent === value, name);
      assert.equal(await page.locator('[data-session-id="a"] .name').textContent(), name, 'A confirmed rename updates a refreshed session inventory and the header');
      await page.waitForFunction(() => document.querySelector('#command-panel').hidden);
    }
    // The global feed mirrors changes made by Telegram, another browser or the
    // terminal, without waiting for this viewer to start a turn or reconnect.
    const connectionsBeforeSettings = await page.evaluate(() => window.testSockets.length);
    await page.evaluate(() => window.testPublishSettings('a', 'terminal-selected-model', 'medium'));
    await page.waitForFunction(() => document.querySelector('#model').textContent.includes('terminal-selected-model') && document.querySelector('#effort').textContent === 'medium');
    await page.locator('#refresh-sessions').click();
    await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
    assert.match(await page.locator('#model').textContent(), /terminal-selected-model/, 'Older inventory statistics cannot overwrite authoritative live model settings');
    assert.equal(await page.locator('#effort').textContent(), 'medium');
    await current({ method: 'thread/settings/updated', params: { threadId: 'thread-a', threadSettings: { model: 'older-native-frame', effort: 'low' } } });
    assert.match(await page.locator('#model').textContent(), /terminal-selected-model/, 'An unordered native frame cannot supersede confirmed settings from the global feed');
    await page.evaluate(() => { window.settingsBeforeFeedDrop = window.testActivitySocket; window.testActivitySocket.drop(1006); });
    await page.clock.fastForward(1100);
    await page.waitForFunction(() => window.testActivitySocket !== window.settingsBeforeFeedDrop && window.testActivitySocket.readyState === 1);
    assert.match(await page.locator('#model').textContent(), /terminal-selected-model/, 'The recovered global snapshot retains the confirmed model');
    assert.equal(await page.locator('#effort').textContent(), 'medium', 'Global reconnect retains the confirmed reasoning effort');
    await page.evaluate(() => window.testPublishSettings('b', 'telegram-background-model', 'low'));
    assert.match(await page.locator('#model').textContent(), /terminal-selected-model/, 'A background session setting cannot replace the selected session setting');
    assert.equal(await page.evaluate(() => window.testSockets.length), connectionsBeforeSettings, 'Live settings updates do not open extra transcript connections');
    await choose('b');
    assert.match(await page.locator('#model').textContent(), /telegram-background-model/, 'Selecting a background session restores its newest live model');
    assert.equal(await page.locator('#effort').textContent(), 'low', 'The saved live effort wins over an older native resume snapshot');
    await choose('a');
    assert.match(await page.locator('#model').textContent(), /terminal-selected-model/);
    await page.evaluate(() => { window.testHoldResume = true; });
    await page.locator('#disconnect').click(); await page.locator('#reconnect').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).held));
    await page.evaluate(() => {
      window.testPublishSettings('a', 'changed-during-resume', 'low');
      const socket = window.testSockets.at(-1); window.testHoldResume = false;
      socket.emit({ id: socket.held.id, result: { thread: { id: 'thread-a' }, model: 'old-resume-model', reasoningEffort: 'high' } });
    });
    await connected();
    await page.waitForFunction(() => document.querySelector('#model').textContent.includes('changed-during-resume'));
    assert.equal(await page.locator('#effort').textContent(), 'low', 'A late resume response cannot revert a newer live setting');
    await page.evaluate(() => window.testPublishSettings('a', 'codex-model', ''));
    await page.waitForFunction(() => /default/i.test(document.querySelector('#effort').textContent));
    assert.doesNotMatch(await page.locator('#effort').textContent(), /low|high|medium/, 'An authoritative default effort clears the previous explicit effort');
    await page.evaluate(() => { window.testPublishSettings('a', 'codex-model', 'high'); window.testPublishSettings('b', 'codex-model', 'high'); });
    await page.waitForFunction(() => document.querySelector('#effort').textContent === 'high');
    await page.locator('#prompt').fill('/permissions');
    await page.locator('#send').click();
    await page.locator('#command-content').getByRole('button', { name: /^Read Only/ }).waitFor();
    await page.locator('#command-content').getByRole('button', { name: /^Read Only/ }).evaluate(button => { window.stalePermissionButton = button; });
    const commandsBeforeSwitch = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length);
    await current({ method: 'item/completed', params: { threadId: 'thread-a', turnId: 'stale-question-a', item: { id: 'stale-async-question-a', type: 'agentMessage', delivery: 'async', text: 'An old question from session A.', questions: [{ title: 'Old async question from A?' }] } } });
    await page.getByLabel('Old async question from A?', { exact: true }).fill('Never send this answer to session B.');
    await page.getByLabel('Old async question from A?', { exact: true }).evaluate(field => { window.staleAsyncForm = field.closest('form'); });
    const answersBeforeSwitch = await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer', 'gateway/answer'].includes(frame.method)).length);
    await page.locator('#prompt').fill('A draft for session A');
    await page.evaluate(() => { window.oldHandler = window.testSockets.at(-1).onmessage; });
    await choose('b');
    await page.evaluate(() => window.staleAsyncForm.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer', 'gateway/answer'].includes(frame.method)).length), answersBeforeSwitch, 'A retained async question form cannot send an answer into another session');
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('aria-current'), 'true', 'Rejecting a stale answer leaves session B selected');
    assert.equal(await page.locator('#questions').isHidden(), true, 'A stale form cannot add or answer a question in session B');
    await page.evaluate(() => window.stalePermissionButton.click());
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command').length), commandsBeforeSwitch, 'An old permission button cannot change the newly selected session');
    await page.waitForFunction(() => document.querySelector('#send').textContent.includes('Steer'));
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(await page.locator('[data-item-id="reply-a"]').count(), 0, 'Switch clears old conversation');
    await page.evaluate(() => window.oldHandler({ data: JSON.stringify({ method: 'item/completed', params: { threadId: 'thread-a', item: { id: 'late-a', type: 'agentMessage', text: 'Late A must never appear' } } }) }));
    await page.waitForTimeout(100);
    assert.equal(await page.locator('[data-item-id="late-a"]').count(), 0, 'Late events cannot cross session boundaries');
    await current({ id: 'question-b', method: 'item/tool/requestUserInput', params: { threadId: 'thread-b', turnId: 'running-b', questions: [{ id: 'platform', question: 'Which machine should I test?', options: [{ label: 'Apple Silicon', description: 'The Mac' }, { label: 'Linux', description: 'The worker' }] }, { id: 'notes', question: 'Any extra notes?' }] } });
    await page.waitForSelector('[data-request-id="question-b"]');
    if (process.env.WEBUI_SCREENSHOTS) {
      await page.setViewportSize({ width: 390, height: 844 });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-question.png') });
      await page.setViewportSize({ width: 1440, height: 1000 });
    }
    await page.getByLabel('Apple Silicon — The Mac').check();
    await page.getByLabel('Any extra notes?').fill('Use the external display.');
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    const answer = await page.evaluate(() => window.testSent.find(frame => frame.id === 'question-b' && frame.result));
    assert.equal(answer.result.answers.platform.answers[0], 'Apple Silicon');
    assert.equal(answer.result.answers.notes.answers[0], 'Use the external display.');
    await current({ id: 'approval-b', method: 'item/commandExecution/requestApproval', params: { threadId: 'thread-b', turnId: 'running-b', command: 'go test ./...', availableDecisions: ['accept', 'decline'] } });
    await page.getByRole('button', { name: 'Allow once', exact: true }).click();
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.id === 'approval-b' && frame.result?.decision === 'accept')));
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    await page.evaluate(() => { window.testRejectAnswers = true; });
    await current({ id: 'rejected-b', method: 'item/tool/requestUserInput', params: { threadId: 'thread-b', turnId: 'running-b', questions: [{ id: 'retry', question: 'Keep this answer if rejected?' }] } });
    await page.getByLabel('Keep this answer if rejected?').fill('Keep my original text.');
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Answer rejected'));
    assert.equal(await page.getByLabel('Keep this answer if rejected?').inputValue(), 'Keep my original text.', 'Rejected RPC answers restore their form and text');
    await page.evaluate(() => { window.testRejectAnswers = false; });
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    // Async question items use the native quoted-title answer framing, not RPC IDs.
    await current({ method: 'item/completed', params: { threadId: 'thread-b', turnId: 'running-b', item: { id: 'async-b', type: 'agentMessage', delivery: 'async', text: 'Choose a display.', questions: [{ title: 'Which display?', options: ['External', 'Built-in'] }] } } });
    await page.getByLabel('External', { exact: true }).check();
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.method === 'turn/steer' && frame.params.input[0].text === '> Which display?\n\nExternal')), 'Async answers use native framing');
    await current({ method: 'item/completed', params: { threadId: 'thread-b', turnId: 'running-b', item: { id: 'async-complete-b', type: 'agentMessage', delivery: 'async', text: 'Question remains after the turn ends.', questions: [{ title: 'Do you see the stage?' }] } } });
    await current({ method: 'turn/completed', params: { threadId: 'thread-b', turn: { id: 'running-b', status: 'completed' } } });
    assert.equal(await page.getByLabel('Do you see the stage?').isVisible(), true, 'Async questions survive normal turn completion');
    await current({ method: 'item/completed', params: { threadId: 'thread-b', turnId: 'reply-b', item: { id: 'context-only', type: 'userMessage', content: [{ type: 'text', text: '<environment_context>runtime information</environment_context>' }] } } });
    assert.equal(await page.getByLabel('Do you see the stage?').isVisible(), true, 'Context-only history does not dismiss async questions');
    await current({ method: 'item/completed', params: { threadId: 'thread-b', turnId: 'reply-b', item: { id: 'external-answer', type: 'userMessage', content: [{ type: 'text', text: '> Do you see the stage?\n\nYes, on the external display.' }] } } });
    assert.equal(await page.locator('#questions').isHidden(), true, 'Explicit answer from another client resolves async question');
    await page.locator('#prompt').fill('B draft stays here');
    await page.locator('#disconnect').click();
    assert.match(await page.locator('#connection-text').textContent(), /Running work continues/);
    assert.equal(await page.locator('#send').isDisabled(), true);
    const beforeReconnect = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' || frame.method === 'turn/steer').length);
    await page.locator('#reconnect').click(); await connected();
    assert.equal(await page.locator('#prompt').inputValue(), 'B draft stays here');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' || frame.method === 'turn/steer').length), beforeReconnect, 'Reconnect never submits a draft');
    await choose('a');
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft for session A');
    await atTranscriptEnd('Switching sessions');
    await page.locator('#transcript').evaluate(scroller => { scroller.scrollTop = Math.max(0, scroller.scrollHeight - scroller.clientHeight - 180); });
    await choose('b'); await choose('a');
    await atTranscriptEnd('Returning to a previously scrolled session');
    await page.locator('#font-increase').click(); await atTranscriptEnd('Late font growth');
    await page.locator('#font-decrease').click(); await atTranscriptEnd('Restored font size');
    await current({ id: 'late-layout-question', method: 'item/tool/requestUserInput', params: { threadId: 'thread-a', turnId: 'new-a', questions: [{ id: 'late-layout', question: 'A question arrived after history was displayed?' }] } });
    await page.getByLabel('A question arrived after history was displayed?', { exact: true }).waitFor();
    await atTranscriptEnd('A late question changes the available conversation height');
    await current({ method: 'serverRequest/resolved', params: { requestId: 'late-layout-question' } });
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    await atTranscriptEnd('Question dismissal restores the conversation height');
    assert.equal(await page.locator('#model').evaluate(element => element.tagName), 'SPAN', 'The model status is read-only text');
    assert.equal(await page.locator('#effort').evaluate(element => element.tagName), 'SPAN', 'The reasoning status is read-only text');
    assert.equal(await page.locator('#session-status select').count(), 0, 'Status settings are selected through slash menus, not dropdowns');
    await page.locator('#prompt').fill('/model codex-model low');
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#effort').textContent === 'low');
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden);
    await page.locator('#prompt').fill('A confirmed prompt');
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#prompt').value === '');
    const sent = await page.evaluate(() => window.testSent.find(frame => frame.method === 'turn/start'));
    assert.equal(sent.params.model, undefined); assert.equal(sent.params.effort, undefined);
    assert.match(await page.locator('#model').textContent(), /codex-model/);
    assert.equal(await page.locator('#effort').textContent(), 'low', 'Prompt submission leaves native model and effort settings intact');
    await page.locator('#stop').click();
    await page.waitForFunction(() => document.querySelector('#stop').hidden);
    // Clipboard image drafts are visible, removable, and sent as native image
    // input alongside optional text. Neither selection nor paste sends a turn.
    const beforeImagePaste = await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length);
    const preview = await pasteImage('desktop-preview.png');
    assert.equal(preview.handled, true, 'Pasting an image is handled by the composer');
    await page.locator('#image-preview img').waitFor();
    assert.match(await page.locator('#image-preview img').getAttribute('src'), /^blob:/, 'Image previews use a local object URL');
    assert.match(await page.locator('#image-description').textContent(), /desktop-preview\.png/);
    await atTranscriptEnd('A late image preview expands the composer');
    await page.locator('#image-thumbnail').evaluate(image => { image.style.height = '120px'; });
    await atTranscriptEnd('A loaded image changes preview dimensions');
    await page.locator('#image-thumbnail').evaluate(image => image.style.removeProperty('height'));
    await atTranscriptEnd('Restored image preview dimensions');
    const nearBottomPosition = await page.locator('#transcript').evaluate(scroller => {
      scroller.dispatchEvent(new WheelEvent('wheel', { deltaY: -45, bubbles: true }));
      scroller.scrollTop = scroller.scrollHeight - scroller.clientHeight - 45;
      return scroller.scrollTop;
    });
    await page.locator('#image-thumbnail').evaluate(image => { image.style.height = '120px'; });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.ok(Math.abs(await page.locator('#transcript').evaluate(scroller => scroller.scrollTop) - nearBottomPosition) <= 2, 'A small deliberate upward scroll remains at the reader’s position when the footer grows');
    await page.locator('#image-thumbnail').evaluate(image => image.style.removeProperty('height'));
    await page.locator('#jump-latest').evaluate(button => button.click());
    await atTranscriptEnd('Explicitly returning to the latest message');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length), beforeImagePaste, 'Pasting an image does not submit it');
    await page.locator('#remove-image').click();
    assert.equal(await page.locator('#image-preview').isHidden(), true);
    assert.equal(await page.locator('#send').isDisabled(), true, 'Removing the only input disables empty submission');
    const textImage = await pasteImage('with-caption.png');
    await page.locator('#prompt').fill('Describe this pasted image.');
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#image-preview').hidden && document.querySelector('#prompt').value === '');
    const textImageFrame = await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).at(-1));
    assert.deepEqual(textImageFrame.params.input, [{ type: 'text', text: 'Describe this pasted image.' }, { type: 'image', url: textImage.dataURL }], 'Text and pasted PNG share one native prompt');
    await page.locator('#stop').click(); await page.waitForFunction(() => document.querySelector('#stop').hidden);
    const imageOnly = await pasteImage('image-only.png');
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#image-preview').hidden);
    assert.deepEqual(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).at(-1).params.input), [{ type: 'image', url: imageOnly.dataURL }], 'An image-only prompt is supported without fabricated text');
    await page.locator('#stop').click(); await page.waitForFunction(() => document.querySelector('#stop').hidden);
    await pasteImage('session-a-draft.png');
    await page.locator('#prompt').fill('Image draft belonging to A');
    await choose('b');
    assert.equal(await page.locator('#image-preview').isHidden(), true, 'Session A’s image is not exposed in B');
    await pasteImage('session-b-draft.png');
    await page.locator('#prompt').fill('Image draft belonging to B');
    await choose('a');
    assert.match(await page.locator('#image-description').textContent(), /session-a-draft\.png/);
    assert.equal(await page.locator('#prompt').inputValue(), 'Image draft belonging to A');
    await choose('b');
    assert.match(await page.locator('#image-description').textContent(), /session-b-draft\.png/);
    assert.equal(await page.locator('#prompt').inputValue(), 'Image draft belonging to B');
    await page.locator('#remove-image').click(); await page.locator('#prompt').fill('');
    await choose('a');
    await page.locator('#remove-image').click(); await page.locator('#prompt').fill('');
    // Paste and switch within one event-loop task, before image decoding can
    // finish. The pending image must remain attached to its original session.
    await pasteImage('paste-race-a.png', 'b'); await connected();
    assert.equal(await page.locator('[data-session-id="b"]').getAttribute('aria-current'), 'true');
    assert.equal(await page.locator('#image-preview').isHidden(), true, 'A pending image decode cannot populate a different session');
    await choose('a');
    assert.match(await page.locator('#image-description').textContent(), /paste-race-a\.png/);
    await page.locator('#remove-image').click();
    await pasteImage('send-race-a.png');
    await page.locator('#prompt').fill('Do not send after switching sessions.');
    await page.evaluate(() => { window.testHoldImageRead = true; });
    const beforeImageReadRace = await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length);
    await page.locator('#send').click();
    await page.waitForFunction(() => Boolean(window.testHeldImageRead));
    await choose('b');
    await page.evaluate(() => { window.testHoldImageRead = false; window.testHeldImageRead(); window.testHeldImageRead = null; });
    await page.waitForTimeout(100);
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length), beforeImageReadRace, 'A late image read cannot send into either session after navigation');
    assert.equal(await page.locator('#image-preview').isHidden(), true);
    await choose('a');
    assert.match(await page.locator('#image-description').textContent(), /send-race-a\.png/);
    assert.equal(await page.locator('#prompt').inputValue(), 'Do not send after switching sessions.');
    await page.locator('#remove-image').click(); await page.locator('#prompt').fill('');
    // Queued guidance is acknowledged temporarily above the input. A pending,
    // rejected or lost response must never claim the message was accepted.
    await choose('b');
    await page.evaluate(() => { window.testHoldSteer = true; });
    await page.locator('#prompt').fill('Guidance awaiting acknowledgement'); await page.locator('#send').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldSteer));
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'Queued guidance feedback waits for an accepted response');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1); window.testHoldSteer = false;
      socket.emit({ id: socket.heldSteer.id, result: {} });
    });
    await page.locator('#steer-notice').waitFor();
    assert.match(await page.locator('#steer-notice').textContent(), /Message queued/);
    assert.equal(await page.locator('#steer-notice').getAttribute('role'), 'status');
    assert.equal(await page.locator('#messages #steer-notice').count(), 0, 'Queue feedback does not enter conversation history');
    assert.equal(await page.locator('#steer-notice').evaluate(element => element.getBoundingClientRect().bottom <= document.querySelector('#composer .input-row').getBoundingClientRect().top), true, 'The ephemeral queue notice is above the input');
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-steer-queued.png') });
    await page.clock.fastForward(2500);
    await page.locator('#prompt').fill('More accepted guidance'); await page.locator('#send').click();
    await page.waitForFunction(() => !document.querySelector('#steer-notice').hidden && document.querySelector('#prompt').value === '');
    await page.clock.fastForward(2500);
    assert.equal(await page.locator('#steer-notice').isVisible(), true, 'A subsequent accepted steer restarts the feedback timer');
    await page.clock.fastForward(1501);
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'The accepted guidance notice automatically disappears after four seconds');
    await page.locator('#prompt').fill('/tgsteer Guidance from slash command'); await page.locator('#send').click();
    await page.locator('#steer-notice').waitFor();
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'Successful slash steering uses the same ephemeral notice and closes its command panel');
    await choose('a');
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'Switching sessions clears guidance feedback from the previous session');
    await choose('b');
    await page.locator('#prompt').fill('Accepted before turn completion'); await page.locator('#send').click();
    await page.locator('#steer-notice').waitFor();
    await current({ method: 'turn/completed', params: { threadId: 'thread-b', turn: { id: 'running-b', status: 'completed' } } });
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'A completed turn immediately removes its queued-message feedback');
    await current({ method: 'turn/started', params: { threadId: 'thread-b', turn: { id: 'steer-test-b', status: 'inProgress' } } });
    await page.evaluate(() => { window.testRejectSteer = true; });
    await page.locator('#prompt').fill('Rejected guidance'); await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Steering rejected by runtime'));
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'Rejected guidance has no success feedback');
    assert.equal(await page.locator('#prompt').inputValue(), 'Rejected guidance');
    await page.evaluate(() => { window.testDropSteer = true; });
    await page.locator('#prompt').fill('Uncertain steering delivery'); await page.locator('#send').click();
    await page.waitForSelector('#connection[data-state="reconnecting"]');
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'A lost steering acknowledgement has no success feedback');
    await page.evaluate(() => { window.testDropSteer = false; });
    await connected();
    assert.match(await page.locator('#notice').textContent(), /No input was resent/);
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'Reconnect does not manufacture a queue acknowledgement');
    assert.equal(await page.locator('#prompt').inputValue(), 'Uncertain steering delivery');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/steer' && frame.params.input[0].text === 'Uncertain steering delivery').length), 1, 'Uncertain guidance is never replayed');
    await page.locator('#prompt').fill('Accepted before disconnect'); await page.locator('#send').click();
    await page.locator('#steer-notice').waitFor();
    await page.locator('#disconnect').click();
    assert.equal(await page.locator('#steer-notice').isHidden(), true, 'Disconnect clears queued-message feedback');
    await choose('a');
    // A lost worker command acknowledgement is as unsafe to replay as a prompt.
    await page.evaluate(() => { window.testDropCommand = true; });
    await page.locator('#prompt').fill('/rename Uncertain rename');
    await page.locator('#send').click();
    await page.waitForSelector('#connection[data-state="reconnecting"]');
    await connected();
    assert.equal(await page.locator('#prompt').inputValue(), '/rename Uncertain rename', 'An unconfirmed command keeps its draft');
    assert.match(await page.locator('#command-content .command-result-error').textContent(), /No input was resent/, 'Successful automatic reconnect preserves a lost command acknowledgement warning');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command' && frame.params.name === 'rename' && frame.params.args === 'Uncertain rename').length), 1, 'Reconnect never repeats a worker command');
    await page.evaluate(() => { window.testDropCommand = false; });
    if (await page.locator('#command-panel').isVisible()) await page.getByRole('button', { name: 'Close command menu', exact: true }).click();
    // Lost submit acknowledgement: keep the draft and reconnect without a replay.
    await page.evaluate(() => { window.testDropSubmit = true; });
    await pasteImage('uncertain-image.png');
    await page.locator('#prompt').fill('An uncertain prompt');
    await page.locator('#send').click();
    await page.waitForSelector('#connection[data-state="reconnecting"]');
    assert.match(await page.locator('#notice').textContent(), /No input was resent/);
    await connected();
    assert.equal(await page.locator('#prompt').inputValue(), 'An uncertain prompt');
    assert.equal(await page.locator('#notice').isVisible(), true, 'Recovery does not hide a lost prompt acknowledgement warning');
    assert.match(await page.locator('#notice').textContent(), /No input was resent/, 'The uncertain submission still requires an explicit user decision after reconnect');
    assert.match(await page.locator('#image-description').textContent(), /uncertain-image\.png/, 'A lost acknowledgement preserves the unsent image for explicit recovery');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' && frame.params.input[0].text === 'An uncertain prompt').length), 1);
    await page.evaluate(() => { window.testDropSubmit = false; });
    await page.locator('#remove-image').click();
    // A failed resume must not enable prompt submission.
    await page.locator('#disconnect').click();
    await page.evaluate(() => { window.testFailResume = true; });
    await page.locator('#reconnect').click();
    await page.waitForSelector('#connection[data-state="error"]');
    assert.equal(await page.locator('#send').isDisabled(), true);
    assert.match(await page.locator('#notice').textContent(), /Thread unavailable/, 'A failed bootstrap keeps its error visible');
    await page.evaluate(() => { window.testFailResume = false; window.testHoldResume = true; });
    await page.locator('#reconnect').click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).held));
    assert.match(await page.locator('#notice').textContent(), /Thread unavailable/, 'Clicking reconnect and receiving ready cannot prematurely clear a failed bootstrap');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1); window.testHoldResume = false;
      socket.emit({ id: socket.held.id, result: { thread: { id: 'thread-a' }, model: 'codex-model', reasoningEffort: 'high' } });
    });
    await connected();
    assert.equal(await page.locator('#notice').isHidden(), true, 'A successfully reloaded conversation clears its resolved bootstrap error');
    await page.waitForFunction(() => document.querySelector('#model').textContent.includes('codex-model'));
    // Gateway errors are recovered only once the selected conversation has
    // resumed and its history loaded, rather than when the transport is ready.
    await current({ type: 'error', message: 'Worker gateway connection unavailable' });
    assert.match(await page.locator('#notice').textContent(), /Worker gateway connection unavailable/);
    await page.evaluate(() => { window.testHoldResume = true; window.testHoldInitialItems = true; window.testSockets.at(-1).drop(1006); });
    await page.waitForSelector('#connection[data-state="reconnecting"]');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).held));
    assert.equal(await page.locator('#notice').isVisible(), true);
    assert.match(await page.locator('#notice').textContent(), /Worker gateway connection unavailable/, 'The gateway banner remains while automatic reconnect is still resuming the thread');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1); window.testHoldResume = false;
      socket.emit({ id: socket.held.id, result: { thread: { id: 'thread-a' }, model: 'codex-model', reasoningEffort: 'high' } });
    });
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldInitialItems));
    assert.equal(await page.locator('#send').isDisabled(), true, 'An open transport with history pending is not a recovered session');
    assert.match(await page.locator('#notice').textContent(), /Worker gateway connection unavailable/, 'A successful resume alone cannot clear the gateway error');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1), reply = socket.heldInitialItems; window.testHoldInitialItems = false;
      socket.emit({ id: reply.frame.id, result: reply.result });
    });
    await connected();
    assert.equal(await page.locator('#notice').isHidden(), true, 'Automatic reconnect clears the gateway error only after history succeeds');
    assert.equal(await page.locator('#notice').textContent(), '', 'A resolved connection banner is removed from the DOM content');
    await current({ type: 'error', message: 'Waiting for worker recovery' });
    await page.evaluate(() => { window.testFailInitialItems = true; });
    await page.locator('#disconnect').click(); await page.locator('#reconnect').click();
    await page.waitForSelector('#connection[data-state="error"]');
    assert.match(await page.locator('#notice').textContent(), /Initial conversation history unavailable/, 'A history failure keeps bootstrap in an error state');
    await page.locator('#reconnect').click(); await connected();
    assert.equal(await page.locator('#notice').isHidden(), true, 'Successful bootstrap retries clear initial history errors');
    await current({ method: 'turn/started', params: { threadId: 'thread-a', turn: { id: 'retry-recovery-a', status: 'inProgress' } } });
    await current({ method: 'error', params: { threadId: 'thread-a', turnId: 'retry-recovery-a', willRetry: true, error: { message: 'Retryable network interruption' } } });
    assert.match(await page.locator('#notice').textContent(), /Retryable network interruption/);
    await page.evaluate(() => { const socket = window.testActivitySocket; socket.emit({ type: 'heartbeat', version: 2, sequence: socket.sequence }); });
    assert.equal(await page.locator('#notice').isVisible(), true, 'An activity heartbeat cannot prove a native turn retry recovered');
    await current({ method: 'item/agentMessage/delta', params: { threadId: 'thread-a', turnId: 'unrelated-turn', itemId: 'unrelated-retry-item', delta: 'Unrelated progress' } });
    assert.match(await page.locator('#notice').textContent(), /Retryable network interruption/, 'Progress from another turn cannot clear a retry error');
    await current({ method: 'item/agentMessage/delta', params: { threadId: 'thread-a', turnId: 'retry-recovery-a', itemId: 'recovered-retry-item', delta: 'The active turn resumed producing text.' } });
    assert.equal(await page.locator('#notice').isHidden(), true, 'Matching assistant progress resolves a retryable native network error');
    await current({ method: 'turn/completed', params: { threadId: 'thread-a', turn: { id: 'retry-recovery-a', status: 'completed' } } });
    await current({ method: 'error', params: { threadId: 'thread-a', error: { message: 'Runtime build failure needs attention' } } });
    await page.locator('#disconnect').click(); await page.locator('#reconnect').click(); await connected();
    assert.match(await page.locator('#notice').textContent(), /Runtime build failure needs attention/, 'Unrelated runtime errors survive a successful manual reconnect');
    assert.equal(await page.locator('#notice').isVisible(), true);
    await choose('b'); await choose('a');
    // Deep quote nesting is bounded and stays literal beyond the supported depth.
    await page.evaluate(() => { const area = document.createElement('div'); area.id = 'deep-quote-test'; area.append(window.CodexFormat.markdown('>'.repeat(5000) + ' safe')); document.querySelector('#messages').append(area); });
    assert.equal(await page.locator('#messages blockquote').count(), 13);
    await page.locator('#deep-quote-test').evaluate(element => element.remove());
    // Responsive rendering must fit both phone widths, including long workspace paths.
    await page.setViewportSize({ width: 850, height: 844 });
    assert.equal(await page.locator('#font-increase').isVisible(), false, 'Desktop font controls are hidden at the tablet breakpoint');
    assert.equal(await page.locator('#font-decrease').isVisible(), false);
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 });
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Mobile must not overflow at ' + width);
      assert.equal(await page.locator('#model').isVisible(), true);
      assert.equal(await page.locator('#effort').isVisible(), true);
      assert.equal(await page.locator('#font-increase').isVisible(), false);
      assert.equal(await conversationFont(), 13, 'Desktop font preferences do not enlarge the mobile conversation');
      assert.equal(await promptFont(), 13, 'Mobile prompt keeps the conversation font size');
      await page.waitForFunction(() => document.querySelector('#rate-limits .rate-limit'));
      const mobileStatus = await page.locator('#session-status').evaluate(element => {
        const transcript = document.querySelector('#transcript'), originalTop = transcript.scrollTop;
        const originallyAtEnd = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight <= 2;
        const box = target => { const rect = target.getBoundingClientRect(); return { top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right, height: rect.height }; };
        const model = element.querySelector('.model-controls'), limits = element.querySelector('#rate-limits');
        const geometry = { status: box(element), model: box(model), turn: box(element.querySelector('#turn-state')), usage: box(element.querySelector('#usage')), limits: box(limits), buckets: [...limits.querySelectorAll('.rate-limit')].map(box), rowGap: parseFloat(getComputedStyle(element).rowGap) };
        const children = [...limits.childNodes]; limits.replaceChildren();
        geometry.emptyLimits = { height: box(element).height, visible: getComputedStyle(limits).display !== 'none' };
        limits.append(...children);
        element.classList.add('hide-details');
        geometry.hiddenDetails = { height: box(element).height, modelHeight: box(model).height, turnVisible: getComputedStyle(element.querySelector('#turn-state')).display !== 'none', limitsVisible: getComputedStyle(limits).display !== 'none' };
        element.classList.remove('hide-details');
        // These CSS probes temporarily change layout twice within one task.
        // Restore any browser-clamped scroll without simulating a user scroll.
        transcript.scrollTop = originallyAtEnd ? transcript.scrollHeight : originalTop;
        return geometry;
      });
      assert.ok(Math.abs(mobileStatus.model.top - mobileStatus.turn.top) < 2, width + 'px status aligns model/effort and turn state on the first row');
      assert.ok(Math.abs(mobileStatus.turn.right - mobileStatus.status.right) < 2, width + 'px turn state aligns to the right edge');
      assert.ok(mobileStatus.usage.top >= Math.max(mobileStatus.model.bottom, mobileStatus.turn.bottom), width + 'px context usage starts on the second row');
      assert.ok(Math.abs(mobileStatus.usage.left - mobileStatus.status.left) < 2, width + 'px context usage aligns to the left edge');
      assert.ok(Math.abs(mobileStatus.usage.top - mobileStatus.limits.top) < 2, width + 'px context percentage and usage limits share the second row');
      assert.ok(Math.abs(mobileStatus.limits.right - mobileStatus.status.right) < 2, width + 'px usage limits align to the right edge');
      assert.equal(mobileStatus.buckets.length, 2, 'The mobile status shows both five-hour and weekly quotas');
      assert.ok(Math.abs(mobileStatus.buckets[0].top - mobileStatus.buckets[1].top) < 2, width + 'px quota labels stay on the same line');
      assert.ok(mobileStatus.buckets.every(bucket => bucket.height <= mobileStatus.usage.height + 2), width + 'px quota text does not wrap');
      assert.ok(Math.abs(mobileStatus.status.height - mobileStatus.model.height - mobileStatus.usage.height - mobileStatus.rowGap) < 2, width + 'px status is exactly two text lines high');
      assert.equal(await page.locator('#usage').textContent(), '9% context', 'Mobile context usage has no token count');
      assert.equal(mobileStatus.emptyLimits.visible, false, 'Empty mobile limits are hidden');
      assert.ok(Math.abs(mobileStatus.emptyLimits.height - mobileStatus.status.height) < 2, 'Context usage preserves the second row when limits are empty');
      assert.equal(mobileStatus.hiddenDetails.turnVisible, false, 'Hidden status details remove the state row');
      assert.equal(mobileStatus.hiddenDetails.limitsVisible, false, 'Hidden status details remove the limit row');
      assert.ok(Math.abs(mobileStatus.hiddenDetails.height - mobileStatus.hiddenDetails.modelHeight) < 2, 'Hidden mobile status retains only the model row');
      for (const scheme of ['dark', 'light']) {
        await page.emulateMedia({ colorScheme: scheme });
        await assertToolColors(width + 'px mobile ' + scheme + ' theme');
      }
      await page.emulateMedia({ colorScheme: 'dark' });
      if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-status-' + width + '.png') });
    }
    await page.setViewportSize({ width: 390, height: 844 });
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-status-aligned.png') });
    await atTranscriptEnd('Mobile viewport before opening the model menu');
    await page.evaluate(() => {
      window.testScrollTrace = [];
      const transcript = document.querySelector('#transcript');
      const trace = type => {
        window.testScrollTrace.push({ type, time: Math.round(performance.now()), top: transcript.scrollTop, height: transcript.scrollHeight, visible: transcript.clientHeight, menu: !document.querySelector('#command-panel').hidden, preview: !document.querySelector('#image-preview').hidden });
        if (window.testScrollTrace.length > 100) window.testScrollTrace.shift();
      };
      for (const type of ['scroll', 'wheel', 'touchstart', 'touchmove', 'keydown']) transcript.addEventListener(type, () => trace(type));
      const resized = new ResizeObserver(entries => trace('resize:' + entries.map(entry => entry.target.id).join(',')));
      for (const id of ['transcript', 'composer', 'command-panel', 'image-preview']) resized.observe(document.getElementById(id));
      new MutationObserver(entries => trace('change:' + entries.map(entry => entry.target.id).join(','))).observe(document.querySelector('#command-panel'), { attributes: true, attributeFilter: ['hidden'] });
      trace('start');
    });
    await touchEmulation.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 1 });
    await page.locator('#prompt').fill('/mod');
    await touchTap(page.locator('#command-suggestions .command-option').first());
    await touchTap(page.locator('#command-content').getByRole('button', { name: 'Codex model', exact: true }));
    await page.locator('#command-content').getByRole('button', { name: 'High', exact: true }).waitFor();
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-model-menu.png') });
    await touchTap(page.locator('#command-content').getByRole('button', { name: 'High', exact: true }));
    await page.waitForFunction(() => document.querySelector('#command-panel').hidden && !document.querySelector('#show-commands').disabled);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Mobile model and effort menus fit the screen');
    assert.equal(await page.locator('#prompt').evaluate(element => element === document.activeElement), false, 'Completed mobile selection does not reopen the keyboard');
    await page.locator('#prompt').fill('');
    const mobileImage = await pasteImage('mobile-clipboard.png');
    await page.locator('#image-preview img').waitFor();
    await atTranscriptEnd('Mobile clipboard image preview');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'A mobile image preview fits the viewport');
    if (process.env.WEBUI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile-image-preview.png') });
    await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#image-preview').hidden);
    assert.deepEqual(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).at(-1).params.input), [{ type: 'image', url: mobileImage.dataURL }], 'Mobile clipboard images use the same native image-only input');
    await page.locator('#stop').click(); await page.waitForFunction(() => document.querySelector('#stop').hidden);
    await page.locator('#show-sessions').click();
    assert.equal(await page.locator('[data-session-id="b"]').isVisible(), true);
    assert.match(await page.locator('[data-session-id="b"] .name').textContent(), /Second session/);
    await page.locator('#close-sessions').click();
    // The visible viewport can pan after the keyboard resize without changing
    // height again. Verify actual element geometry, not implementation styles.
    const submissionsBeforeKeyboard = await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' || frame.method === 'turn/steer').length);
    const viewportBounds = async selectors => page.evaluate(selectors => {
      const viewport = visualViewport;
      return selectors.map(selector => {
        const rect = document.querySelector(selector).getBoundingClientRect();
        const visibleTop = viewport.pageTop - scrollY;
        const visibleLeft = viewport.pageLeft - scrollX;
        return { selector, top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right, visibleTop, visibleBottom: visibleTop + viewport.height, visibleLeft, visibleRight: visibleLeft + viewport.width };
      });
    }, selectors);
    const assertInViewport = async (selectors, context) => {
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      for (const rect of await viewportBounds(selectors)) {
        assert.ok(rect.top >= rect.visibleTop - 1 && rect.bottom <= rect.visibleBottom + 1, `${context}: ${rect.selector} must remain vertically visible: ${JSON.stringify(rect)}`);
        assert.ok(rect.left >= rect.visibleLeft - 1 && rect.right <= rect.visibleRight + 1, `${context}: ${rect.selector} must remain horizontally visible: ${JSON.stringify(rect)}`);
      }
    };
    const keyboardViewport = { width: 390, height: 430, offsetTop: 0, offsetLeft: 0 };
    await page.locator('#prompt').fill('Keep this draft while opening the keyboard.');
    await page.evaluate(values => window.testViewport(values), keyboardViewport);
    await assertInViewport(['.topbar', '#composer', '#prompt'], 'Keyboard resize');
    keyboardViewport.offsetTop = 164;
    await page.evaluate(values => window.testViewport(values, 'scroll'), keyboardViewport);
    await assertInViewport(['.topbar', '#composer', '#prompt'], 'Keyboard pan without resize');
    const transcript = page.locator('#transcript');
    await transcript.evaluate(element => { element.scrollTop = 0; });
    const transcriptBox = await transcript.boundingBox();
    await page.mouse.move(transcriptBox.x + transcriptBox.width / 2, transcriptBox.y + transcriptBox.height / 2);
    await page.mouse.wheel(0, 160);
    await page.waitForFunction(() => document.querySelector('#transcript').scrollTop > 0);
    await assertInViewport(['.topbar', '#composer'], 'Native conversation scrolling');
    assert.equal(await page.evaluate(() => window.scrollY), 0, 'Conversation scrolling must not scroll the page');
    assert.equal(await page.locator('#prompt').inputValue(), 'Keep this draft while opening the keyboard.');
    assert.equal(await page.locator('#prompt').evaluate(element => getComputedStyle(element).fontSize), await page.locator('#messages').evaluate(element => getComputedStyle(element).fontSize), 'Prompt keeps the conversation font size during keyboard focus');
    // Safari may move the layout viewport as well as the visual viewport. Use
    // real document scrolling so bounds are checked in the correct coordinate
    // space; offsetTop alone is intentionally different from pageTop here.
    await page.evaluate(() => {
      document.documentElement.style.minHeight = '1800px';
      document.body.style.minHeight = '1800px';
      scrollTo(0, 140);
    });
    await page.waitForFunction(() => scrollY === 140);
    assert.equal(await page.evaluate(() => visualViewport.pageTop - visualViewport.offsetTop), 140, 'Keyboard model includes a distinct document scroll offset');
    await assertInViewport(['.topbar', '#composer', '#prompt'], 'Combined page scroll and visual viewport pan');
    // Safari focus metrics can settle after its last resize/scroll event. A
    // delayed metric change deliberately emits no event and must still settle.
    await page.evaluate(() => {
      window.testPromptStyleMutations = 0;
      window.testPromptObserver = new MutationObserver(records => { window.testPromptStyleMutations += records.length; });
      window.testPromptObserver.observe(document.querySelector('#prompt'), { attributes: true, attributeFilter: ['style'] });
    });
    await page.locator('#prompt').evaluate(element => { element.blur(); element.focus({ preventScroll: true }); });
    await page.waitForTimeout(120);
    keyboardViewport.height = 404;
    keyboardViewport.offsetTop = 76;
    await page.evaluate(values => window.testViewport(values, null), keyboardViewport);
    await page.waitForFunction(() => {
      const viewport = visualViewport;
      const header = document.querySelector('.topbar').getBoundingClientRect();
      const composer = document.querySelector('#composer').getBoundingClientRect();
      const visibleTop = viewport.pageTop - scrollY;
      return Math.abs(header.top - visibleTop) <= 1 && composer.bottom <= visibleTop + viewport.height + 1;
    }, null, { timeout: 1200 });
    await assertInViewport(['.topbar', '#composer', '#prompt'], 'Delayed focus metrics without another viewport event');
    assert.equal(await page.locator('#prompt').inputValue(), 'Keep this draft while opening the keyboard.');
    await page.waitForTimeout(1100);
    assert.ok(await page.evaluate(() => window.testPromptStyleMutations <= 2), 'Focus stabilization must not repeatedly rewrite textarea geometry');
    await page.evaluate(() => window.testPromptObserver.disconnect());
    const framesAfterSettling = await page.evaluate(() => window.testAnimationFrames);
    await page.waitForTimeout(180);
    assert.equal(await page.evaluate(() => window.testAnimationFrames), framesAfterSettling, 'Viewport polling must stop after focus settles');
    await page.evaluate(() => {
      scrollTo(0, 0);
      document.documentElement.style.removeProperty('min-height');
      document.body.style.removeProperty('min-height');
    });
    await page.waitForFunction(() => scrollY === 0);
    await current({ id: 'keyboard-question', method: 'item/tool/requestUserInput', params: { threadId: 'thread-a', turnId: 'keyboard-test', questions: [{ id: 'notes', question: 'Keyboard test answer?' }] } });
    await page.getByLabel('Keyboard test answer?').fill('Keep the question answer too.');
    keyboardViewport.offsetTop = 96;
    await page.evaluate(values => window.testViewport(values, 'scroll'), keyboardViewport);
    await assertInViewport(['.topbar', '#questions', '#composer'], 'Question focus');
    assert.equal(await page.getByLabel('Keyboard test answer?').inputValue(), 'Keep the question answer too.');
    await current({ method: 'serverRequest/resolved', params: { requestId: 'keyboard-question' } });
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    await page.locator('#prompt').focus();
    await page.evaluate(() => {
      window.testSessionSearchFocuses = 0;
      document.querySelector('#session-search').addEventListener('focus', () => { window.testSessionSearchFocuses++; });
      // Mobile tapping does not consistently transfer focus to buttons. The
      // menu must explicitly dismiss the existing input in that case too.
      document.querySelector('#show-sessions').click();
    });
    assert.equal(await page.evaluate(() => document.activeElement?.matches('input,textarea,[contenteditable=true]') || false), false, 'Opening the session menu blurs the prompt even when the button does not take focus');
    assert.equal(await page.evaluate(() => window.testSessionSearchFocuses), 0, 'The mobile session menu never briefly autofocuses search');
    await page.locator('#session-search').click();
    assert.equal(await page.locator('#session-search').evaluate(element => document.activeElement === element), true, 'Explicitly tapping session search still opens its editable input');
    await page.locator('#session-search').fill('Gateway');
    keyboardViewport.offsetTop = 128;
    await page.evaluate(values => window.testViewport(values, 'scroll'), keyboardViewport);
    await assertInViewport(['#sidebar', '#sidebar-backdrop', '#session-search'], 'Session search focus');
    assert.equal(await page.locator('#session-search').inputValue(), 'Gateway');
    await page.locator('#session-search').fill('');
    await page.locator('#close-sessions').evaluate(button => button.click());
    assert.equal(await page.locator('#session-search').evaluate(element => document.activeElement === element), false, 'Closing the mobile session menu blurs search and dismisses its keyboard');
    await page.locator('#show-sessions').evaluate(button => button.click());
    assert.equal(await page.locator('#session-search').evaluate(element => document.activeElement === element), false, 'Reopening the session menu leaves search unfocused');
    await page.locator('#close-sessions').click();
    await page.locator('#prompt').evaluate(element => element.blur());
    await page.evaluate(() => window.testViewport({}));
    await assertInViewport(['.topbar', '#composer'], 'Keyboard dismissal');
    assert.ok(Math.abs((await page.locator('.topbar').boundingBox()).y) <= 1, 'Header returns to the screen top after keyboard dismissal');
    assert.equal(await page.locator('#prompt').inputValue(), 'Keep this draft while opening the keyboard.');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'turn/start' || frame.method === 'turn/steer').length), submissionsBeforeKeyboard, 'Viewport and focus changes never submit or replay prompts');
    await page.locator('#prompt').fill('');
    if (process.env.WEBUI_SCREENSHOTS) {
      fs.mkdirSync(process.env.WEBUI_SCREENSHOTS, { recursive: true });
      await page.locator('#transcript').evaluate(element => { element.scrollTop = element.scrollHeight; });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mobile.png') });
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-desktop.png') });
    }
    // Pages are bounded by rendered transcript items, including tools and
    // summaries: a single 45-item turn must not fill the initial viewport.
    await page.setViewportSize({ width: 1440, height: 1000 });
    await choose('c');
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 20);
    const historyIDs = () => page.locator('#messages article').evaluateAll(items => items.map(item => item.dataset.itemId));
    const historyDiagnostics = async expected => {
      const diagnostics = await page.evaluate(() => {
        const scroller = document.querySelector('#transcript');
        const articles = [...document.querySelectorAll('#messages article')];
        return { count: articles.length, first: articles[0]?.dataset.itemId, last: articles.at(-1)?.dataset.itemId,
          scroll: { top: scroller.scrollTop, height: scroller.scrollHeight, visible: scroller.clientHeight },
          older: { hidden: document.querySelector('#older').hidden, disabled: document.querySelector('#older').disabled },
          connection: document.querySelector('#connection').dataset.state, questionsHidden: document.querySelector('#questions').hidden,
          paging: window.testSent.filter(frame => frame.method === 'thread/items/list').slice(-8).map(frame => ({ session: frame.session, thread: frame.params.threadId, cursor: frame.params.cursor || null, limit: frame.params.limit })),
          held: !!window.testSockets.at(-1)?.heldOlderItems, trace: window.testScrollTrace?.slice(-20) };
      });
      console.error('History pagination diagnostics:', JSON.stringify({ expected, ...diagnostics }));
    };
    const waitHistoryCount = async expected => {
      try { await page.waitForFunction(count => document.querySelectorAll('#messages article').length === count, expected); }
      catch (error) { await historyDiagnostics(expected); throw error; }
    };
    assert.deepEqual(await historyIDs(), Array.from({ length: 20 }, (_, index) => 'history-' + (index + 65)), 'The initial page contains the latest 20 transcript items in chronological order');
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.method === 'thread/items/list' && frame.params.threadId === 'thread-c' && frame.params.limit === 20 && frame.params.sortDirection === 'desc')), 'Transcript paging uses the native 20-item history API');
    assert.ok(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'thread/turns/list' && frame.params.threadId === 'thread-c').every(frame => frame.params.itemsView === 'notLoaded' && frame.params.limit === 20)), 'Initial turn reads fetch bounded metadata only, without reloading historical turn bodies');
    await page.getByLabel('Pending question outside the latest 20 items?', { exact: true }).waitFor();
    const questionReads = await page.evaluate(() => window.testSent.filter(frame => frame.session === 'c' && frame.method === 'gateway/questions').length);
    await activity(activitySnapshot.map(session => session.session_id === 'c' ? { ...session, pending_questions: 1, pending_revision: 'first-pending-question' } : session));
    await page.waitForFunction(count => window.testSent.filter(frame => frame.session === 'c' && frame.method === 'gateway/questions').length > count, questionReads);
    await page.evaluate(() => { window.testReplacementQuestion = true; });
    await activity(activitySnapshot.map(session => session.session_id === 'c' ? { ...session, pending_questions: 1, pending_revision: 'replacement-pending-question' } : session));
    await page.getByLabel('Replacement question with the same pending count?', { exact: true }).fill('Recovered from the worker snapshot.');
    assert.equal(await page.getByLabel('Pending question outside the latest 20 items?', { exact: true }).count(), 0, 'A changed pending revision replaces the old question even when the count stays at one');
    // An authoritative refresh can occur while the answer is awaiting an RPC
    // result. Keep that request and its draft recoverable if the answer fails.
    await page.evaluate(() => { window.testHoldGatewayAnswer = true; });
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldGatewayAnswer));
    await page.evaluate(() => { window.pendingAnswerCard = document.querySelector('[data-request-id="worker:approval-replacement-c"]'); });
    await activity(activitySnapshot.map(session => session.session_id === 'c' ? { ...session, pending_questions: 1, pending_revision: 'refreshed-during-answer' } : session));
    await page.waitForFunction(() => document.querySelector('[data-request-id="worker:approval-replacement-c"]') !== window.pendingAnswerCard);
    assert.equal(await page.locator('[data-request-id="worker:approval-replacement-c"]').getAttribute('class'), 'question-card answered', 'The refreshed request remains pending until its answer RPC resolves');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1);
      socket.emit({ id: socket.heldGatewayAnswer.id, error: { message: 'Worker answer rejected after refresh' } });
      socket.heldGatewayAnswer = null; window.testHoldGatewayAnswer = false;
    });
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Worker answer rejected after refresh'));
    assert.equal(await page.getByLabel('Replacement question with the same pending count?', { exact: true }).inputValue(), 'Recovered from the worker snapshot.', 'A failed answer restores its draft after a same-ID snapshot refresh');
    assert.equal(await page.getByRole('button', { name: 'Send answer', exact: true }).isDisabled(), false, 'A failed answer restores usable controls after a same-ID snapshot refresh');
    await page.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#questions').hidden);
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.method === 'gateway/answer' && frame.params.approvalId === 'approval-replacement-c' && frame.params.result.answers['question-c'].answers[0] === 'Recovered from the worker snapshot.')), 'The replacement question outside the visible history is answered through the worker');
    assert.equal(await page.locator('#messages article').count(), 20, 'Closing the question panel does not automatically fetch older history');
    for (const count of [40, 60]) {
      // Use a real reader gesture. Assigning scrollTop alone after the question
      // panel collapses can race its layout-driven bottom-follow animation and
      // never represents the synchronous wheel/key/touch intent in the UI.
      // Hold the reply so the anchor is captured before a fast mock can prepend.
      await page.evaluate(() => { window.testHoldOlderItems = true; });
      await page.locator('#transcript').focus();
      await page.keyboard.press('Home');
      await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldOlderItems)).catch(async error => {
        await historyDiagnostics(count); throw error;
      });
      await page.waitForFunction(() => document.querySelector('#transcript').scrollTop === 0);
      await page.locator('#transcript').evaluate(scroller => {
        const first = document.querySelector('#messages article');
        window.historyAnchor = { id: first.dataset.itemId, top: first.getBoundingClientRect().top };
      });
      await page.evaluate(() => { const socket = window.testSockets.at(-1), reply = socket.heldOlderItems; window.testHoldOlderItems = false; socket.heldOlderItems = null; socket.emit({ id: reply.frame.id, result: reply.result }); });
      await waitHistoryCount(count);
      assert.deepEqual(await historyIDs(), Array.from({ length: count }, (_, index) => 'history-' + (index + 85 - count)), 'Scrolling to the top adds exactly 20 older items without reordering or duplicates');
      const drift = await page.evaluate(() => document.querySelector('[data-item-id="' + window.historyAnchor.id + '"]').getBoundingClientRect().top - window.historyAnchor.top);
      assert.ok(Math.abs(drift) < 3, 'Prepending older history preserves the reading anchor instead of jumping: ' + drift);
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    }
    // The reader can move away from the top while an older page is in flight.
    // Preserve the latest visible anchor, not the position at request time.
    await page.evaluate(() => { window.testHoldOlderItems = true; });
    await page.locator('#transcript').evaluate(scroller => { scroller.scrollTop = 0; });
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldOlderItems));
    await page.locator('#transcript').evaluate(scroller => {
      scroller.scrollTop = Math.floor(scroller.scrollHeight / 2);
      const visible = [...document.querySelectorAll('#messages article')].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
      window.movingHistoryAnchor = { id: visible.dataset.itemId, top: visible.getBoundingClientRect().top };
    });
    await page.evaluate(() => { const socket = window.testSockets.at(-1); const held = socket.heldOlderItems; socket.emit({ id: held.frame.id, result: held.result }); socket.heldOlderItems = null; });
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 80);
    const movingDrift = await page.evaluate(() => document.querySelector('[data-item-id="' + window.movingHistoryAnchor.id + '"]').getBoundingClientRect().top - window.movingHistoryAnchor.top);
    assert.ok(Math.abs(movingDrift) < 3, 'A delayed page preserves the reader’s latest anchor after scrolling away: ' + movingDrift);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.evaluate(() => { window.oldHistoryHandler = window.testSockets.at(-1).onmessage; });
    await page.locator('#transcript').evaluate(scroller => { scroller.scrollTop = 0; });
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldOlderItems));
    await page.evaluate(() => { window.oldHistoryReply = window.testSockets.at(-1).heldOlderItems; });
    await page.setViewportSize({ width: 1440, height: 2200 });
    await page.locator('#transcript').evaluate(scroller => { scroller.scrollTop = Math.floor(scroller.scrollHeight / 2); });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await choose('d');
    await page.waitForTimeout(150);
    assert.equal(await page.locator('#messages article').count(), 20, 'A short initial page is not expanded by programmatic session-switch scrolling');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.method === 'thread/items/list' && frame.params.threadId === 'thread-d').length), 1, 'Bootstrap loads just one 20-item page until the reader requests older history');
    await page.evaluate(() => { window.testHoldOlderItems = false; window.oldHistoryHandler({ data: JSON.stringify({ id: window.oldHistoryReply.frame.id, result: window.oldHistoryReply.result }) }); });
    assert.equal(await page.locator('[data-item-id^="history-"]').count(), 0, 'A delayed older page from another session cannot enter the selected transcript');
    await page.locator('#prompt').fill('Selected session remains usable after pagination.');
    assert.equal(await page.locator('#send').isDisabled(), false, 'Switching away from a pending history read releases the new session controls');
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.evaluate(() => { window.testFailOlderItems = true; document.querySelector('#older').click(); });
    await page.getByRole('button', { name: 'Reload recent messages', exact: true }).waitFor();
    assert.match(await page.locator('#notice').textContent(), /Saved history cursor expired/, 'A failed older-history read explains the failed operation');
    assert.equal(await page.locator('#prompt').inputValue(), 'Selected session remains usable after pagination.', 'A history failure preserves the prompt draft');
    await page.locator('#older').click();
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 22);
    assert.equal(await page.locator('#notice').isHidden(), true, 'A successful older-history retry removes its recovered error banner');
    assert.equal(await page.locator('#reconnect').isHidden(), true, 'The reload fallback disappears after pagination recovers');
    assert.equal(await page.locator('#prompt').inputValue(), 'Selected session remains usable after pagination.');
    await page.locator('#disconnect').click(); await page.locator('#reconnect').click(); await connected();
    await page.evaluate(() => { window.testFailOlderItems = true; document.querySelector('#older').click(); });
    await page.getByRole('button', { name: 'Reload recent messages', exact: true }).waitFor();
    await page.getByRole('button', { name: 'Reload recent messages', exact: true }).click(); await connected();
    assert.equal(await page.locator('#messages article').count(), 20, 'Reloading after a history failure restores the latest 20 messages');
    assert.equal(await page.locator('#prompt').inputValue(), 'Selected session remains usable after pagination.', 'Reloading recent messages preserves the unsent prompt');
    assert.equal(await page.locator('#notice').isHidden(), true, 'Reloading recent messages clears the resolved older-history error');
    assert.equal(await page.locator('#reconnect').isHidden(), true);
    await current({ method: 'error', params: { threadId: 'thread-d', error: { message: 'Unrelated runtime error during pagination' } } });
    await page.locator('#older').click();
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 22);
    assert.match(await page.locator('#notice').textContent(), /Unrelated runtime error during pagination/, 'Successful pagination cannot clear an unrelated runtime error');

    // Sidebar deletion is a scoped gateway operation: inspecting or deleting
    // another row must not resume that thread or change the selected session.
    const deletable = id => ({ ...sessions[0], session_id: id, codex_thread_id: 'thread-' + id, name: 'Delete fixture ' + id + ' ' + hostile, cwd: '/projects/keep-' + id, state: 'idle' });
    const deletedOther = deletable('delete-other');
    const deleteFailure = deletable('delete-failure');
    const deletedActive = deletable('delete-active');
    const deleteUnknown = deletable('delete-unknown');
    sessions.push(deletedOther, deleteFailure, deletedActive, deleteUnknown);
    await page.locator('#refresh-sessions').click();
    await page.waitForSelector('[data-delete-session-id="delete-other"]');
    const deleteButton = id => page.locator('.session-delete[data-delete-session-id="' + id + '"]');
    const dialog = page.locator('#delete-session-dialog');
    const confirmDelete = async () => {
      const reply = page.waitForResponse(response => response.url().includes('/webui/sessions/delete') && response.request().method() === 'POST');
      await page.locator('#delete-session-confirm').click();
      await reply;
      return sessionDeletes.at(-1);
    };
    const completeDelete = async request => {
      Object.assign(deleteStatuses.get(request.request_id), { status: 'completed', pending: false, deleted: true, message: 'Session deleted. The working directory and files were kept.' });
      sessions.splice(sessions.findIndex(session => session.session_id === request.session_id), 1);
      await page.clock.fastForward(2100);
      await page.waitForSelector('[data-session-id="' + request.session_id + '"]', { state: 'detached' });
    };
    assert.equal(await page.locator('.session-delete').count(), sessions.length, 'Every desktop session row has its own delete button');
    assert.equal(await deleteButton('delete-other').getAttribute('aria-label'), 'Delete session ' + deletedOther.name, 'The delete icon has a complete accessible session name');
    assert.equal(await deleteButton('delete-other').evaluate(button => Boolean(button.closest('.session-button'))), false, 'Delete buttons are siblings of selection buttons, never nested interactive controls');
    const socketsBeforeDelete = await page.evaluate(() => window.testSockets.length);
    const deletesBeforeCancel = sessionDeletes.length;
    await deleteButton('delete-other').click();
    await dialog.waitFor({ state: 'visible' });
    assert.equal(await page.locator('#delete-session-name').textContent(), deletedOther.name);
    assert.match(await page.locator('#delete-session-cwd').textContent(), /\/projects\/keep-delete-other/);
    assert.match(await dialog.textContent(), /permanently.*conversation.*child sessions/is, 'Confirmation describes deletion of the conversation and child sessions');
    assert.match(await dialog.textContent(), /working directory.*all files.*kept/is, 'Confirmation explicitly preserves the working directory and files');
    assert.equal(await dialog.locator('img').count(), 0, 'Session names are escaped in the delete confirmation');
    assert.equal(await page.locator('[data-session-id="d"]').getAttribute('aria-current'), 'true', 'Opening another session’s confirmation does not select it');
    assert.equal(await page.evaluate(() => window.testSockets.length), socketsBeforeDelete, 'Opening a deletion dialog does not attach to the target Codex thread');
    await page.locator('#delete-session-cancel').click();
    assert.equal(sessionDeletes.length, deletesBeforeCancel, 'Cancelling a sidebar delete never sends a destructive request');
    assert.equal(await deleteButton('delete-other').isVisible(), true, 'Cancelling preserves the session row');

    rejectNextDelete = true;
    await deleteButton('delete-failure').click();
    await confirmDelete();
    await page.waitForFunction(() => document.querySelector('#delete-session-message').textContent.toLowerCase().includes('offline'));
    assert.equal(await page.locator('[data-session-id="delete-failure"]').count(), 1, 'A rejected offline deletion retains the session in the list');
    assert.equal(await page.locator('[data-session-id="d"]').getAttribute('aria-current'), 'true', 'Rejected deletion leaves the selected session untouched');
    await page.locator('#delete-session-cancel').click();

    await deleteButton('delete-other').click();
    const deletingOther = await confirmDelete();
    assert.deepEqual({ session_id: deletingOther.session_id, confirmed: deletingOther.confirmed, csrf: deletingOther.csrf }, { session_id: 'delete-other', confirmed: true, csrf: 'browser-test-csrf' }, 'Confirmed deletion targets the chosen row with CSRF protection');
    assert.match(deletingOther.request_id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i, 'Deletion is identified by a stable UUID for status lookup');
    assert.equal(await page.locator('[data-session-id="delete-other"]').count(), 1, 'A queued deletion is not mistaken for confirmed worker completion');
    await page.locator('#delete-session-cancel').click();
    await choose('a');
    const afterDeleteSwitch = await page.evaluate(() => window.testSockets.length);
    await completeDelete(deletingOther);
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('aria-current'), 'true', 'A delayed deletion result does not change the session selected after confirmation');
    assert.equal(await page.evaluate(() => window.testSockets.length), afterDeleteSwitch, 'Completing background deletion never opens a replacement transcript');
    assert.equal(sessionDeletes.filter(request => request.request_id === deletingOther.request_id).length, 1, 'Closing the dialog and changing sessions does not replay the delete POST');

    await deleteButton('delete-failure').click();
    const failingDelete = await confirmDelete();
    Object.assign(deleteStatuses.get(failingDelete.request_id), { status: 'failed', pending: false, deleted: false, error_code: 'session_busy', message: 'The session has an active turn. Wait for it to finish before deleting.' });
    await page.clock.fastForward(2100);
    await page.waitForFunction(() => document.querySelector('#delete-session-message').textContent.includes('active turn'));
    assert.equal(await page.locator('[data-session-id="delete-failure"]').count(), 1, 'Worker failure retains the session after the queued command finishes');
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('aria-current'), 'true');
    await page.locator('#delete-session-cancel').click();

    // A failed HTTP response does not prove the durable command was rejected.
    // Recover with status reads, never a second destructive POST.
    dropNextDeleteAcknowledgement = true;
    await deleteButton('delete-failure').click();
    const lostDeleteReply = page.waitForEvent('requestfailed', request => request.url().includes('/webui/sessions/delete') && request.method() === 'POST');
    await page.locator('#delete-session-confirm').click();
    await lostDeleteReply;
    const uncertainDelete = sessionDeletes.at(-1);
    await page.waitForFunction(() => document.querySelector('#delete-session-message').textContent.includes('may have been accepted'));
    assert.equal(await page.locator('[data-session-id="delete-failure"]').count(), 1, 'A lost delete acknowledgement does not optimistically remove the session');
    await completeDelete(uncertainDelete);
    assert.equal(sessionDeletes.filter(request => request.request_id === uncertainDelete.request_id).length, 1, 'A lost deletion acknowledgement is recovered through GET without replaying POST');
    assert.equal(await page.locator('[data-session-id="a"]').getAttribute('aria-current'), 'true', 'Recovering an uncertain deletion does not disturb the active session');
    await page.locator('#delete-session-cancel').click();

    await deleteButton('delete-unknown').click();
    const unknownDelete = await confirmDelete();
    Object.assign(deleteStatuses.get(unknownDelete.request_id), { status: 'outcome_unknown', pending: false, deleted: false, message: 'The worker could not confirm the deletion outcome. Check status before retrying.' });
    await page.clock.fastForward(2100);
    await page.waitForFunction(() => document.querySelector('#delete-session-confirm').textContent === 'Check status' && !document.querySelector('#delete-session-confirm').disabled);
    const postsBeforeUnknownCheck = sessionDeletes.length;
    const unknownStatusReply = page.waitForResponse(response => response.url().includes('/webui/sessions/delete') && response.request().method() === 'GET');
    await page.locator('#delete-session-confirm').click();
    await unknownStatusReply;
    assert.equal(sessionDeletes.length, postsBeforeUnknownCheck, 'An unknown terminal outcome only offers a read-only status check, never a fresh deletion');
    assert.equal(await page.locator('[data-session-id="delete-unknown"]').count(), 1, 'An unknown terminal outcome keeps the session visible until deletion is confirmed');
    await page.locator('#delete-session-cancel').click();

    await page.setViewportSize({ width: 390, height: 844 });
    await page.locator('#show-sessions').click();
    await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
    assert.equal(await deleteButton('delete-active').isVisible(), true, 'The delete icon is available on mobile without hover');
    await deleteButton('delete-active').scrollIntoViewIfNeeded();
    const deleteBounds = await deleteButton('delete-active').boundingBox();
    assert.ok(deleteBounds.width >= 40 && deleteBounds.height >= 40, 'Mobile delete icons have a usable touch target');
    await deleteButton('delete-active').click();
    assert.equal(await dialog.isVisible(), true, 'Mobile deletion opens the same explicit confirmation');
    assert.equal(await page.evaluate(() => document.activeElement?.matches('input,textarea,[contenteditable=true]') || false), false, 'Mobile deletion does not open a text keyboard');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Long malicious session names cannot overflow the mobile confirmation');
    await page.locator('#delete-session-cancel').click();
    await choose('delete-active');
    await page.locator('#show-sessions').click();
    await deleteButton('delete-active').click();
    const deletingActive = await confirmDelete();
    await completeDelete(deletingActive);
    assert.equal(await page.evaluate(() => window.testSockets.at(-1).readyState), 3, 'Confirmed deletion of the selected session detaches its transcript');
    assert.equal(await page.locator('#send').isDisabled(), true, 'A deleted session cannot receive a new prompt');
    if (await dialog.isVisible()) await page.locator('#delete-session-cancel').click();
    await page.setViewportSize({ width: 1440, height: 1000 });
    await choose('d');
    // A notification can select only an authorized inventory entry. Reusing
    // the existing page retains the draft in the conversation being left.
    const notificationID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
    sessions.push({ ...sessions[0], session_id: notificationID, name: 'Notification target', codex_thread_id: 'thread-a' });
    await page.locator('#refresh-sessions').click();
    await page.waitForSelector('[data-session-id="' + notificationID + '"]');
    await page.locator('#prompt').fill('Keep this draft when a notification opens another session.');
    const notificationConnections = await page.evaluate(() => window.testSockets.length);
    const notificationClick = (url, scriptURL = 'https://webui.test/tgw/webui/sw.js') => page.evaluate(({ url, scriptURL }) => {
      window.testNotificationAcknowledged = false;
      const event = new Event('message');
      Object.defineProperties(event, { data: { value: { type: 'codex-notification-open', url } }, source: { value: { scriptURL } }, ports: { value: [{ postMessage: () => { window.testNotificationAcknowledged = true; } }] } });
      navigator.serviceWorker.dispatchEvent(event);
    }, { url, scriptURL });
    await notificationClick('/tgw/webui/?session_id=' + notificationID, 'https://evil.test/tgw/webui/sw.js');
    await notificationClick('https://evil.test/tgw/webui/?session_id=' + notificationID);
    await notificationClick('/tgw/webui/?session_id=not-a-session');
    assert.equal(await page.evaluate(() => window.testSockets.length), notificationConnections, 'Untrusted worker senders, off-origin URLs and malformed IDs cannot select a session');
    await notificationClick('/tgw/webui/?session_id=' + notificationID);
    await page.waitForSelector('[data-session-id="' + notificationID + '"][aria-current="true"]'); await connected();
    assert.equal(await page.evaluate(() => window.testNotificationAcknowledged), true, 'A valid notification click acknowledges handling to the service worker');
    assert.equal(new URL(page.url()).searchParams.get('session_id'), notificationID);
    await notificationClick('/tgw/webui/?session_id=11111111-2222-4333-8444-555555555555');
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('no longer available'));
    assert.equal(await page.locator('[data-session-id="' + notificationID + '"]').getAttribute('aria-current'), 'true', 'An unavailable notification target does not disrupt the current session');
    await choose('d');
    assert.equal(await page.locator('#prompt').inputValue(), 'Keep this draft when a notification opens another session.', 'Notification navigation preserves the previous session draft');
    await page.goto('https://webui.test/tgw/webui/?session_id=' + notificationID);
    await page.waitForSelector('[data-session-id="' + notificationID + '"][aria-current="true"]'); await connected();
    assert.equal(await page.locator('#session-title').textContent(), 'Notification target', 'Opening the app from a notification selects its authorized session after loading inventory');
    assert.equal(await page.locator('#auth a').getAttribute('href'), '/tgw/admin/?next=' + encodeURIComponent('/tgw/webui/?session_id=' + notificationID), 'Passkey sign-in preserves the validated notification target');
    await page.goto('https://webui.test/tgw/webui/?session_id=invalid');
    await page.waitForSelector('[data-session-id="a"]');
    assert.equal(await page.evaluate(() => window.testSockets.length), 0, 'An invalid startup deep link cannot attach to a runtime');
    await choose('d');
    await page.locator('#disconnect').click();
    assert.equal(await page.evaluate(() => window.testSockets.at(-1).readyState), 3, 'The conversation socket is closed before testing activity-only authentication');
    await requestWorkerUpdates();
    const readsBeforeExpiry = gatewayStatusReads();
    authStatus = 401;
    await page.evaluate(() => window.testActivitySocket.drop(1006));
    await page.waitForSelector('#auth:not([hidden])');
    await page.clock.fastForward(31000);
    assert.equal(gatewayStatusReads(), readsBeforeExpiry, 'Expired authentication cancels update status timers');
    assert.equal(await page.locator('#command-panel').isHidden(), true, 'Expired authentication removes update results');
    if (process.env.WEBUI_SCREENSHOTS) {
      await page.setViewportSize({ width: 390, height: 844 });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-signin.png') });
    }
    assert.equal(await page.locator('#messages article').count(), 0);
    assert.equal(await page.locator('.session-button').count(), 0);
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.deepEqual(await page.evaluate(() => Object.keys(localStorage)), ['codex-webui-font-size'], 'Expired authentication keeps only the harmless font preference');
    assert.equal(await page.evaluate(() => localStorage.getItem('codex-webui-font-size')), '18');
    assert.equal(await page.evaluate(() => Object.keys(sessionStorage).length), 0);
    assert.ok(paths.every(value => value.startsWith('/tgw/')), 'All gateway routes use /tgw');
    assert.deepEqual(errors, []);
    console.log('Web UI browser checks passed: slash autocomplete and CLI choices with keyboard/touch/current markers, successful setting auto-close without mobile autofocus, errors/read-only results remain visible, typed worker and gateway commands without a session, permission confirmation/stale-button isolation, unavailable commands never become prompts, native and global v2 model/effort changes with inventory/resume/command-ack isolation, read-only model status, clipboard images/text+image/image-only/per-session drafts/navigation races/no replay, tool colors in both themes, safe formatting/nonempty reasoning/native sub-agent activity, chronology/pagination, independent global activity events/sequence gaps/resync/heartbeat and handshake recovery/inventory races, static Working label plus matching green activity pulse/reduced motion, worker heading background/sidebar footer, desktop font controls/persistence/mobile isolation, full-width responsive status, live quotas/reset tooltips/no polling/session isolation/unavailable fallback, responsive layout and bottom following through late layout changes, keyboard resize/pan/page scroll/delayed focus/dismissal/mobile menu without autofocus, native inner scrolling, blocking/async questions, rejected-answer recovery, approvals, live external prompts, session isolation, disconnect/reconnect, prompt and command drafts/no replay, scoped connection/history/retry error recovery without hiding uncertain-send or runtime warnings, ACK-only ephemeral queued-steer feedback/timer/switch/end/rejection/isolation, mobile status grid alignment/empty rows, failed resume, sidebar deletion confirmation/desktop/mobile/cancel/CSRF/scope/delayed result/failure/lost acknowledgement/unknown outcome/no POST replay, idle heartbeat, auth expiry.');
  } catch (error) {
    if (process.env.WEBUI_SCREENSHOTS) {
      fs.mkdirSync(process.env.WEBUI_SCREENSHOTS, { recursive: true });
      await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-failure.png') });
    }
    throw error;
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

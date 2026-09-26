#!/usr/bin/env node
'use strict';
// Actual embedded WebUI, rendered with fictional API/RPC fixtures. No live
// gateway, Codex account, local session history, or external network is used.
// PLAYWRIGHT_MODULE=/path/to/playwright node scripts/capture-site-screenshots.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const output = path.resolve(__dirname, '../site/assets/screenshots');
const now = Math.floor(new Date('2026-09-23T14:35:00Z').getTime() / 1000);
const sessions = [
  { session_id: 'demo-storefront', codex_thread_id: 'thread-storefront', worker_id: 'linux', worker_name: 'Studio Linux', runtime_name: 'Development', name: 'Build the storefront', cwd: '/projects/storefront', state: 'running', stats: { model: 'gpt-6-astra', reasoning_effort: 'high', context_tokens: 48200, context_window: 258400 } },
  { session_id: 'demo-checkout', codex_thread_id: 'thread-checkout', worker_id: 'linux', worker_name: 'Studio Linux', runtime_name: 'Development', name: 'Polish checkout', cwd: '/projects/checkout', state: 'waiting_input' },
  { session_id: 'demo-api', codex_thread_id: 'thread-api', worker_id: 'linux', worker_name: 'Studio Linux', runtime_name: 'Development', name: 'Review API changes', cwd: '/projects/api', state: 'idle' },
  { session_id: 'demo-docs', codex_thread_id: 'thread-docs', worker_id: 'mac', worker_name: 'MacBook Pro', runtime_name: 'Development', name: 'Update documentation', cwd: '/projects/docs', state: 'running' },
  { session_id: 'demo-library', codex_thread_id: 'thread-library', worker_id: 'mac', worker_name: 'MacBook Pro', runtime_name: 'Development', name: 'Refactor component library', cwd: '/projects/components', state: 'idle' },
];
const turns = [
  { id: 'turn-accessibility', status: 'inProgress', startedAt: now - 35, items: [
    { id: 'user-followup', type: 'userMessage', content: [{ type: 'text', text: 'Now check keyboard navigation and the mobile layout.' }] },
    { id: 'live-comment', type: 'agentMessage', phase: 'commentary', text: 'Checking the focus order, touch targets, and layout at 390 px. I’ll keep the changes small and run the checks again.' },
  ] },
  { id: 'turn-storefront', status: 'completed', startedAt: now - 420, items: [
    { id: 'user-request', type: 'userMessage', content: [{ type: 'text', text: 'Make the storefront feel fast and clear. Add product filtering and keep the cart visible on mobile.' }] },
    { id: 'reasoning-summary', type: 'reasoning', summary: ['Checked the product grid and cart layout before updating the filters.'] },
    { id: 'tool-check', type: 'commandExecution', command: 'npm run test -- storefront', status: 'completed', exitCode: 0, aggregatedOutput: '✓ Product filters\n✓ Persistent cart\n✓ Responsive layout\n\n12 tests passed.' },
    { id: 'completed-reply', type: 'agentMessage', text: 'The storefront is ready for a second pass.\n\n- **Product filters** update the grid without a page reload.\n- **The cart stays visible** on smaller screens.\n\n```diff\n- grid-template-columns: repeat(4, 1fr);\n+ grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));\n```\n\nAll 12 storefront tests passed.' },
  ] },
];

async function prepare(page, pendingQuestions = true) {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.clock.install({ time: new Date(now * 1000) });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'fictional-demo-csrf', url: 'https://webui.example.test/', secure: true, sameSite: 'Strict' }]);
  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== 'https://webui.example.test') return route.abort('blockedbyclient');
    const json = data => route.fulfill({ contentType: 'application/json', body: JSON.stringify(data) });
    if (url.pathname === '/tgw/api/v1/admin/session') return json({ authenticated: true, session_id: 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa', owner_id: 'screenshot-owner', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 8 * 3600000).toISOString(), reauthenticated_at: new Date().toISOString() });
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions });
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: true, public_key: 'fictional-demo-public-key', subscribed: false, scope: 'all' });
    let name = url.pathname === '/tgw/webui/' ? 'webui.html' : url.pathname.endsWith('/manifest.webmanifest') ? 'webui-manifest.webmanifest' : path.basename(url.pathname);
    if (!/^(webui[\w.-]*|session-auth)\.(html|js|css|svg|png|webmanifest)$/.test(name) || !fs.existsSync(path.join(assets, name))) return route.fulfill({ status: 404, body: 'Not part of the demo fixture' });
    const mime = { html: 'text/html', js: 'text/javascript', css: 'text/css', svg: 'image/svg+xml', png: 'image/png', webmanifest: 'application/manifest+json' }[name.split('.').pop()];
    return route.fulfill({ contentType: mime, body: fs.readFileSync(path.join(assets, name)) });
  });
  await page.addInitScript(({ sessions, turns, now, pendingQuestions }) => {
    // Present an unconfigured browser's notification state. Headless Chromium
    // denies this permission by default; no prompt/subscription is requested.
    Object.defineProperty(Notification, 'permission', { get: () => 'default' });
    localStorage.setItem('codex-webui-font-size', '15');
    localStorage.setItem('codex-webui-theme', 'dark');
    const rows = sessions.map(session => ({ session_id: session.session_id, state: session.state, active_turn_id: session.state === 'running' ? 'turn-accessibility' : '', worker_connectivity: 'online', runtime_state: 'running', pending_questions: pendingQuestions && session.session_id === 'demo-checkout' ? 1 : 0, pending_revision: pendingQuestions && session.session_id === 'demo-checkout' ? 'demo-question' : '', model: 'gpt-6-astra', reasoning_effort: 'high', settings_revision: '1:1' }));
    class FixtureSocket {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.url = url; this.activity = new URL(url).pathname.endsWith('/activity');
        if (this.activity) window.demoShowQuestion = () => { const session = { ...rows.find(row => row.session_id === 'demo-checkout'), pending_questions: 1, pending_revision: 'demo-question' }; this.emit({ type: 'activity_event', version: 2, sequence: 1, revision: 2, event: 'question_requested', session_id: session.session_id, session }); };
        setTimeout(() => { this.readyState = 1; this.onopen?.({}); this.emit(this.activity ? { type: 'activity_snapshot', version: 2, sequence: 0, revision: 1, sessions: rows } : { type: 'ready' }); }, 10);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
      send(raw) {
        const request = JSON.parse(raw); let result = {};
        if (request.method === 'thread/resume') result = { thread: { id: request.params.threadId }, model: 'gpt-6-astra', reasoningEffort: 'high' };
        else if (request.method === 'thread/items/list') result = { data: turns.flatMap(turn => [...turn.items].reverse().map(item => ({ turnId: turn.id, item: { ...item, createdAt: turn.startedAt } }))), nextCursor: null };
        else if (request.method === 'thread/turns/list') result = { data: turns.map(({ items, ...turn }) => ({ ...turn, items: [] })), nextCursor: null };
        else if (request.method === 'gateway/questions') result = { questions: [] };
        else if (request.method === 'model/list') result = { data: [{ id: 'gpt-6-astra', model: 'gpt-6-astra', displayName: 'GPT-6 Astra', supportedReasoningEfforts: [{ reasoningEffort: 'high' }], defaultReasoningEffort: 'high' }] };
        else if (request.method === 'account/rateLimits/read') result = { rateLimits: { limitId: 'codex', primary: { usedPercent: 18, windowDurationMins: 300, resetsAt: now + 7200 }, secondary: { usedPercent: 32, windowDurationMins: 10080, resetsAt: now + 3 * 86400 } } };
        else throw new Error('Unexpected demo RPC: ' + request.method);
        setTimeout(() => this.emit({ id: request.id, result }), 10);
      }
    }
    window.WebSocket = FixtureSocket;
  }, { sessions, turns, now, pendingQuestions });
  await page.goto('https://webui.example.test/tgw/webui/');
  await page.locator('.session-button[data-session-id="demo-storefront"]').waitFor({ state: 'attached' });
  // Select through the actual interface, including its mobile drawer.
  if (page.viewportSize().width <= 650) await page.locator('#show-sessions').click();
  await page.locator('.session-button[data-session-id="demo-storefront"]').click();
  await page.locator('#connection[data-state="connected"]').waitFor();
  await page.getByText('All 12 storefront tests passed.', { exact: true }).waitFor();
  await page.locator('#rate-limits .rate-limit').first().waitFor();
  await page.waitForFunction(() => !document.querySelector('#connection-text').textContent.includes('Checking questions'));
  await page.waitForFunction(() => !document.querySelector('#toggle-notifications').textContent.includes('Updating'));
  assert.equal(await page.locator('#toggle-notifications').textContent(), 'Enable notifications', await page.locator('#notifications-status').textContent());
  assert.equal(await page.locator('#turn-state').innerText(), 'Working');
  assert.deepEqual(errors, []);
  return errors;
}

(async () => {
  fs.mkdirSync(output, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  try {
    const desktop = await browser.newPage({ viewport: { width: 1440, height: 980 }, deviceScaleFactor: 1, colorScheme: 'dark', locale: 'en-GB', timezoneId: 'UTC' });
    await prepare(desktop);
    await desktop.screenshot({ path: path.join(output, 'webui-desktop.png'), animations: 'disabled' });
    const mobile = await browser.newPage({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 1, isMobile: true, hasTouch: true, colorScheme: 'dark', locale: 'en-GB', timezoneId: 'UTC' });
    await prepare(mobile, false);
    assert.equal(await mobile.locator('#show-sessions').getAttribute('data-activity'), 'running');
    await mobile.screenshot({ path: path.join(output, 'webui-mobile.png'), animations: 'disabled' });
    await mobile.evaluate(() => window.demoShowQuestion());
    await mobile.locator('#show-sessions').click();
    await mobile.locator('.session-button[data-activity="question"]').waitFor();
    assert.equal(await mobile.locator('.session-button[data-activity="running"]').count(), 2);
    await mobile.screenshot({ path: path.join(output, 'webui-mobile-sessions.png'), animations: 'disabled' });
    const hashes = {};
    for (const name of ['webui.html', 'webui.css', 'webui.js']) hashes[name] = crypto.createHash('sha256').update(fs.readFileSync(path.join(assets, name))).digest('hex');
    fs.writeFileSync(path.join(output, 'capture.json'), JSON.stringify({ captured_at: new Date().toISOString(), demo_data: true, description: 'Actual embedded WebUI rendered in Chromium with fictional API and WebSocket fixtures. No live server or private session data.', source_sha256: hashes, images: { 'webui-desktop.png': { width: 1440, height: 980 }, 'webui-mobile.png': { width: 390, height: 844 }, 'webui-mobile-sessions.png': { width: 390, height: 844 } } }, null, 2) + '\n');
    console.log('Captured actual desktop, mobile conversation, and mobile session drawer screenshots with fictional demo data.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

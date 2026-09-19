#!/usr/bin/env node
// Optional browser regression checks. Use an existing Playwright installation:
// PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-admin-ui.cjs
// This is a development check; the gateway has no Node.js runtime dependency.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const malicious = '<img src=x onerror="window.injected=true">';
const time = '2026-01-20T12:00:00Z';
const fullName = 'A complete session title that stays readable on a narrow screen and is never truncated ' + malicious;
const sessions = Array.from({ length: 12 }, (_, i) => ({
  session_id: 'session-' + i, worker_id: 'worker-1', runtime_id: 'runtime-1', codex_thread_id: 'thread-' + i,
  name: i === 0 ? fullName : 'Session ' + i, cwd: i === 0 ? '/projects/a-very-long-directory-name-that-must-wrap-without-horizontal-scroll/' + 'z'.repeat(150) : '/projects/sample',
  state: i === 0 ? 'running' : 'idle', loaded: true, archived: false, worker_name: 'Linux worker', runtime_name: 'Main runtime', worker_connectivity: 'connected', codex_version: '1.2.3',
  pending_approvals: i === 0 ? 1 : 0, queued_commands: 0,
  stats: i === 1 ? { history_complete: false } : { created_at: time, active_since: i === 0 ? time : undefined, last_message_at: time, prompt_count: i === 0 ? 0 : 12, assistant_message_count: 11, total_tokens: 42000, input_tokens: 40000, cached_input_tokens: 5000, output_tokens: 2000, reasoning_output_tokens: 1000, context_tokens: 6000, context_window: 200000, last_message_role: 'user', last_message: malicious, model: 'test-model', reasoning_effort: 'high', observed_at: time, history_complete: i !== 2 },
}));
const data = {
  workers: [{ ID: 'worker-1', Name: 'Linux worker ' + malicious, OS: 'linux', Arch: 'amd64', Connectivity: 'connected', Version: '1.2.3', Enabled: true }],
  runtimes: [{ runtime_id: 'runtime-1' }],
  sessions: [...sessions, { session_id: 'hidden', name: 'Archived subagent', state: 'idle', archived: true }], pending_approvals: 1, queued_commands: 0,
  bot: { username: 'example_bot', display_name: 'Example bot ' + malicious, id: 12345, status: 'connected', webhook_status: 'active', webhook_url: 'https://gateway.example.com/tgapi/v1/telegram/webhook', pending_updates: 0, allowed_user_count: 1, allowed_chat_count: 0, checked_at: time },
};

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let dashboardStatus = 200;
  let dashboardReads = 0;
  const mutations = [];
  const paths = [];
  await page.route('http://admin.test/**', async route => {
    const request = route.request();
    const url = new URL(request.url());
    paths.push(url.pathname);
    if (url.pathname.startsWith('/tgapi/')) {
      let body = {};
      let status = 200;
      if (url.pathname.endsWith('/dashboard')) { body = data; status = dashboardStatus; dashboardReads++; }
      else if (url.pathname.endsWith('/passkeys')) body = [{ id: 'test-passkey', created_at: time }];
      else if (url.pathname.endsWith('/rotate-token')) body = { token: 'one-time-secret' };
      else if (url.pathname.endsWith('/workers') && request.method() === 'POST') body = { worker_id: 'new-worker', token: 'one-time-secret' };
      if (request.method() !== 'GET') mutations.push({ path: url.pathname, method: request.method(), body: request.postData() });
      return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    }
    const filename = url.pathname.endsWith('app.js') ? 'app.js' : url.pathname.endsWith('app.css') ? 'app.css' : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  try {
    if (page.clock) await page.clock.install();
    await page.goto('http://admin.test/tgadmin');
    await page.waitForSelector('#console:not([hidden])');
    assert.equal(await page.locator('#sessions').textContent(), '12');
    assert.equal(await page.locator('.session-card').count(), 10);
    assert.equal(await page.locator('.session-card h3').first().textContent(), fullName);
    assert.equal(await page.locator('.session-metrics dd').first().textContent(), '0', 'Known zero must not look unavailable');
    const partial = page.locator('[data-session-id="session-2"]');
    assert.equal(await partial.locator('.session-metrics dd').nth(0).textContent(), '≥12', 'Partial prompt count is a lower bound');
    assert.equal(await partial.locator('.session-metrics dd').nth(1).textContent(), '≥11', 'Partial reply count is a lower bound');
    assert.equal(await partial.locator('.session-metrics dd').nth(2).textContent(), '42,000', 'Recorded cumulative tokens are independent of partial history');
    assert.match(await partial.innerText(), /Partial history · message counts may still be loading/);
    assert.equal(await page.locator('#bot-info a').getAttribute('href'), 'https://t.me/example_bot');
    assert.match(await page.locator('#bot-info').innerText(), /Any chat with an allowed user/);
    assert.equal(await page.locator('img').count(), 0, 'API data must not become HTML');
    assert.equal(await page.evaluate(() => window.injected), undefined);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Mobile layout must fit viewport');
    await page.locator('.session-card summary').first().click();
    await page.locator('#refresh').click();
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.equal(await page.locator('.session-card details').first().getAttribute('open'), '', 'Refresh preserves expanded details');
    assert.match(await page.locator('.session-card').first().innerText(), /6,000 \/ 200,000 \(3%\)/);
    if (process.env.ADMIN_UI_SCREENSHOTS) {
      fs.mkdirSync(process.env.ADMIN_UI_SCREENSHOTS, { recursive: true });
      await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-mobile.png'), fullPage: true });
    }
    await page.locator('#session-next').click();
    assert.equal(await page.locator('.session-card').count(), 2);
    assert.equal(await page.locator('#session-page').textContent(), 'Page 2 of 2');
    await page.locator('#session-search').fill('Linux worker');
    assert.equal(await page.locator('.session-card').count(), 10, 'Search resets page');
    await page.locator('#session-state').selectOption('running');
    assert.equal(await page.locator('.session-card').count(), 1);
    await page.locator('#session-search').fill('not-present');
    assert.match(await page.locator('#session-list').innerText(), /No sessions match/);
    await page.locator('#session-search').fill('Session 1');
    await page.locator('#session-state').selectOption('');
    const unknown = page.locator('[data-session-id="session-1"]');
    assert.equal(await unknown.locator('.session-metrics dd').first().textContent(), '—');
    assert.match(await unknown.innerText(), /Not available/);
    await page.locator('#session-search').fill('');
    dashboardStatus = 503;
    await page.locator('#refresh').click();
    await page.waitForSelector('#dashboard-error:not([hidden])');
    assert.equal(await page.locator('.session-card').count(), 10, 'Keep last good data on temporary errors');
    dashboardStatus = 200;
    await page.locator('#refresh').click();
    await page.waitForFunction(() => document.getElementById('dashboard-error').hidden && !document.getElementById('refresh').disabled);
    // Exercise existing enrollment-token flows without placing the token in the DOM.
    const dialogs = [];
    page.on('dialog', async dialog => { dialogs.push({ message: dialog.message(), value: dialog.defaultValue() }); await dialog.accept(dialog.message() === 'Worker display name' ? 'Fresh worker' : ''); });
    await page.getByRole('button', { name: 'Rotate token', exact: true }).click();
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    await page.locator('#new-worker').click();
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.ok(mutations.some(entry => entry.path.endsWith('/worker-1/rotate-token') && entry.method === 'POST'));
    assert.ok(mutations.some(entry => entry.path.endsWith('/workers') && entry.body.includes('Fresh worker')));
    assert.ok(dialogs.some(dialog => dialog.value === 'one-time-secret'));
    assert.equal((await page.locator('body').innerText()).includes('one-time-secret'), false);
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    if (process.env.ADMIN_UI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-desktop.png'), fullPage: true });
    if (page.clock) {
      const before = dashboardReads;
      await page.clock.fastForward(16000);
      await page.waitForFunction(() => !document.getElementById('refresh').disabled);
      assert.ok(dashboardReads > before, 'Visible authenticated dashboard refreshes automatically');
    }
    dashboardStatus = 401;
    await page.locator('#refresh').click();
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#console').getAttribute('hidden'), '');
    assert.equal(await page.locator('.session-card').count(), 0, 'Session expiry removes session data');
    assert.ok(paths.every(path => path === '/tgadmin' || path.startsWith('/tgadmin/') || path.startsWith('/tgapi/')), 'Every gateway request must use a tg-prefixed route');
    assert.deepEqual(errors, []);
    console.log('Admin browser checks passed: mobile/desktop layout, full names, stats, filters, pagination, safe rendering, refresh/error recovery, enrollment flows, session expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

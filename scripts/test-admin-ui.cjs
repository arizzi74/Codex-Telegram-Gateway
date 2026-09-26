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
  bot: { username: 'example_bot', display_name: 'Example bot ' + malicious, id: 12345, status: 'connected', webhook_status: 'active', webhook_url: 'https://gateway.example.com/tgw/api/v1/telegram/webhook', pending_updates: 0, allowed_user_count: 1, allowed_chat_count: 0, checked_at: time },
};

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let dashboardStatus = 200;
  let loginStatus = 200;
  let browserID = 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa';
  let nearExpiry = false;
  let holdLoginFinish = false;
  let releaseLoginFinish = null;
  let markFinishStarted = null;
  let loginFinishStatus = 200;
  let loginFinishCalls = 0;
  const otherBrowserID = 'bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb';
  let otherBrowserRevoked = false;
  let dashboardReads = 0;
  const mutations = [];
  const paths = [];
  const nextDashboardResponse = () => page.waitForResponse(response =>
    new URL(response.url()).pathname === '/tgw/api/v1/admin/dashboard' && response.request().method() === 'GET');
  await page.route('https://admin.test/**', async route => {
    const request = route.request();
    const url = new URL(request.url());
    paths.push(url.pathname);
    if (url.pathname === '/tgw/webui/') return route.fulfill({ contentType: 'text/html', body: '<p>Web UI destination</p>' });
    if (url.pathname.startsWith('/tgw/api/')) {
      let body = {};
      let status = 200;
      if (url.pathname.endsWith('/dashboard')) { body = data; status = dashboardStatus; dashboardReads++; }
      else if (url.pathname.endsWith('/admin/session')) {
        status = loginStatus;
        body = { authenticated: true, session_id: browserID, server_time: new Date().toISOString(), expires_at: new Date(Date.now() + (nearExpiry ? 240000 : 8 * 3600000)).toISOString(), reauthenticated_at: new Date().toISOString() };
      }
      else if (url.pathname.endsWith('/login/begin')) body = { ceremony_id: 'ceremony', publicKey: { challenge: 'dGVzdC1jaGFsbGVuZ2U', rpId: 'admin.test' } };
      else if (url.pathname.endsWith('/login/finish')) {
        loginFinishCalls++;
        if (holdLoginFinish) await new Promise(resolve => { releaseLoginFinish = resolve; markFinishStarted?.(); });
        status = loginFinishStatus;
        if (status === 200) { browserID = 'cccccccc-3333-4333-8333-cccccccccccc'; nearExpiry = false; }
        body = { ok: status === 200 };
      }
      else if (url.pathname.endsWith('/admin/sessions')) body = { sessions: [
        { session_id: browserID, browser_label: 'Safari on iOS', current: true, created_at: time, last_seen_at: time, expires_at: time },
        ...(!otherBrowserRevoked ? [{ session_id: otherBrowserID, browser_label: 'Browser ' + malicious, current: false, created_at: time, last_seen_at: time, expires_at: time }] : []),
      ] };
      else if (url.pathname.endsWith('/admin/sessions/' + otherBrowserID) && request.method() === 'DELETE') { otherBrowserRevoked = true; status = 204; }
      else if (url.pathname.endsWith('/admin/logout')) { loginStatus = 401; status = 204; }
      else if (url.pathname.endsWith('/passkeys')) body = [{ id: 'test-passkey', created_at: time }];
      else if (url.pathname.endsWith('/rotate-token')) body = { token: 'one-time-secret' };
      else if (url.pathname.endsWith('/workers') && request.method() === 'POST') body = { worker_id: 'new-worker', token: 'one-time-secret' };
      if (request.method() !== 'GET') mutations.push({ path: url.pathname, method: request.method(), body: request.postData() });
      return route.fulfill({ status, contentType: 'application/json', body: status === 204 ? '' : JSON.stringify(body) });
    }
    const filename = /\.(js|css)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(() => {
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async () => ({
      id: 'passkey', rawId: new Uint8Array([1, 2, 3]).buffer, type: 'public-key',
      response: { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer },
    }) });
  });
  try {
    if (page.clock) await page.clock.install();
    await page.goto('https://admin.test/tgw/admin');
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
    await page.waitForSelector('.browser-session');
    assert.equal(await page.locator('.browser-session').count(), 2);
    assert.match(await page.locator('#browser-sessions').innerText(), /Safari on iOS · This browser/);
    assert.equal(await page.locator('#browser-sessions img').count(), 0, 'Browser labels must remain plain text');
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
    // The POST and token dialog finish before load() disables Refresh. An
    // already-enabled button does not prove that the resulting refresh ran.
    await Promise.all([
      nextDashboardResponse(),
      page.getByRole('button', { name: 'Rotate token', exact: true }).click(),
    ]);
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    await Promise.all([
      nextDashboardResponse(),
      page.locator('#new-worker').click(),
    ]);
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.ok(mutations.some(entry => entry.path.endsWith('/worker-1/rotate-token') && entry.method === 'POST'));
    assert.ok(mutations.some(entry => entry.path.endsWith('/workers') && entry.body.includes('Fresh worker')));
    assert.ok(dialogs.some(dialog => dialog.value === 'one-time-secret'));
    assert.equal((await page.locator('body').innerText()).includes('one-time-secret'), false);
    await Promise.all([
      nextDashboardResponse(),
      page.locator('.browser-session').nth(1).getByRole('button', { name: 'Sign out', exact: true }).click(),
    ]);
    await page.waitForFunction(() => document.querySelectorAll('.browser-session').length === 1);
    assert.ok(mutations.some(entry => entry.path.endsWith('/sessions/' + otherBrowserID) && entry.method === 'DELETE'));
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    if (process.env.ADMIN_UI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-desktop.png'), fullPage: true });
    if (page.clock) {
      await page.waitForFunction(() => !document.getElementById('refresh').disabled);
      const before = dashboardReads;
      // Advancing the browser clock schedules fetch; it does not wait for the
      // Node-side route handler or the browser's response/render continuation.
      await Promise.all([nextDashboardResponse(), page.clock.fastForward(16000)]);
      await page.waitForFunction(() => !document.getElementById('refresh').disabled);
      assert.ok(dashboardReads > before, 'Visible authenticated dashboard refreshes automatically');
    }
    // Hold login/finish while the old cookie has been revoked. No dashboard
    // request may begin in that window, including the 15-second automatic poll.
    // A rejected finish must verify the still-valid cookie and resume once.
    for (const finishStatus of [200, 503]) {
      await page.waitForFunction(() => !document.getElementById('refresh').disabled);
      nearExpiry = true; holdLoginFinish = true; loginFinishStatus = finishStatus;
      await page.evaluate(() => sessionAuth.verify('test-warning'));
      const finishStarted = new Promise(resolve => { markFinishStarted = resolve; });
      const finishesBefore = loginFinishCalls;
      await page.locator('.session-auth-banner button').click();
      await finishStarted;
      const beforePoll = dashboardReads;
      dashboardStatus = 401;
      if (page.clock) await page.clock.fastForward(16000);
      await page.evaluate(() => load(false)); // Manual refresh is fenced too.
      assert.equal(dashboardReads, beforePoll, 'Rotation pauses dashboard reads made with the revoked cookie');
      assert.equal(await page.locator('#console').getAttribute('hidden'), null, 'Renewal keeps the current console visible');
      dashboardStatus = 200; holdLoginFinish = false;
      releaseLoginFinish();
      await page.waitForFunction(() => !rotationPending && !document.getElementById('refresh').disabled && !document.getElementById('console').inert);
      assert.equal(await page.locator('#console').getAttribute('hidden'), null, 'Finish success or recovery preserves authenticated console');
      assert.ok(dashboardReads > beforePoll, 'Dashboard polling resumes after verifying the cookie');
      assert.equal(loginFinishCalls, finishesBefore + 1, 'Failed finish is never automatically replayed');
      assert.equal(await page.evaluate(() => sessionAuth.snapshot()?.session_id), browserID);
      if (finishStatus === 503) assert.match(await page.locator('#message').textContent(), /sign-in failed|^$/i);
    }
    nearExpiry = false;
    dashboardStatus = 401;
    await page.locator('#refresh').click();
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#console').getAttribute('hidden'), '');
    assert.equal(await page.locator('.session-card').count(), 0, 'Session expiry removes session data');
    assert.equal(await page.locator('.browser-session').count(), 0, 'Session expiry removes browser-session data');
    assert.ok(paths.every(path => path === '/tgw/admin' || path.startsWith('/tgw/admin/') || path.startsWith('/tgw/api/')), 'Every gateway request must use the /tgw prefix');
    // Notification links retain only the built-in session target after login.
    dashboardStatus = 200;
    const sessionLink = '/tgw/webui/?session_id=11111111-2222-4333-8444-555555555555';
    for (const destination of ['/tgw/webui/', sessionLink]) {
      await page.goto('https://admin.test/tgw/admin/?next=' + encodeURIComponent(destination));
      await page.waitForURL('https://admin.test' + destination);
    }
    for (const destination of ['https://invalid.example/', '//invalid.example/', '/tgw/admin/', sessionLink + '&next=https://invalid.example/', sessionLink + '\n', '/tgw/webui/?session_id=invalid']) {
      await page.goto('https://admin.test/tgw/admin/?next=' + encodeURIComponent(destination));
      await page.waitForSelector('#console:not([hidden])');
      assert.equal(new URL(page.url()).pathname, '/tgw/admin/', 'Unsafe or unsupported return targets stay in admin');
    }
    // Explicit sign-out cleans up the browser device even after a fresh login.
    const device = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
    await page.evaluate(id => localStorage.setItem('codex-webui-push-subscription', id), device);
    const beforeLogout = mutations.length;
    await page.locator('#logout').click();
    await page.waitForFunction(() => !localStorage.getItem('codex-webui-push-subscription'));
    const logoutCalls = mutations.slice(beforeLogout);
    assert.ok(logoutCalls.some(entry => entry.path.endsWith('/webui/push/unsubscribe') && JSON.parse(entry.body).subscription_id === device), 'Sign-out disables the current device');
    assert.ok(logoutCalls.findIndex(entry => entry.path.endsWith('/webui/push/unsubscribe')) < logoutCalls.findIndex(entry => entry.path.endsWith('/admin/logout')), 'Device cleanup starts while the login is still valid');
    assert.deepEqual(errors, []);
    console.log('Admin browser checks passed: mobile/desktop layout, full names, stats, filters, pagination, safe rendering, refresh/error recovery, enrollment flows, renewal/poll races, failed-finish recovery, session expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

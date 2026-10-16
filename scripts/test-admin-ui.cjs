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
const revokedWorkerName = 'Revoked worker ' + malicious;
const sessions = Array.from({ length: 12 }, (_, i) => ({
  session_id: 'session-' + i, worker_id: 'worker-1', runtime_id: 'runtime-1', codex_thread_id: 'thread-' + i,
  name: i === 0 ? fullName : 'Session ' + i, cwd: i === 0 ? '/projects/a-very-long-directory-name-that-must-wrap-without-horizontal-scroll/' + 'z'.repeat(150) : '/projects/sample',
  state: i === 0 ? 'running' : 'idle', loaded: true, archived: false, worker_name: 'Linux worker', runtime_name: 'Main runtime', worker_connectivity: 'connected', codex_version: '1.2.3',
  pending_approvals: i === 0 ? 1 : 0, queued_commands: 0,
  stats: i === 1 ? { history_complete: false } : { created_at: time, active_since: i === 0 ? time : undefined, last_message_at: time, prompt_count: i === 0 ? 0 : 12, assistant_message_count: 11, total_tokens: 42000, input_tokens: 40000, cached_input_tokens: 5000, output_tokens: 2000, reasoning_output_tokens: 1000, context_tokens: 6000, context_window: 200000, last_message_role: 'user', last_message: malicious, model: 'test-model', reasoning_effort: 'high', observed_at: time, history_complete: i !== 2 },
}));
const data = {
  workers: [
    { ID: 'worker-1', Name: 'Linux worker ' + malicious, OS: 'linux', Arch: 'amd64', Connectivity: 'connected', Version: '1.2.3', Enabled: true },
    { ID: 'worker-revoked', Name: revokedWorkerName, OS: 'linux', Arch: 'amd64', Connectivity: 'disconnected', Version: '1.2.3', Enabled: false },
  ],
  runtimes: [{ runtime_id: 'runtime-1' }],
  sessions: [...sessions, { session_id: 'hidden', name: 'Archived subagent', state: 'idle', archived: true }], pending_approvals: 1, queued_commands: 0,
  bot: { username: 'example_bot', display_name: 'Example bot ' + malicious, id: 12345, status: 'connected', webhook_status: 'active', webhook_url: 'https://gateway.example.com/tgw/api/v1/telegram/webhook', pending_updates: 0, allowed_user_count: 1, allowed_chat_count: 0, checked_at: time },
};

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, hasTouch: true });
  const errors = [];
  const consoleMessages = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => consoleMessages.push(message.text()));
  let dashboardStatus = 200;
  let loginStatus = 200;
  let browserID = 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa';
  let nearExpiry = false;
  let holdLoginFinish = false;
  let releaseLoginFinish = null;
  let markFinishStarted = null;
  let loginFinishStatus = 200;
  let loginFinishCalls = 0;
  let staleFreshAuth = false;
  let enrollmentPostStatus = 200;
  let enrollmentDeleteStatus = 204;
  let enrollmentNetworkFailure = '';
  let enrollmentInvalidURL = false;
  let enrollmentInvalidCode = false;
  let enrollmentInvalidID = false;
  let enrollmentURLOrigin = 'https://ADMIN.TEST:443';
  let enrollmentNumber = 0;
  const workerPurgeStatuses = [];
  const enrollmentResponses = [];
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
      if (request.method() !== 'GET') mutations.push({ path: url.pathname, method: request.method(), body: request.postData(), csrf: request.headers()['x-csrf-token'] });
      if (url.pathname.endsWith('/dashboard')) { body = data; status = dashboardStatus; dashboardReads++; }
      else if (url.pathname.endsWith('/login/options')) body = { passkey: true, password: false };
      else if (url.pathname.endsWith('/admin/password')) body = { enabled: false, username: '' };
      else if (url.pathname.endsWith('/admin/session')) {
        status = loginStatus;
        const now = await page.evaluate(() => Date.now());
        body = { authenticated: true, session_id: browserID, server_time: new Date(now).toISOString(), expires_at: new Date(now + (nearExpiry ? 240000 : 8 * 3600000)).toISOString(), reauthenticated_at: new Date(now - (staleFreshAuth ? 360000 : 0)).toISOString() };
      }
      else if (url.pathname.endsWith('/login/begin')) body = { ceremony_id: 'ceremony', publicKey: { challenge: 'dGVzdC1jaGFsbGVuZ2U', rpId: 'admin.test' } };
      else if (url.pathname.endsWith('/login/finish')) {
        loginFinishCalls++;
        if (holdLoginFinish) await new Promise(resolve => { releaseLoginFinish = resolve; markFinishStarted?.(); });
        status = loginFinishStatus;
        if (status === 200) { browserID = 'cccccccc-3333-4333-8333-cccccccccccc'; nearExpiry = false; staleFreshAuth = false; }
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
      else if (url.pathname.endsWith('/workers/worker-revoked/purge') && request.method() === 'DELETE') {
        status = workerPurgeStatuses.shift() ?? 204;
        if (status === 204) data.workers = data.workers.filter(worker => worker.ID !== 'worker-revoked');
        else body = status === 403 ? { code: 'reauthentication_required' } : { message: 'Unable to delete revoked worker.' };
      }
      else if (url.pathname.endsWith('/worker-enrollments') && request.method() === 'POST') {
        if (enrollmentNetworkFailure === 'POST') return route.abort('failed');
        status = enrollmentPostStatus;
        if (status === 200) {
          enrollmentNumber++;
          const now = await page.evaluate(() => Date.now());
          const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
          const code = enrollmentInvalidCode ? 'AAAAAAAAAAA0' : 'A'.repeat(10) + alphabet[Math.floor(enrollmentNumber / alphabet.length)] + alphabet[enrollmentNumber % alphabet.length];
          body = { enrollment_id: enrollmentInvalidID ? '00000000-0000-0000-0000-000000000000' : 'dddddddd-4444-4444-8444-' + String(enrollmentNumber).padStart(12, '0'), enrollment_url: (enrollmentInvalidURL ? 'https://invalid.example' : enrollmentURLOrigin) + '/tgw/enroll/#' + code, expires_at: new Date(now + 600000).toISOString(), service_access: JSON.parse(request.postData()).service_access };
          enrollmentResponses.push(body);
        } else body = { code: status === 403 ? 'reauthentication_required' : 'request_failed' };
      }
      else if (/\/worker-enrollments\/[0-9a-f-]+$/.test(url.pathname) && request.method() === 'DELETE') {
        if (enrollmentNetworkFailure === 'DELETE') return route.abort('failed');
        status = enrollmentDeleteStatus;
        if (status === 403) body = { code: 'reauthentication_required' };
      }
      return route.fulfill({ status, contentType: 'application/json', body: status === 204 ? '' : JSON.stringify(body) });
    }
    const filename = /\.(js|css|svg)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.svg') ? 'image/svg+xml' : 'text/html', headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(() => {
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async () => ({
      id: 'passkey', rawId: new Uint8Array([1, 2, 3]).buffer, type: 'public-key',
      response: { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer },
    }) });
    const getCredential = navigator.credentials.get;
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async (...args) => {
      if (window.failCredentials) throw new DOMException('Passkey was cancelled.', 'NotAllowedError');
      return getCredential(...args);
    } });
    Object.defineProperty(navigator.clipboard, 'writeText', { configurable: true, value: async text => {
      if (window.failClipboard) throw new Error('Clipboard denied');
      window.copiedEnrollmentURL = text;
    } });
  });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'test-csrf', url: 'https://admin.test/', secure: true, sameSite: 'Strict' }]);
  try {
    if (page.clock) await page.clock.install();
    await page.goto('https://admin.test/tgw/admin');
    await page.waitForSelector('#console:not([hidden])');
    const sectionIDs = ['telegram-bots-section', 'audio-section', 'session-section', 'worker-section', 'passkey-section', 'browser-session-section', 'password-section'];
    for (const id of sectionIDs) {
      const section = page.locator('#' + id);
      assert.equal(await section.evaluate(node => node.tagName), 'DETAILS', id + ' uses a native disclosure');
      assert.equal(await section.evaluate(node => node.open), false, id + ' starts collapsed');
      assert.equal(await section.locator(':scope > .section-content').isVisible(), false, id + ' hides its controls');
      assert.equal(await section.locator(':scope > summary button').count(), 0, id + ' summary contains only its heading');
      if (id === 'session-section') continue;
      await section.locator(':scope > summary').tap();
      assert.equal(await section.evaluate(node => node.open), true, id + ' opens with touch');
      await section.locator(':scope > summary').press('Space');
      assert.equal(await section.evaluate(node => node.open), false, id + ' closes from the keyboard');
      await section.locator(':scope > summary').press('Enter');
      assert.equal(await section.evaluate(node => node.open), true, id + ' opens from the keyboard');
    }
    await page.locator('#audio-section > summary').click();
    await page.locator('#password-section > summary').click();
    const sectionStates = () => page.locator('#console > .console-section').evaluateAll(sections => sections.map(section => ({ id: section.id, open: section.open })));
    const sessionSection = page.locator('#session-section');
    const sessionToggle = sessionSection.locator(':scope > summary');
    assert.equal(await sessionSection.evaluate(section => section.open), false, 'Sessions start collapsed');
    assert.equal(await page.locator('#session-search').isVisible(), false, 'Collapsed sessions hide their controls');
    await sessionToggle.tap();
    assert.equal(await sessionSection.evaluate(section => section.open), true, 'Touch opens sessions');
    assert.equal(await page.locator('#sessions').textContent(), '12');
    assert.equal(await page.locator('.session-card').count(), 10);
    assert.equal(await page.locator('.session-card h3').first().textContent(), fullName);
    const firstSession = page.locator('[data-session-id="session-0"]');
    assert.equal(await firstSession.evaluate(card => card.open), false, 'Each session starts collapsed');
    assert.equal(await firstSession.locator('.session-metrics').isVisible(), false, 'Session metrics are inside its disclosure');
    assert.equal(await firstSession.locator('.last-message').isVisible(), false, 'Session messages are inside its disclosure');
    await firstSession.locator(':scope > summary').press('Enter');
    assert.equal(await firstSession.evaluate(card => card.open), true, 'A session opens from the keyboard');
    assert.equal(await page.locator('.session-metrics dd').first().textContent(), '0', 'Known zero must not look unavailable');
    const partial = page.locator('[data-session-id="session-2"]');
    assert.equal(await partial.evaluate(card => card.open), false, 'Opening one session keeps other sessions collapsed');
    await partial.locator(':scope > summary').tap();
    assert.equal(await partial.locator('.session-metrics dd').nth(0).textContent(), '≥12', 'Partial prompt count is a lower bound');
    assert.equal(await partial.locator('.session-metrics dd').nth(1).textContent(), '≥11', 'Partial reply count is a lower bound');
    assert.equal(await partial.locator('.session-metrics dd').nth(2).textContent(), '42,000', 'Recorded cumulative tokens are independent of partial history');
    assert.match(await partial.innerText(), /Partial history · message counts may still be loading/);
    assert.equal(await page.locator('#bot-info a').getAttribute('href'), 'https://t.me/example_bot');
    assert.equal(await page.locator('#bot-info .bot-content').isVisible(), false, 'The primary bot fallback starts collapsed');
    await page.locator('#bot-info .bot-toggle').click();
    assert.match(await page.locator('#bot-info').innerText(), /Any chat with an allowed user/);
    assert.equal(await page.locator('#session-list img, #bot-info img, #worker-list img, #browser-sessions img').count(), 0, 'API data must not become HTML');
    assert.equal(await page.evaluate(() => window.injected), undefined);
    await page.waitForSelector('.browser-session');
    assert.equal(await page.locator('.browser-session').count(), 2);
    assert.match(await page.locator('#browser-sessions').innerText(), /Safari on iOS · This browser/);
    assert.equal(await page.locator('#browser-sessions img').count(), 0, 'Browser labels must remain plain text');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Mobile layout must fit viewport');
    await firstSession.locator('.session-details > summary').click();
    const beforeRefreshSections = await sectionStates();
    await page.locator('#refresh').click();
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.equal(await sessionSection.evaluate(section => section.open), true, 'Refresh preserves the expanded sessions section');
    assert.deepEqual(await sectionStates(), beforeRefreshSections, 'Manual refresh preserves all open and collapsed Admin sections');
    assert.equal(await firstSession.evaluate(card => card.open), true, 'Refresh preserves the expanded session');
    assert.equal(await page.locator('.session-card details').first().getAttribute('open'), '', 'Refresh preserves expanded details');
    assert.equal(await page.locator('[data-session-id="session-3"]').evaluate(card => card.open), false, 'Refresh preserves collapsed sessions');
    assert.equal(await page.locator('#bot-info .bot-toggle').getAttribute('aria-expanded'), 'true', 'Refresh preserves the expanded primary bot fallback');
    assert.match(await page.locator('.session-card').first().innerText(), /6,000 \/ 200,000 \(3%\)/);
    await Promise.all([nextDashboardResponse(), page.clock.runFor(15010)]);
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.equal(await firstSession.evaluate(card => card.open), true, 'The 15-second dashboard poll preserves expanded sessions');
    assert.deepEqual(await sectionStates(), beforeRefreshSections, 'The 15-second poll preserves every Admin section state');
    if (process.env.ADMIN_UI_SCREENSHOTS) {
      fs.mkdirSync(process.env.ADMIN_UI_SCREENSHOTS, { recursive: true });
      await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-mobile.png'), fullPage: true });
    }
    await page.locator('#session-next').click();
    assert.equal(await page.locator('.session-card').count(), 2);
    assert.equal(await page.locator('#session-page').textContent(), 'Page 2 of 2');
    await page.locator('#session-prev').click();
    assert.equal(await firstSession.evaluate(card => card.open), true, 'Pagination preserves expanded sessions');
    assert.equal(await firstSession.locator('.session-details').evaluate(details => details.open), true, 'Pagination preserves the token breakdown');
    await page.locator('#session-next').click();
    await page.locator('#session-search').fill('Linux worker');
    assert.equal(await page.locator('.session-card').count(), 10, 'Search resets page');
    await page.locator('#session-state').selectOption('running');
    assert.equal(await page.locator('.session-card').count(), 1);
    await page.locator('#session-search').fill('not-present');
    assert.match(await page.locator('#session-list').innerText(), /No sessions match/);
    await page.locator('#session-search').fill('Session 1');
    await page.locator('#session-state').selectOption('');
    const unknown = page.locator('[data-session-id="session-1"]');
    await unknown.locator(':scope > summary').click();
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
    // Existing worker token rotation still reveals its secret only once.
    const dialogs = [];
    let confirmWorkerDeletion = false;
    page.on('dialog', async dialog => {
      dialogs.push({ message: dialog.message(), value: dialog.defaultValue() });
      if (dialog.message().startsWith('Permanently delete revoked worker') && !confirmWorkerDeletion) await dialog.dismiss();
      else await dialog.accept('');
    });
    // The POST and token dialog finish before load() disables Refresh. An
    // already-enabled button does not prove that the resulting refresh ran.
    await Promise.all([
      nextDashboardResponse(),
      page.getByRole('button', { name: 'Rotate token', exact: true }).click(),
    ]);
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.ok(mutations.some(entry => entry.path.endsWith('/worker-1/rotate-token') && entry.method === 'POST'));
    assert.ok(dialogs.some(dialog => dialog.value === 'one-time-secret'));
    assert.equal((await page.locator('body').innerText()).includes('one-time-secret'), false);
    const activeWorker = page.locator('#worker-list .worker').filter({ hasText: 'Worker ID: worker-1' });
    const revokedWorker = page.locator('#worker-list .worker').filter({ hasText: 'Worker ID: worker-revoked' });
    const deleteWorker = revokedWorker.getByRole('button', { name: 'Delete', exact: true });
    const purgeCalls = () => mutations.filter(entry => entry.path.endsWith('/workers/worker-revoked/purge'));
    assert.equal(await activeWorker.getByRole('button', { name: 'Delete', exact: true }).count(), 0, 'Enabled workers cannot be deleted');
    assert.equal(await deleteWorker.count(), 1, 'Revoked workers can be deleted');
    assert.equal(await revokedWorker.locator('.badge').textContent(), 'revoked', 'Delete retains the revoked badge');
    const beforeCancelledDelete = mutations.length;
    const beforeCancelledDeleteReads = dashboardReads;
    await deleteWorker.click();
    await page.waitForFunction(control => !control.disabled, await deleteWorker.elementHandle());
    const deleteConfirmation = dialogs.at(-1).message;
    assert.ok(deleteConfirmation.includes(revokedWorkerName), 'Deletion confirmation identifies the worker as plain text');
    assert.match(deleteConfirmation, /all related gateway database records, including sessions, history, and routing/);
    assert.match(deleteConfirmation, /Project files and native Codex sessions are untouched/);
    assert.match(deleteConfirmation, /cannot be undone/);
    assert.equal(mutations.length, beforeCancelledDelete, 'Cancelling deletion sends no mutation');
    assert.equal(dashboardReads, beforeCancelledDeleteReads, 'Cancelling deletion does not refresh');
    confirmWorkerDeletion = true;
    // Cancelling fresh authentication preserves the worker without a delete request.
    staleFreshAuth = true;
    await page.evaluate(() => { window.failCredentials = true; });
    await deleteWorker.click();
    await page.waitForSelector('#dashboard-error:not([hidden])');
    assert.equal(purgeCalls().length, 0, 'Cancelled fresh authentication sends no deletion');
    assert.equal(await revokedWorker.count(), 1);
    await page.evaluate(() => { window.failCredentials = false; });
    staleFreshAuth = false;
    // A failed purge retains the record and reports its error without refreshing.
    workerPurgeStatuses.push(503);
    const beforeFailedDeleteReads = dashboardReads;
    await deleteWorker.click();
    await page.waitForFunction(() => document.getElementById('dashboard-error').textContent === 'Unable to delete revoked worker.');
    assert.equal(purgeCalls().length, 1);
    assert.equal(dashboardReads, beforeFailedDeleteReads, 'Failed deletion does not refresh');
    assert.equal(await revokedWorker.count(), 1, 'Failed deletion preserves the worker');
    // An explicit fresh-auth rejection reauthenticates once, then refreshes on success.
    workerPurgeStatuses.push(403, 204);
    const beforeDeleteLogin = loginFinishCalls;
    await deleteWorker.click();
    await page.waitForFunction(() => ![...document.querySelectorAll('#worker-list .worker')].some(worker => worker.textContent.includes('Worker ID: worker-revoked')));
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
    assert.equal(loginFinishCalls, beforeDeleteLogin + 1);
    assert.equal(purgeCalls().length, 3);
    assert.ok(purgeCalls().every(entry => entry.method === 'DELETE' && entry.csrf === 'test-csrf'));
    assert.ok(purgeCalls().every(entry => JSON.parse(entry.body).confirmed === true));
    assert.equal(await page.locator('#workers').textContent(), '1', 'Successful deletion removes the worker from the dashboard');
    assert.equal(await activeWorker.count(), 1);
    const enrollmentCalls = () => mutations.filter(entry => entry.path.includes('/worker-enrollments'));
    const waitForEnrollment = () => page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-link').hidden);
    // Keyboard activation opens a modal and immediately creates a restricted URL.
    await page.locator('#new-worker').focus();
    await page.keyboard.press('Enter');
    await waitForEnrollment();
    const firstEnrollment = enrollmentResponses.at(-1);
    assert.equal(await page.locator('#enrollment-url').inputValue(), firstEnrollment.enrollment_url);
    assert.match(await page.locator('#enrollment-url').inputValue(), /^https:\/\/ADMIN\.TEST:443\//, 'A same-origin URL is preserved verbatim');
    enrollmentURLOrigin = 'https://admin.test';
    assert.equal(await page.locator('#enrollment-code').textContent(), firstEnrollment.enrollment_url.split('#')[1]);
    assert.match(await page.locator('#enrollment-status').textContent(), /expires in (10:00|9:59)/);
    assert.deepEqual(JSON.parse(enrollmentCalls().at(-1).body), { service_access: 'restricted' });
    assert.equal(enrollmentCalls().at(-1).csrf, 'test-csrf');
    assert.equal(await page.locator('#workers').textContent(), '1', 'An unused URL does not create a placeholder worker');
    assert.equal(dialogs.some(dialog => /display name/i.test(dialog.message)), false, 'Only the installer asks for a worker name');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth && document.getElementById('worker-enrollment').scrollWidth <= document.getElementById('worker-enrollment').clientWidth), true, 'Mobile enrollment dialog fits');
    if (process.env.ADMIN_UI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-enrollment-mobile.png') });
    await page.locator('#enrollment-copy').click();
    assert.equal(await page.evaluate(() => window.copiedEnrollmentURL), firstEnrollment.enrollment_url);
    await page.evaluate(() => { window.failClipboard = true; });
    await page.locator('#enrollment-copy').click();
    assert.match(await page.locator('#enrollment-copy-status').textContent(), /URL selected/);
    assert.equal(await page.evaluate(() => document.activeElement.id), 'enrollment-url');
    assert.equal(await page.evaluate(() => { const input = document.getElementById('enrollment-url'); return input.selectionEnd - input.selectionStart; }), firstEnrollment.enrollment_url.length);
    assert.equal(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }).includes('/tgw/enroll/')), false, 'Enrollment URL is never stored');
    const profileChangeStart = enrollmentCalls().length;
    await page.locator('#enrollment-access').selectOption('full');
    await waitForEnrollment();
    const profileCalls = enrollmentCalls().slice(profileChangeStart);
    assert.equal(profileCalls[0].method, 'DELETE');
    assert.ok(profileCalls[0].path.endsWith('/' + firstEnrollment.enrollment_id));
    assert.equal(profileCalls[1].method, 'POST');
    assert.deepEqual(JSON.parse(profileCalls[1].body), { service_access: 'full' });
    const fullEnrollment = enrollmentResponses.at(-1);
    assert.notEqual(fullEnrollment.enrollment_url, firstEnrollment.enrollment_url);
    // A failed revoke hides the old URL and prevents creation until an explicit retry.
    enrollmentDeleteStatus = 503;
    const beforeFailedRevoke = enrollmentCalls().length;
    await page.locator('#enrollment-access').selectOption('restricted');
    await page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-error').hidden);
    assert.equal(enrollmentCalls().length, beforeFailedRevoke + 1);
    assert.equal(await page.locator('#enrollment-url').inputValue(), '');
    assert.equal(await page.locator('#enrollment-link').isVisible(), false);
    assert.match(await page.locator('#enrollment-status').textContent(), /may still be valid/);
    enrollmentDeleteStatus = 204;
    await page.locator('#enrollment-new').click();
    await waitForEnrollment();
    assert.deepEqual(enrollmentCalls().slice(-2).map(entry => entry.method), ['DELETE', 'POST']);
    // A link consumed by the installer no longer needs revocation.
    enrollmentDeleteStatus = 404;
    await page.locator('#enrollment-new').click();
    await waitForEnrollment();
    assert.deepEqual(enrollmentCalls().slice(-2).map(entry => entry.method), ['DELETE', 'POST'], 'Confirmed consumed URLs can be replaced');
    enrollmentDeleteStatus = 204;
    // Close and Escape keep an unused URL valid, while clearing its DOM and memory.
    const beforeClose = enrollmentCalls().length;
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#worker-enrollment').evaluate(dialog => dialog.open), false);
    assert.equal(await page.locator('#enrollment-url').inputValue(), '');
    assert.equal(enrollmentCalls().length, beforeClose);
    assert.equal(await page.evaluate(() => workerEnrollment), null);
    await page.locator('#new-worker').click();
    await waitForEnrollment();
    const cancellation = enrollmentResponses.at(-1);
    await page.locator('#enrollment-cancel').click();
    await page.waitForFunction(() => !document.getElementById('worker-enrollment').open);
    assert.ok(enrollmentCalls().at(-1).path.endsWith('/' + cancellation.enrollment_id));
    assert.equal(enrollmentCalls().at(-1).method, 'DELETE');
    // Both HTTP and transport failures produce one POST and require manual retry.
    for (const failure of ['http', 'network', 'fresh-auth']) {
      enrollmentPostStatus = failure === 'http' ? 503 : failure === 'fresh-auth' ? 403 : 200;
      enrollmentNetworkFailure = failure === 'network' ? 'POST' : '';
      const beforeFailure = enrollmentCalls().length;
      const beforeLogin = loginFinishCalls;
      await page.locator('#new-worker').click();
      await page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-error').hidden);
      assert.equal(enrollmentCalls().length, beforeFailure + 1, failure + ' never automatically replays creation');
      assert.equal(await page.locator('#enrollment-link').isVisible(), false);
      if (failure === 'fresh-auth') assert.equal(loginFinishCalls, beforeLogin + 1, 'Fresh-auth rejection confirms passkey without replay');
      await page.locator('#enrollment-close').click();
    }
    enrollmentPostStatus = 200; enrollmentNetworkFailure = '';
    // Revocation has the same no-replay rule, including fresh-auth rejection.
    for (const failure of ['network', 'fresh-auth']) {
      await page.locator('#new-worker').click();
      await waitForEnrollment();
      enrollmentNetworkFailure = failure === 'network' ? 'DELETE' : '';
      enrollmentDeleteStatus = failure === 'fresh-auth' ? 403 : 204;
      const beforeFailure = enrollmentCalls().length;
      const beforeLogin = loginFinishCalls;
      await page.locator('#enrollment-cancel').click();
      await page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-error').hidden);
      assert.equal(enrollmentCalls().length, beforeFailure + 1, failure + ' never automatically replays revocation');
      assert.equal(await page.locator('#enrollment-link').isVisible(), false);
      if (failure === 'fresh-auth') assert.equal(loginFinishCalls, beforeLogin + 1);
      enrollmentNetworkFailure = ''; enrollmentDeleteStatus = 204;
      await page.locator('#enrollment-cancel').click();
      await page.waitForFunction(() => !document.getElementById('worker-enrollment').open);
    }
    // Cancelled fresh authentication sends no enrollment mutation.
    staleFreshAuth = true;
    await page.evaluate(() => { window.failCredentials = true; });
    const beforeCancelledAuth = enrollmentCalls().length;
    await page.locator('#new-worker').click();
    await page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-error').hidden);
    assert.equal(enrollmentCalls().length, beforeCancelledAuth);
    await page.locator('#enrollment-close').click();
    await page.evaluate(() => { window.failCredentials = false; });
    staleFreshAuth = false;
    // Unsafe origins, mistyped codes and missing identities never become links.
    for (const invalid of ['origin', 'code', 'identity']) {
      enrollmentInvalidURL = invalid === 'origin'; enrollmentInvalidCode = invalid === 'code'; enrollmentInvalidID = invalid === 'identity';
      await page.locator('#new-worker').click();
      await page.waitForFunction(() => !enrollmentBusy && !document.getElementById('enrollment-error').hidden);
      assert.equal(await page.locator('#enrollment-url').inputValue(), '');
      assert.match(await page.locator('#enrollment-error').textContent(), /invalid enrollment URL/);
      enrollmentInvalidURL = false; enrollmentInvalidCode = false; enrollmentInvalidID = false;
      await page.locator('#enrollment-cancel').click();
      await page.waitForFunction(() => !document.getElementById('worker-enrollment').open);
    }
    if (page.clock) {
      await page.locator('#new-worker').click();
      await waitForEnrollment();
      const beforeExpiry = enrollmentCalls().length;
      await page.clock.fastForward(600001);
      await page.waitForFunction(() => document.getElementById('enrollment-link').hidden);
      assert.match(await page.locator('#enrollment-status').textContent(), /expired/);
      assert.equal(await page.locator('#enrollment-url').inputValue(), '');
      await page.locator('#enrollment-new').click();
      await waitForEnrollment();
      assert.equal(enrollmentCalls().length, beforeExpiry + 1, 'Expired URLs need no revoke before regeneration');
      await page.locator('#enrollment-cancel').click();
      await page.waitForFunction(() => !document.getElementById('worker-enrollment').open);
    }
    assert.equal(mutations.some(entry => entry.path.endsWith('/workers') && entry.method === 'POST'), false, 'Enrollment UI never uses legacy worker creation');
    assert.equal(paths.some(path => path.startsWith('/tgw/enroll/')), false, 'Showing or copying a URL never opens or redeems it');
    assert.equal(consoleMessages.some(message => enrollmentResponses.some(response => message.includes(response.enrollment_url) || message.includes(response.enrollment_url.split('#')[1]))), false, 'Enrollment URL and code stay out of console output');
    assert.equal((await page.locator('body').innerText()).includes('one-time-secret'), false);
    await Promise.all([
      nextDashboardResponse(),
      page.locator('.browser-session').nth(1).getByRole('button', { name: 'Sign out', exact: true }).click(),
    ]);
    await page.waitForFunction(() => document.querySelectorAll('.browser-session').length === 1);
    assert.ok(mutations.some(entry => entry.path.endsWith('/sessions/' + otherBrowserID) && entry.method === 'DELETE'));
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    const desktopSections = await sectionStates();
    for (const { id, open } of desktopSections) {
      const summary = page.locator('#' + id + ' > summary');
      await summary.press('Space');
      assert.equal(await page.locator('#' + id).evaluate(section => section.open), !open, id + ' toggles by keyboard on desktop');
      await summary.press('Enter');
    }
    assert.deepEqual(await sectionStates(), desktopSections, 'Desktop keyboard toggles restore every Admin section state');
    await sessionToggle.focus();
    await page.keyboard.press('Space');
    assert.equal(await sessionSection.evaluate(section => section.open), false, 'Keyboard collapses sessions');
    assert.equal(await page.locator('#session-search').isVisible(), false);
    await page.keyboard.press('Enter');
    assert.equal(await sessionSection.evaluate(section => section.open), true, 'Keyboard opens sessions');
    assert.equal(await page.locator('#session-search').isVisible(), true);
    await page.locator('#new-worker').click();
    await waitForEnrollment();
    assert.equal(await page.locator('#worker-enrollment').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true, 'Desktop enrollment dialog fits');
    if (process.env.ADMIN_UI_SCREENSHOTS) await page.screenshot({ path: path.join(process.env.ADMIN_UI_SCREENSHOTS, 'admin-enrollment-desktop.png') });
    await page.locator('#enrollment-cancel').click();
    await page.waitForFunction(() => !document.getElementById('worker-enrollment').open);
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
      await page.locator('.session-auth-banner button:not(.session-auth-password)').click();
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
    await page.locator('#new-worker').click();
    await waitForEnrollment();
    dashboardStatus = 401;
    await page.evaluate(() => load(false));
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#console').getAttribute('hidden'), '');
    assert.equal(await page.locator('.session-card').count(), 0, 'Session expiry removes session data');
    assert.equal(await page.locator('.browser-session').count(), 0, 'Session expiry removes browser-session data');
    assert.equal(await page.locator('#worker-enrollment').evaluate(dialog => dialog.open), false, 'Session expiry closes enrollment');
    assert.equal(await page.locator('#enrollment-url').inputValue(), '', 'Session expiry clears the enrollment URL');
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
      assert.equal(await sessionSection.evaluate(section => section.open), false, 'Sessions start collapsed on desktop too');
      assert.equal(await page.locator('#console > .console-section[open]').count(), 0, 'Every Admin section starts collapsed on desktop');
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
    console.log('Admin browser checks passed: all Admin sections collapsed by default, mobile touch/desktop keyboard controls, section state retained through manual/15-second refreshes, session statistics/filters, safe rendering, revoked-worker deletion, enrollment URL lifecycle, no secret persistence, renewal/poll races, failed-finish recovery and session expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const sessionID = '11111111-2222-4333-8444-555555555555';
const selected = { session_id: sessionID, codex_thread_id: 'thread', worker_name: 'Test worker', runtime_name: 'Codex', name: 'Restored conversation', cwd: '/projects/demo', state: 'idle', stats: { model: 'model', reasoning_effort: 'high' } };
(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 850 } });
  let now = Date.now(), expiry = now + 600000, auth = true, loginID = 1, finishCount = 0, sessionChecks = 0;
  const errors = [], posts = [], drafts = new Map(), rotationSockets = [];
  let finishFails = false, activePage = null;
  await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'csrf-1', url: 'https://auth.test', secure: true, sameSite: 'Strict' }]);
  await context.route('https://auth.test/**', async route => {
    const url = new URL(route.request().url());
    const json = (value, status = 200, headers = {}) => route.fulfill({ status, headers, contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname === '/tgw/api/v1/admin/session') {
      sessionChecks++;
      return json({ authenticated: true, owner_id: 'owner', session_id: 'login-' + loginID, server_time: new Date(now).toISOString(), expires_at: new Date(expiry).toISOString(), reauthenticated_at: new Date(now).toISOString() }, auth && now < expiry ? 200 : 401);
    }
    if (url.pathname === '/tgw/api/v1/admin/login/begin') { posts.push({ route: 'begin', csrf: route.request().headers()['x-csrf-token'] }); return json({ ceremony_id: 'ceremony', publicKey: { challenge: 'AQID', rpId: 'auth.test', allowCredentials: [] } }); }
    if (url.pathname === '/tgw/api/v1/admin/login/finish') {
      if (activePage) {
        rotationSockets.push(await activePage.evaluate(() => window.authSockets.filter(socket => socket.readyState === 1).length));
        // Simulate revocation frames already queued by the old connection.
        await activePage.evaluate(() => { for (const socket of window.authSockets) socket.originalClose?.({ code: 4401 }); });
      }
      if (finishFails) { finishFails = false; return json({}, 503); }
      posts.push({ route: 'finish', csrf: route.request().headers()['x-csrf-token'], body: route.request().postDataJSON() }); finishCount++; loginID++; auth = true; expiry = now + 28800000;
      return json({ ok: true }, 200, { 'Set-Cookie': '__Host-telegramgw-csrf=csrf-' + loginID + '; Path=/; Secure; SameSite=Strict' });
    }
    if (url.pathname.startsWith('/tgw/api/v1/webui/drafts/')) {
      if (!auth) return json({}, 401);
      const id = path.basename(url.pathname), method = route.request().method();
      if (method === 'DELETE') { drafts.delete(id); return route.fulfill({ status: 204 }); }
      if (method === 'PUT') { const value = { ...route.request().postDataJSON(), expires_at: new Date(now + 1800000).toISOString() }; drafts.set(id, value); return json(value); }
      return json(drafts.get(id) || {}, drafts.has(id) ? 200 : 404);
    }
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions: [selected] }, auth ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await context.addInitScript(({ sessionID }) => {
    window.authSockets = []; window.authFrames = []; window.passkeyMode = 'success'; window.holdSend = false;
    Object.defineProperty(navigator, 'credentials', { configurable: true, value: { get: async () => {
      if (window.passkeyMode === 'cancel') throw new DOMException('Cancelled', 'NotAllowedError');
      return { id: 'passkey', rawId: new Uint8Array([1, 2]), type: 'public-key', response: { clientDataJSON: new Uint8Array([3]), authenticatorData: new Uint8Array([4]), signature: new Uint8Array([5]), userHandle: null }, getClientExtensionResults: () => ({}) };
    } } });
    window.testHidden = false;
    Object.defineProperty(document, 'hidden', { get: () => window.testHidden });
    window.visibility = value => { window.testHidden = value; document.dispatchEvent(new Event('visibilitychange')); };
    const items = Array.from({ length: 60 }, (_, i) => ({ turnId: 'turn', item: { id: 'message-' + i, type: 'agentMessage', text: 'Saved message ' + i + '\n\n' + 'Conversation detail for scroll restoration. '.repeat(22), createdAt: 1780000000 + i } })).reverse();
    class Socket {
      static OPEN = 1;
      constructor(url) {
        this.activity = new URL(url).pathname.endsWith('/activity'); this.readyState = 0; this.sequence = 0;
        window.authSockets.push(this);
        setTimeout(() => { if (this.readyState === 3) return; this.readyState = 1; this.originalClose = this.onclose; this.emit(this.activity ? { type: 'activity_snapshot', version: 2, sequence: 0, revision: 1, sessions: [{ session_id: sessionID, state: 'idle', worker_connectivity: 'connected', pending_questions: 0 }] } : { type: 'ready' }); if (this.activity) this.heartbeat = setInterval(() => this.emit({ type: 'heartbeat', version: 2, sequence: 0 }), 15000); }, 5);
      }
      emit(value) { if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; clearInterval(this.heartbeat); this.onclose?.({ code }); }
      send(raw) {
        const frame = JSON.parse(raw); window.authFrames.push(frame);
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: 'thread' }, model: 'model', reasoningEffort: 'high' };
        if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'completed', startedAt: 1780000000 }], nextCursor: null };
        if (frame.method === 'thread/items/list') { const offset = Number(frame.params.cursor || 0); result = { data: items.slice(offset, offset + 20), nextCursor: offset + 20 < items.length ? String(offset + 20) : null }; }
        if (frame.method === 'gateway/questions') result = { questions: [] };
        if (frame.method === 'model/list') result = { data: [] };
        if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        if (frame.method === 'turn/start' && window.holdSend) return;
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    }
    window.WebSocket = Socket;
  }, { sessionID });
  const page = await context.newPage(); activePage = page; page.on('pageerror', error => errors.push(error.message));
  await page.clock.install({ time: now });
  const ready = async target => { await target.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden); };
  const viewerCount = () => page.evaluate(() => window.authSockets.filter(value => !value.activity).length);
  const anchor = target => target.evaluate(() => { const view = document.querySelector('#transcript'); const item = [...document.querySelector('#messages').children].find(row => row.getBoundingClientRect().bottom > view.getBoundingClientRect().top); return { id: item?.dataset.itemId, offset: Math.round(item.getBoundingClientRect().top - view.getBoundingClientRect().top) }; });
  try {
    await page.goto('https://auth.test/tgw/webui/?session_id=' + sessionID); await ready(page);
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true);
    await page.locator('#prompt').fill('An unsent private draft');
    now += 300050; await page.clock.fastForward(300050);
    await page.waitForSelector('.session-auth-banner:not([hidden])');
    await page.evaluate(() => { window.passkeyMode = 'cancel'; });
    await page.locator('.session-auth-banner button').click();
    await page.waitForFunction(() => document.querySelector('.session-auth-status').textContent.includes('cancelled'));
    assert.equal(finishCount, 0, 'Cancelling a passkey does not renew authentication');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft');
    await page.locator('#older').click();
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    await page.evaluate(() => { const view = document.querySelector('#transcript'); view.dispatchEvent(new WheelEvent('wheel', { deltaY: -1 })); view.scrollTop = 1600; });
    const before = await anchor(page);
    finishFails = true;
    const socketsBeforeFailure = await viewerCount();
    await page.evaluate(() => { window.passkeyMode = 'success'; });
    await page.locator('.session-auth-banner button').click();
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, socketsBeforeFailure); await ready(page);
    assert.equal(finishCount, 0, 'A failed finish does not create a login');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft', 'Finish failure reconnects the still-authorized view without losing its draft');
    assert.equal(await page.locator('.session-auth-banner').isVisible(), true);
    const socketsBefore = await viewerCount();
    await page.evaluate(() => { window.passkeyMode = 'success'; });
    await page.locator('.session-auth-banner button').click();
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, socketsBefore); await ready(page);
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    assert.equal(await viewerCount(), socketsBefore + 1, 'Renewal reconnects the selected session using the rotated cookie');
    assert.deepEqual(await anchor(page), before, 'Renewal restores the earlier loaded history anchor');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft', 'Early renewal preserves an already authorized in-memory draft');
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true);
    assert.equal(finishCount, 1);
    assert.ok(rotationSockets.every(count => count === 0), 'Both old sockets detach before server cookie rotation; queued revocation callbacks cannot cancel login');
    assert.equal(posts.at(-1).body.credential.response.signature, 'BQ');
    assert.equal(await page.evaluate(() => document.cookie.includes('csrf-2')), true);

    await page.locator('#open-settings').click();
    auth = false;
    await page.evaluate(() => window.visibility(false));
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#settings-dialog').isHidden(), true, 'Expired authentication closes Settings so sign-in remains reachable');
    assert.equal(await page.locator('#messages article').count(), 0);
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(await page.locator('.session-button').count(), 0);
    assert.equal(await page.evaluate(() => window.authSockets.every(socket => socket.readyState === 3)), true);
    assert.equal(await page.evaluate(() => Object.keys(sessionStorage).length), 0, 'Default expiry retains no drafts or conversation in browser storage');
    await page.locator('#auth-login').click(); await ready(page);
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    assert.deepEqual(await anchor(page), before, 'Expired login resumes the selected session and previous scroll anchor');
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(posts.at(-1).csrf, 'csrf-2', 'Renewal reads the current CSRF cookie for each ceremony');

    await page.evaluate(() => { window.holdSend = true; });
    await page.locator('#prompt').fill('A prompt with an uncertain result'); await page.locator('#send').click();
    await page.waitForFunction(() => window.authFrames.some(frame => frame.method === 'turn/start'));
    const sends = await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length);
    auth = false; await page.evaluate(() => window.authSockets.findLast(socket => !socket.activity).close(4401));
    await page.waitForSelector('#auth:not([hidden])'); await page.locator('#auth-login').click(); await ready(page);
    assert.equal(await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length), sends, 'Unlock never replays an uncertain prompt');
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Nothing was resent'));
    assert.equal(await page.locator('#prompt').inputValue(), '');

    await page.locator('#prompt').fill('A draft explicitly saved for recovery');
    await page.locator('#open-settings').click();
    await page.locator('#draft-recovery-enabled').check();
    await page.waitForFunction(() => document.querySelector('#draft-recovery-status').textContent.includes('is on'));
    await page.locator('#close-settings').click();
    await page.locator('#open-settings').click();
    assert.equal(await page.locator('#draft-recovery-enabled').isChecked(), true, 'Closing and reopening Settings preserves the recovery preference');
    while (!drafts.size) await new Promise(resolve => setTimeout(resolve, 10));
    const encrypted = [...drafts.values()][0].ciphertext;
    assert.ok(!encrypted.includes('explicitly saved'));
    assert.ok(!(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).includes('explicitly saved'), 'No plaintext draft enters browser storage');
    auth = false; await page.evaluate(() => window.visibility(false)); await page.waitForSelector('#auth:not([hidden])');
    await page.locator('#auth-login').click(); await ready(page);
    await page.waitForSelector('#draft-recovery-offer:not([hidden])');
    await page.locator('#open-settings').click();
    assert.equal(await page.locator('#draft-recovery-enabled').isChecked(), true, 'The Settings recovery preference survives renewed authentication');
    await page.locator('#close-settings').click();
    assert.equal(await page.locator('#prompt').inputValue(), '', 'Recovery always waits for an explicit restore action');
    await page.locator('#restore-drafts').click();
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft explicitly saved for recovery');
    assert.equal(await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length), sends, 'Restoring a draft never submits it');
    await page.locator('#prompt').fill('An uncertain encrypted prompt');
    await page.locator('#send').click();
    await page.waitForFunction(count => window.authFrames.filter(frame => frame.method === 'turn/start').length > count, sends);
    auth = false; await page.evaluate(() => window.authSockets.findLast(socket => !socket.activity).close(4401)); await page.waitForSelector('#auth:not([hidden])');
    await page.locator('#auth-login').click(); await ready(page);
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(await page.locator('#draft-recovery-offer').isHidden(), true, 'A send with unknown outcome cannot reappear as a saved unsent draft');
    await page.locator('#open-settings').click();
    await page.locator('#draft-recovery-enabled').uncheck();
    await page.locator('#close-settings').click();

    const other = await context.newPage(); other.on('pageerror', error => errors.push(error.message));
    await other.clock.install({ time: now }); await other.goto('https://auth.test/tgw/webui/?session_id=' + sessionID); await ready(other);
    const otherBefore = await other.evaluate(() => window.authSockets.filter(socket => !socket.activity).length);
    // Simulate a login in an operations-console tab. Its non-secret hint must
    // cause both web views to fetch the authoritative new session themselves.
    loginID++; expiry = now + 28800000;
    const primaryBeforeBroadcast = await viewerCount();
    await other.evaluate(() => { const channel = new BroadcastChannel('codex-gateway-auth'); channel.postMessage({ kind: 'renewed', nonce: 'test' }); channel.close(); });
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, primaryBeforeBroadcast);
    await ready(page);
    await other.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, otherBefore);
    assert.ok(sessionChecks > 2, 'Every tab verifies renewal hints against the server');
    auth = false;
    await other.evaluate(() => { const channel = new BroadcastChannel('codex-gateway-auth'); channel.postMessage({ kind: 'logout' }); channel.close(); });
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#messages article').count(), 0, 'Logout in another tab clears the transcript');
    await other.close();

    await page.setViewportSize({ width: 390, height: 844 });
    await page.locator('#auth-login').click(); await ready(page);
    await page.evaluate(() => window.visibility(true));
    const checksBefore = sessionChecks; auth = false;
    await page.evaluate(() => window.visibility(false)); await page.waitForSelector('#auth:not([hidden])');
    assert.ok(sessionChecks > checksBefore, 'Mobile foreground always rechecks server authentication');
    await page.locator('#auth-login').click(); await ready(page);
    await page.evaluate(() => window.visibility(true));
    // OS sleep can advance wall time while performance.now is paused. The
    // foreground handler must clear content before waiting for a network check.
    now = expiry + 1000; auth = false; await page.clock.setSystemTime(now);
    await page.evaluate(() => window.visibility(false));
    assert.equal(await page.locator('#messages article').count(), 0, 'Suspended mobile expiry fails closed using wall time');
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    assert.deepEqual(errors, []);
    console.log('Web UI auth checks passed: warning, passkey cancel/finish-failure/rotation-race/renewal, CSRF rotation, history anchor restore, expiry erasure/Settings dismissal, encrypted opt-in/Settings preference persistence/explicit recovery/no uncertain-send replay, cross-tab renewal/logout, mobile foreground and suspended-clock expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

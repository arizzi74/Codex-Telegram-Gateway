#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');

(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 850 } });
  const state = { enabled: false, username: '', password: '', authenticated: false, method: 'passkey', id: 0, nearExpiry: false, stale: false, csrf: 'password-csrf' };
  const posts = [], mutations = [], errors = [], logs = [];
  const origin = 'https://password.test';
  let optionsMode = 'valid', holdPassword = false, releasePassword = null, passwordFailure = 0;
  function signedIn(method) { state.authenticated = true; state.method = method; state.id++; state.nearExpiry = false; state.stale = false; }
  await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'password-csrf', url: origin, secure: true, sameSite: 'Strict' }]);
  await context.route(origin + '/**', async route => {
    const request = route.request(), url = new URL(request.url()), method = request.method();
    const json = (value, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname === '/tgw/api/v1/admin/login/options') {
      if (optionsMode === 'failure') return json({}, 503);
      return json(optionsMode === 'generic' ? {} : { passkey: true, password: state.enabled });
    }
    if (url.pathname === '/tgw/api/v1/admin/session') {
      const now = Date.now();
      return json({ authenticated: true, owner_id: 'gateway-owner', session_id: 'login-' + state.id, authentication_method: state.method,
        server_time: new Date(now).toISOString(), expires_at: new Date(now + (state.nearExpiry ? 240000 : 28800000)).toISOString(), reauthenticated_at: new Date(now - (state.stale ? 360000 : 0)).toISOString() }, state.authenticated ? 200 : 401);
    }
    if (url.pathname === '/tgw/api/v1/admin/login/begin') return json({ ceremony_id: 'passkey', publicKey: { challenge: 'AQID', rpId: 'password.test' } });
    if (url.pathname === '/tgw/api/v1/admin/login/finish') { signedIn('passkey'); return json({ ok: true }); }
    if (url.pathname === '/tgw/api/v1/admin/login/password') {
      const body = request.postDataJSON();
      posts.push({ body, csrf: request.headers()['x-csrf-token'] });
      assert.equal(request.headers()['x-csrf-token'], state.csrf, 'Sign-in reads the latest CSRF cookie');
      if (holdPassword) await new Promise(resolve => { releasePassword = resolve; });
      if (passwordFailure) { const status = passwordFailure; passwordFailure = 0; return json({}, status); }
      if (!state.enabled || body.username !== state.username || body.password !== state.password) return json({ code: 'invalid_credentials' }, 401);
      signedIn('password'); return json({ ok: true });
    }
    if (url.pathname === '/tgw/api/v1/admin/password') {
      if (!state.authenticated) return json({}, 401);
      if (method === 'GET') return json({ enabled: state.enabled, username: state.username });
      mutations.push({ method, body: request.postDataJSON(), csrf: request.headers()['x-csrf-token'] });
      assert.equal(request.headers()['x-csrf-token'], state.csrf);
      if (method === 'PUT') { const body = request.postDataJSON(); state.enabled = true; state.username = body.username; state.password = body.password; }
      else { state.enabled = false; state.username = ''; state.password = ''; }
      if (state.method === 'password') {
        state.authenticated = false; state.csrf = 'password-csrf-' + state.id;
        return route.fulfill({ status: method === 'DELETE' ? 204 : 200, contentType: 'application/json', headers: { 'Set-Cookie': '__Host-telegramgw-csrf=' + state.csrf + '; Path=/; Secure; SameSite=Strict' }, body: method === 'DELETE' ? '' : JSON.stringify({ enabled: state.enabled, username: state.username }) });
      }
      return method === 'DELETE' ? route.fulfill({ status: 204 }) : json({ enabled: state.enabled, username: state.username });
    }
    if (url.pathname === '/tgw/api/v1/admin/dashboard') return json({ workers: [], runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0 }, state.authenticated ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/admin/passkeys') return json([]);
    if (url.pathname === '/tgw/api/v1/admin/sessions') return json({ sessions: [] });
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ workers: [], runtimes: [], sessions: [] }, state.authenticated ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
    if (url.pathname.startsWith('/tgw/api/')) return json({});
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : url.pathname.includes('/webui') ? 'webui.html' : 'index.html';
    const file = path.join(assets, filename);
    if (!fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(file) });
  });
  await context.addInitScript(() => {
    Object.defineProperty(navigator, 'credentials', { configurable: true, value: { get: async () => ({ id: 'key', rawId: new Uint8Array([1]), type: 'public-key', response: { clientDataJSON: new Uint8Array([2]), authenticatorData: new Uint8Array([3]), signature: new Uint8Array([4]) } }) } });
    class Socket {
      static OPEN = 1;
      constructor() { this.readyState = 0; setTimeout(() => { if (this.readyState === 3) return; this.readyState = 1; this.onmessage?.({ data: JSON.stringify({ type: 'activity_snapshot', version: 2, sequence: 0, revision: 1, sessions: [] }) }); }, 5); }
      close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
      send() {}
    }
    window.WebSocket = Socket;
  });
  const page = await context.newPage();
  page.on('pageerror', error => errors.push(error.message)); page.on('console', message => logs.push(message.text()));
  const dialog = () => page.locator('.session-password-dialog');
  async function fillPassword(username, password, confirmation) {
    await dialog().locator('[name=username]').fill(username);
    await dialog().locator('[name=password]').fill(password);
    if (confirmation !== undefined) await dialog().locator('[name=confirm-password]').fill(confirmation);
  }
  async function submit() { await dialog().locator('button[type=submit]').click(); }
  const waitConsole = () => page.waitForSelector('#console:not([hidden])');
  const waitWebUI = () => page.waitForSelector('#empty:not([hidden])');
  try {
    // New and older gateways never accidentally display a password alternative.
    for (const mode of ['valid', 'generic', 'failure']) {
      optionsMode = mode;
      await page.goto(origin + '/tgw/admin/'); await page.waitForSelector('#auth:not([hidden])');
      assert.equal(await page.locator('#password-login').isHidden(), true);
    }
    optionsMode = 'valid';
    await page.goto(origin + '/tgw/webui/'); await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#auth-password').isHidden(), true);
    state.enabled = true; state.username = 'admin'; state.password = '  correct secret  ';
    await page.reload(); await page.waitForSelector('#auth-password:not([hidden])');
    await page.locator('#auth-password').click();
    assert.equal(await dialog().locator('[name=username]').getAttribute('autocomplete'), 'username');
    assert.equal(await dialog().locator('[name=password]').getAttribute('autocomplete'), 'current-password');
    await fillPassword('admin', 'wrong password'); await submit();
    await page.waitForFunction(() => document.querySelector('.session-password-error').textContent.includes('incorrect'));
    assert.equal(state.authenticated, false);
    assert.equal(await dialog().locator('[name=username]').inputValue(), '');
    assert.equal(await dialog().locator('[name=password]').inputValue(), '');
    await page.keyboard.press('Escape'); await page.waitForSelector('.session-password-dialog', { state: 'detached' });
    await page.waitForFunction(() => document.activeElement.id === 'auth-password');
    await page.locator('#auth-password').click(); await fillPassword(' admin ', state.password);
    holdPassword = true; await page.keyboard.press('Enter');
    while (!releasePassword) await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(await dialog().locator('[name=username]').inputValue(), '', 'Username clears before the response');
    assert.equal(await dialog().locator('[name=password]').inputValue(), '', 'Password clears before the response');
    holdPassword = false; releasePassword(); releasePassword = null; await waitWebUI();
    assert.equal(posts.at(-1).body.username, 'admin', 'Username whitespace is trimmed');
    assert.equal(posts.at(-1).body.password, '  correct secret  ', 'Password whitespace is preserved');
    assert.equal(posts.at(-1).csrf, 'password-csrf');
    assert.equal(await dialog().count(), 0);

    // Password renewal is cancellable and renews with the same shared cookie flow.
    state.nearExpiry = true;
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await page.waitForSelector('.session-auth-banner:not([hidden]) .session-auth-password:not([hidden])');
    const beforeCancel = posts.length;
    await page.locator('.session-auth-password').click(); await page.keyboard.press('Escape');
    await page.waitForSelector('.session-password-dialog', { state: 'detached' });
    assert.equal(posts.length, beforeCancel); assert.equal(state.authenticated, true);
    assert.equal(await page.locator('#auth').isHidden(), true);
    await page.locator('.session-auth-password').click(); await fillPassword('admin', 'incorrect secret'); await submit();
    await page.waitForFunction(() => document.querySelector('.session-password-error').textContent.includes('incorrect'));
    await page.keyboard.press('Escape'); await page.waitForSelector('.session-password-dialog', { state: 'detached' }); await waitWebUI();
    assert.equal(state.authenticated, true, 'Cancelling after a wrong password restores the authorized browser');
    passwordFailure = 503;
    const beforeFailure = posts.length;
    await page.locator('.session-auth-password').click(); await fillPassword('admin', state.password); await submit();
    await page.waitForSelector('.session-password-dialog', { state: 'detached' }); await waitWebUI();
    assert.equal(posts.length, beforeFailure + 1, 'An ambiguous password request is never replayed automatically');
    assert.equal(state.authenticated, true);
    await page.locator('.session-auth-password').click(); await fillPassword('admin', state.password); await submit(); await waitWebUI();
    await page.waitForSelector('.session-auth-banner', { state: 'hidden' });
    assert.equal(state.method, 'password');
    state.authenticated = false;
    await page.evaluate(() => window.dispatchEvent(new Event('online'))); await page.waitForSelector('#auth:not([hidden])');
    assert.ok((await page.locator('#auth-description').textContent()).includes('password'));
    await page.locator('#auth-password').click(); await fillPassword('admin', state.password); await submit(); await waitWebUI();

    // Admin sign-in uses the same alternative; password changes invalidate the current browser.
    state.authenticated = false;
    await page.goto(origin + '/tgw/admin/'); await page.waitForSelector('#password-login:not([hidden])');
    await page.locator('#password-login').click(); await fillPassword('admin', state.password); await submit(); await waitConsole();
    await page.waitForSelector('#password-edit:not([hidden])');
    state.stale = true;
    await page.locator('#password-edit').click(); await page.waitForSelector('.session-password-dialog');
    assert.equal(await dialog().locator('[name=password]').getAttribute('autocomplete'), 'current-password', 'Sensitive actions reauthenticate password sessions with a password');
    await fillPassword('admin', state.password); await submit();
    await page.waitForFunction(() => document.querySelector('.session-password-dialog input[name=password]')?.autocomplete === 'new-password');
    assert.equal(await dialog().locator('[name=confirm-password]').getAttribute('autocomplete'), 'new-password');
    await fillPassword('renamed', 'new password value', 'different password'); await submit();
    assert.equal(mutations.length, 0, 'Mismatched confirmation does not update settings');
    const newPassword = 'é'.repeat(6); // Twelve UTF-8 bytes, six characters.
    await fillPassword('é'.repeat(33), newPassword, newPassword); await submit();
    assert.ok((await dialog().locator('.session-password-error').textContent()).includes('1–64 bytes'));
    assert.equal(mutations.length, 0, 'Username limit counts UTF-8 bytes');
    await fillPassword('renamed', newPassword, newPassword);
    page.once('dialog', prompt => prompt.accept()); await submit();
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(state.authenticated, false); assert.equal(state.username, 'renamed');
    assert.equal(mutations.at(-1).body.password, newPassword, 'Password policy counts UTF-8 bytes');
    assert.equal(await dialog().count(), 0);
    await page.locator('#password-login').click(); await fillPassword('renamed', newPassword); await submit(); await waitConsole();
    await page.waitForSelector('#password-disable:not([hidden])');
    page.once('dialog', prompt => prompt.dismiss()); await page.locator('#password-disable').click();
    assert.equal(state.enabled, true, 'Cancelling disable keeps password sign-in enabled');
    page.once('dialog', prompt => prompt.accept()); await page.locator('#password-disable').click();
    await page.waitForSelector('#auth:not([hidden])');
    await page.waitForSelector('#password-login', { state: 'hidden' });
    assert.equal(state.enabled, false); assert.equal(state.authenticated, false);

    // Passkey sessions can enable/change/disable the alternative without losing access.
    await page.locator('#login').click(); await waitConsole(); await page.waitForSelector('#password-edit:not([hidden])');
    await page.locator('#password-edit').click(); await fillPassword('backup', 'backup password secret', 'backup password secret');
    page.once('dialog', prompt => prompt.accept()); await submit();
    await page.waitForSelector('#password-disable:not([hidden])');
    assert.equal(state.authenticated, true); assert.equal(state.method, 'passkey');
    page.once('dialog', prompt => prompt.accept()); await page.locator('#password-disable').click();
    await page.waitForFunction(() => document.querySelector('#password-settings-status').textContent.includes('disabled'));
    assert.equal(state.authenticated, true);

    // The shared password dialog fits a narrow screen and clears on close.
    state.enabled = true; state.username = 'mobile'; state.password = 'mobile password secret'; state.authenticated = false;
    await page.setViewportSize({ width: 390, height: 844 }); await page.goto(origin + '/tgw/webui/');
    await page.waitForSelector('#auth-password:not([hidden])'); await page.locator('#auth-password').click(); await fillPassword('mobile', state.password);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth && document.querySelector('.session-password-dialog').getBoundingClientRect().width <= innerWidth), true);
    await page.evaluate(() => document.documentElement.style.setProperty('--viewport-height', '380px'));
    assert.equal(await dialog().evaluate(element => element.getBoundingClientRect().height <= 348), true, 'The password dialog fits the visible viewport above a mobile keyboard');
    await page.keyboard.press('Escape'); await page.waitForSelector('.session-password-dialog', { state: 'detached' });
    const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
    for (const secret of ['correct secret', 'backup password secret', 'mobile password secret', newPassword]) { assert.ok(!storage.includes(secret)); assert.ok(!logs.join('\n').includes(secret)); }
    assert.ok(mutations.every(value => value.csrf.startsWith('password-csrf')));
    assert.deepEqual(errors, []);
    console.log('Password browser checks passed: optional discovery, both login screens, wrong password, whitespace/UTF-8 policy, cancellation/focus, renewal/expiry, fresh password auth, enable/change/disable/invalidation, passkey continuity, mobile layout, and secret erasure.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

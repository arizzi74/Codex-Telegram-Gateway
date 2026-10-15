#!/usr/bin/env node
// Telegram bot console checks use browser fixtures, never Telegram or a live gateway.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const token = '123456789:private_BotFather_token_for_browser_checks';
const replacement = '123456789:replacement_private_token_for_browser_checks';
const malicious = '<img src=x onerror="window.injected=true">';
const primaryID = 'primary';
const additionalID = '11111111-2222-4333-8444-555555555555';
const createdID = '22222222-3333-4444-8555-666666666666';
const screenshots = process.env.ADMIN_BOTS_SCREENSHOTS || '';
if (screenshots) fs.mkdirSync(screenshots, { recursive: true });

async function fixture(browser, width) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const state = {
    bots: [{ id: primaryID, telegram_id: 12345, username: 'primary_bot', display_name: 'Primary ' + malicious, legacy: true, enabled: true, managed: false, can_manage_bots: false, status: 'connected' }],
    pending: [], reads: 0, profileRefreshes: [], mutations: [], readStatus: 200, refreshStatus: 200, dashboardStatus: 200,
    connectFailure: null, webhookConflict: false, requireReauth: false, credentialCalls: 0,
    invalidCreationURL: false, creationNumber: 0, networkFailure: false, holdNextRead: false,
    managerCanManageBots: true, managedFailure: false, sessionAgeMillis: 0,
  };
  const errors = [];
  const messages = [];
  const externalRequests = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => messages.push(message.text()));
  await page.context().route('https://t.me/**', route => {
    externalRequests.push(route.request().url());
    return route.fulfill({ contentType: 'text/html', body: '<p>Telegram creation fixture</p>' });
  });
  await page.route('https://admin.test/**', async route => {
    const request = route.request();
    const url = new URL(request.url());
    let body = {};
    let status = 200;
    if (url.pathname.startsWith('/tgw/api/')) {
      const now = await page.evaluate(() => Date.now());
      if (url.pathname.endsWith('/login/options')) body = { passkey: true, password: false };
      else if (url.pathname.endsWith('/admin/session')) body = { authenticated: true, session_id: 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa', server_time: new Date(now).toISOString(), expires_at: new Date(now + 8 * 3600000).toISOString(), reauthenticated_at: new Date(now - state.sessionAgeMillis).toISOString() };
      else if (url.pathname.endsWith('/login/begin')) body = { ceremony_id: 'test', publicKey: { challenge: 'dGVzdA', rpId: 'admin.test' } };
      else if (url.pathname.endsWith('/login/finish')) { state.credentialCalls++; body = { ok: true }; }
      else if (url.pathname.endsWith('/dashboard')) {
        status = state.dashboardStatus;
        body = { workers: [], runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0,
          bot: { id: 12345, username: 'primary_bot', allowed_user_count: 1, allowed_chat_count: 0, status: 'connected' } };
      } else if (url.pathname.endsWith('/passkeys')) body = [];
      else if (url.pathname.endsWith('/admin/sessions')) body = { sessions: [] };
      else if (url.pathname.endsWith('/admin/password')) body = { enabled: false, username: '' };
      else if (url.pathname.endsWith('/admin/audio')) body = { settings: { enabled: false, model: 'base', threads: 2, reply_with_audio: false, tts_voice: 'auto' } };
      else if (url.pathname.endsWith('/admin/telegram-bots') && request.method() === 'GET') {
        state.reads++; status = state.readStatus;
        body = status === 200 ? structuredClone({ bots: state.bots, pending: state.pending }) : { message: 'Unsafe upstream failure ' + token };
        if (state.holdNextRead) {
          state.holdNextRead = false;
          await new Promise(resolve => { state.releaseRead = resolve; state.onReadStarted(); });
        }
      } else if (url.pathname.endsWith('/admin/telegram-bots/refresh') && request.method() === 'POST') {
        state.profileRefreshes.push({ csrf: request.headers()['x-csrf-token'] });
        status = state.refreshStatus;
        if (status === 200) {
          state.bots.filter(bot => bot.enabled !== false).forEach(bot => { bot.can_manage_bots = state.managerCanManageBots; });
          body = structuredClone({ bots: state.bots, pending: state.pending });
        } else body = { code: 'telegram_bot_unavailable', message: 'Unsafe upstream failure ' + token };
      } else if (url.pathname.includes('/admin/telegram-bots') && request.method() !== 'GET') {
        const payload = JSON.parse(request.postData() || '{}');
        state.mutations.push({ path: url.pathname, method: request.method(), payload, csrf: request.headers()['x-csrf-token'] });
        if (state.networkFailure) return route.abort('failed');
        if (state.requireReauth) {
          state.requireReauth = false; status = 403; body = { code: 'reauthentication_required' };
        } else if (url.pathname.endsWith('/telegram-bots') && request.method() === 'POST') {
          if (state.connectFailure) {
            status = state.connectFailure.status;
            body = { code: state.connectFailure.code, message: 'Unsafe Telegram error containing ' + payload.token };
          } else if (state.webhookConflict && !payload.takeover) {
            status = 409; body = { code: 'telegram_bot_webhook_conflict', takeover_required: true, message: payload.token };
          } else {
            const bot = state.bots.find(bot => bot.id === payload.replace_id);
            if (bot) bot.status = 'connected';
            else state.bots.push({ id: additionalID, telegram_id: 54321, username: 'connected_bot', display_name: 'Connected bot', enabled: true, managed: false, can_manage_bots: true, status: 'connected' });
            body = bot || state.bots.at(-1);
          }
        } else if (url.pathname.endsWith('/telegram-bots/managed') && request.method() === 'POST') {
          if (state.managedFailure) {
            status = 409; body = { code: 'telegram_bot_manager_unavailable', message: 'Unsafe upstream failure ' + token };
          } else {
            state.creationNumber++;
            body = { id: 'intent-' + state.creationNumber, manager_id: payload.manager_id, requested_username: payload.username, display_name: payload.name,
              launch_url: state.invalidCreationURL ? 'https://invalid.example/newbot' : 'https://t.me/newbot/primary_bot/' + payload.username + '?name=' + encodeURIComponent(payload.name),
              expires_at: new Date(now + 15 * 60000).toISOString(), status: 'pending', bot_id: '' };
            state.pending.push(body);
          }
        } else {
          const id = decodeURIComponent(url.pathname.split('/').at(-1));
          if (request.method() === 'PATCH') Object.assign(state.bots.find(bot => bot.id === id), payload);
          else if (request.method() === 'DELETE') { assert.equal(payload.confirmed, true); state.bots = state.bots.filter(bot => bot.id !== id); }
          status = 204;
        }
      }
      return route.fulfill({ status, contentType: 'application/json', body: status === 204 ? '' : JSON.stringify(body) });
    }
    const filename = /\.(js|css)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html',
      headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(() => {
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async () => {
      if (window.failCredentials) throw new DOMException('Verification cancelled.', 'NotAllowedError');
      return { id: 'passkey', rawId: new Uint8Array([1]).buffer, type: 'public-key', response: { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer } };
    } });
  });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'bot-csrf', url: 'https://admin.test/', secure: true, sameSite: 'Strict' }]);
  await page.clock.install();
  await page.goto('https://admin.test/tgw/admin/');
  await page.waitForSelector('[data-bot-id="primary"]');
  const card = id => page.locator('[data-bot-id="' + id + '"]');
  async function refresh() {
    await page.evaluate(() => loadTelegramBots());
    await page.waitForFunction(() => telegramLoadingEpoch === null);
  }
  async function tokenSubmit(value = token) {
    await page.locator('#bot-token').fill(value);
    await page.locator('#bot-token-submit').click();
    await page.waitForFunction(() => !telegramBusy);
  }
  async function create(username = 'created_bot') {
    await page.locator('#bot-create').click();
    await page.locator('#bot-create-username').fill(username);
    await page.locator('#bot-create-name').fill('Created ' + malicious);
    await page.locator('#bot-create-submit').click();
    await page.waitForFunction(() => !telegramBusy);
  }
  return { page, state, errors, messages, externalRequests, card, refresh, tokenSubmit, create };
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const width of [390, 1280]) {
      const { page, state, errors, messages, externalRequests, card, refresh, tokenSubmit, create } = await fixture(browser, width);
      assert.match(await card(primaryID).innerText(), /Primary/);
      assert.equal(await card(primaryID).getByRole('button', { name: 'Remove', exact: true }).count(), 0);
      assert.equal(await card(primaryID).getByRole('button', { name: 'Replace token', exact: true }).count(), 0);
      assert.equal(await card(primaryID).getByRole('link', { name: 'Open Telegram' }).getAttribute('href'), 'https://t.me/primary_bot');
      assert.equal(await page.locator('#bot-info img').count(), 0, 'Bot names are plain text');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Bot cards fit the viewport');
      await refresh();
      assert.equal(state.profileRefreshes.length, 0, 'Ordinary reads keep cached profile semantics');

      await page.locator('#bot-connect').click();
      assert.equal(await page.locator('#bot-token').getAttribute('type'), 'password');
      assert.equal(await page.locator('#bot-token-dialog').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true);
      await page.locator('#bot-token').fill(token);
      await page.locator('#bot-token-cancel').click();
      assert.equal(await page.locator('#bot-token').inputValue(), '', 'Closing clears the token');
      await page.locator('#bot-connect').click();
      state.connectFailure = { status: 502, code: 'telegram_bot_unavailable' };
      await tokenSubmit();
      assert.match(await page.locator('#bot-token-error').innerText(), /Telegram could not complete/);
      assert.equal(await page.locator('#bot-token').inputValue(), '', 'Submitting clears the token even after failure');
      assert.equal((await page.locator('body').innerText()).includes(token), false, 'Raw server token errors are never rendered');
      state.connectFailure = null;
      state.networkFailure = true;
      const beforeNetworkFailure = state.mutations.length;
      await tokenSubmit();
      assert.equal(state.mutations.length, beforeNetworkFailure + 1, 'An ambiguous network failure never replays a token mutation');
      assert.match(await page.locator('#bot-token-error').innerText(), /check whether it completed/);
      state.networkFailure = false;
      state.requireReauth = true;
      await page.evaluate(() => { window.failCredentials = true; });
      const beforeVerificationCancel = state.mutations.length;
      await tokenSubmit();
      assert.equal(state.mutations.length, beforeVerificationCancel + 1, 'Cancelled verification never replays the rejected mutation');
      assert.match(await page.locator('#bot-token-error').innerText(), /Verification was cancelled/);
      await page.evaluate(() => { window.failCredentials = false; });
      state.requireReauth = true;
      const beforeConnect = state.mutations.length;
      await tokenSubmit();
      await page.waitForSelector('[data-bot-id="' + additionalID + '"]');
      assert.equal(state.mutations.length, beforeConnect + 2, 'Fresh-auth rejection verifies then resubmits once');
      assert.equal(state.credentialCalls, 1);
      assert.equal(await page.locator('#bot-token-dialog').evaluate(dialog => dialog.open), false);
      // Return a pre-mutation poll after Disable has already completed. The
      // console must obtain a newer snapshot before finishing the action.
      const readStarted = new Promise(resolve => { state.onReadStarted = resolve; });
      state.holdNextRead = true;
      const heldRead = page.evaluate(() => loadTelegramBots());
      await readStarted;
      await Promise.all([
        page.waitForResponse(response => response.request().method() === 'PATCH' && response.url().endsWith('/telegram-bots/' + additionalID)),
        card(additionalID).getByRole('button', { name: 'Disable', exact: true }).click(),
      ]);
      state.releaseRead();
      await heldRead;
      await page.waitForFunction(() => document.querySelector('[data-bot-id="11111111-2222-4333-8444-555555555555"]').textContent.includes('Enable'));
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.bots.find(bot => bot.id === additionalID).enabled, false);
      await card(additionalID).getByRole('button', { name: 'Enable', exact: true }).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.bots.find(bot => bot.id === additionalID).enabled, true);

      state.webhookConflict = true;
      await card(additionalID).getByRole('button', { name: 'Replace token', exact: true }).click();
      let takeoverConfirmation = '';
      page.once('dialog', async dialog => { takeoverConfirmation = dialog.message(); await dialog.dismiss(); });
      const beforeCancel = state.mutations.length;
      await tokenSubmit(replacement);
      assert.match(takeoverConfirmation, /previous application will stop receiving/);
      assert.equal(state.mutations.length, beforeCancel + 1, 'Declining takeover does not retry');
      assert.match(await page.locator('#bot-token-status').innerText(), /cancelled/);
      page.once('dialog', dialog => dialog.accept());
      await tokenSubmit(replacement);
      const takeoverCalls = state.mutations.slice(-2);
      assert.equal(takeoverCalls[0].payload.takeover, false);
      assert.equal(takeoverCalls[1].payload.takeover, true);
      assert.equal(takeoverCalls[1].payload.replace_id, additionalID);
      state.webhookConflict = false;
      if (screenshots) await page.locator('#telegram-bots-section').screenshot({ path: path.join(screenshots, 'telegram-bots-' + width + '.png') });

      await page.locator('#bot-create').click();
      assert.equal(await page.locator('#bot-manager option').count(), 2, 'Enabled bots remain selectable while their capability cache refreshes');
      assert.match(await page.locator('#bot-create-description').innerText(), /Bot Management Mode.*@BotFather/);
      assert.match(await page.locator('#bot-manager-help').innerText(), /refreshes the selected bot’s Telegram profile/);

      // A cached GET already in flight must not consume the explicit refresh.
      state.sessionAgeMillis = 6 * 60000;
      await page.evaluate(async () => { window.failCredentials = true; await sessionAuth.verify('test-stale-session'); });
      const beforeProfileVerification = state.credentialCalls;
      const profileReadStarted = new Promise(resolve => { state.onReadStarted = resolve; });
      state.holdNextRead = true;
      const oldProfileRead = page.evaluate(() => loadTelegramBots());
      await profileReadStarted;
      await page.locator('#bot-manager-refresh').click();
      assert.equal(state.profileRefreshes.length, 0, 'Explicit refresh waits for the cached GET');
      state.releaseRead();
      await oldProfileRead;
      await page.waitForFunction(() => telegramBots.bots.find(bot => bot.id === 'primary').can_manage_bots === true);
      assert.equal(state.profileRefreshes.length, 1, 'The manager button sends a profile-refresh POST after the GET');
      assert.equal(state.profileRefreshes[0].csrf, 'bot-csrf');
      assert.equal(state.credentialCalls, beforeProfileVerification, 'Profile refresh does not require recent authentication');
      state.sessionAgeMillis = 0;
      await page.evaluate(() => { window.failCredentials = false; });

      state.refreshStatus = 502;
      await page.locator('#bot-manager-refresh').click();
      await page.waitForFunction(() => !document.getElementById('bot-create-error').hidden);
      assert.match(await page.locator('#bot-create-error').innerText(), /Unable to refresh Telegram bot profiles/);
      assert.equal((await page.locator('body').innerText()).includes(token), false, 'Refresh failures never expose raw upstream errors');
      state.refreshStatus = 200;
      await page.locator('#bot-manager-refresh').click();
      await page.waitForFunction(() => document.getElementById('bot-create-error').hidden);

      state.managedFailure = true;
      await page.locator('#bot-create-username').fill('blocked_bot');
      await page.locator('#bot-create-name').fill('Blocked bot');
      await page.locator('#bot-create-submit').click();
      await page.waitForFunction(() => !telegramBusy);
      assert.match(await page.locator('#bot-create-error').innerText(), /Telegram still reports.*after refreshing its profile/);
      assert.doesNotMatch(await page.locator('#bot-create-error').innerText(), /refresh the bot list/i);
      state.managedFailure = false;
      if (screenshots) await page.screenshot({ path: path.join(screenshots, 'telegram-bot-create-' + width + '.png') });
      await page.locator('#bot-create-cancel').click();
      await create();
      assert.equal(await page.locator('#bot-create-result').isVisible(), true);
      assert.match(await page.locator('#bot-create-result-status').innerText(), /Waiting for Telegram.*15 minutes/);
      assert.equal(await page.locator('#bot-create-dialog').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true);
      const launch = await page.locator('#bot-create-launch').getAttribute('href');
      assert.match(launch, /^https:\/\/t.me\/newbot\/primary_bot\/created_bot\?name=/);
      const popupPromise = page.waitForEvent('popup');
      await page.locator('#bot-create-launch').click();
      const popup = await popupPromise;
      await popup.waitForLoadState();
      assert.equal(popup.url(), launch);
      assert.deepEqual(externalRequests, [launch]);
      await popup.close();
      state.pending[0].status = 'connected';
      state.pending[0].bot_id = createdID;
      state.bots.push({ id: createdID, telegram_id: 98765, username: 'created_bot', display_name: 'Created ' + malicious, enabled: true, managed: true, can_manage_bots: false, status: 'connected' });
      await page.clock.runFor(3100);
      await page.waitForSelector('[data-bot-id="' + createdID + '"]');
      assert.match(await card(createdID).innerText(), /Managed.*Same allowed owner and workers/s);
      assert.match(await page.locator('#bot-create-result-status').innerText(), /is connected/);
      assert.equal(await page.locator('#bot-create-launch').isVisible(), false);
      assert.equal(state.profileRefreshes.length, 3, 'Pending-request polling does not force profile refresh');
      assert.equal(await page.evaluate(() => window.injected), undefined);
      await page.locator('#bot-create-done').click();

      await create('expires_bot');
      state.pending.at(-1).expires_at = new Date(await page.evaluate(() => Date.now()) + 1000).toISOString();
      await page.clock.runFor(3100);
      await page.waitForFunction(() => document.getElementById('bot-create-result-status').textContent.includes('expired'));
      assert.equal(await page.locator('#bot-create-launch').getAttribute('href'), null, 'Expired creation links cannot be opened');
      await page.locator('#bot-create-done').click();
      state.invalidCreationURL = true;
      await create('invalid_link_bot');
      assert.equal(await page.locator('#bot-create-error').isVisible(), true);
      assert.equal(await page.locator('#bot-create-launch').getAttribute('href'), null, 'Unexpected creation destinations never become links');
      state.invalidCreationURL = false;
      state.pending = state.pending.filter(item => item.requested_username !== 'invalid_link_bot');
      await page.locator('#bot-create-cancel').click();

      const beforeRemove = state.mutations.length;
      page.once('dialog', dialog => dialog.dismiss());
      await card(additionalID).getByRole('button', { name: 'Remove', exact: true }).click();
      assert.equal(state.mutations.length, beforeRemove, 'Cancelled removal does nothing');
      let removalConfirmation = '';
      page.once('dialog', async dialog => { removalConfirmation = dialog.message(); await dialog.accept(); });
      await card(additionalID).getByRole('button', { name: 'Remove', exact: true }).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.match(removalConfirmation, /local bot records, chat bindings and notification subscriptions.*Worker sessions and project files will be kept/s);
      assert.equal(await card(additionalID).count(), 0);
      assert.equal(await card(primaryID).count(), 1, 'Removing another bot preserves the primary bot');
      assert.equal(state.mutations.at(-1).method, 'DELETE');
      assert.deepEqual(state.mutations.at(-1).payload, { confirmed: true });
      assert.ok(state.mutations.every(mutation => mutation.csrf === 'bot-csrf'), 'All bot writes include CSRF');

      state.readStatus = 502;
      await refresh();
      assert.equal(await card(primaryID).count(), 1, 'Read failures keep known bot state');
      assert.match(await page.locator('#bot-error').innerText(), /Unable to refresh/);
      state.readStatus = 200;
      await refresh();
      assert.equal(await page.locator('#bot-error').isVisible(), false);
      state.bots.forEach(bot => { bot.enabled = false; });
      await refresh();
      await page.locator('#bot-create').click();
      assert.equal(await page.locator('#bot-create-submit').isDisabled(), true);
      assert.match(await page.locator('#bot-manager-help').innerText(), /Connect or enable a bot first/);
      await page.locator('#bot-create-cancel').click();
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
      assert.equal(storage.includes(token) || storage.includes(replacement), false, 'Tokens never enter browser storage');
      assert.equal(messages.some(message => message.includes(token) || message.includes(replacement)), false, 'Tokens stay out of browser logs');
      assert.equal((await page.locator('body').innerText()).includes(token), false);

      await page.locator('#bot-connect').click();
      await page.locator('#bot-token').fill(token);
      state.dashboardStatus = 401;
      await page.evaluate(() => load(false));
      await page.waitForSelector('#auth:not([hidden])');
      assert.equal(await page.locator('#bot-token-dialog').evaluate(dialog => dialog.open), false, 'Expiry closes secret dialogs');
      assert.equal(await page.locator('#bot-token').inputValue(), '');
      assert.equal(await page.locator('.telegram-bot').count(), 0, 'Expiry clears protected bot data');
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('Admin Telegram bot browser checks passed: mobile/desktop cards and dialogs, token privacy, connect/replace/takeover, recent authentication, explicit profile refresh with cached-read races, enable/disable, managed creation and Telegram link, callback polling, expiry, safe rendering, confirmed removal, CSRF, refresh recovery and sign-in expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

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
    bots: [{ id: primaryID, telegram_id: 12345, username: 'primary_bot', display_name: 'Primary ' + malicious, original_display_name: 'Primary ' + malicious, inherit_session_name: false, name_sync_pending: false, legacy: true, enabled: true, managed: false, can_manage_bots: false, status: 'connected' }],
    pending: [], reads: 0, profileRefreshes: [], mutations: [], readStatus: 200, refreshStatus: 200, dashboardStatus: 200,
    connectFailure: null, webhookConflict: false, requireReauth: false, credentialCalls: 0,
    invalidCreationURL: false, creationNumber: 0, creationUsername: 'created_2_bot', networkFailure: false, holdNextRead: false,
    managerCanManageBots: true, managedFailure: false, sessionAgeMillis: 0, patchFailure: false,
    profileResets: [], profileResetFailure: null, holdNextProfileReset: false,
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
            else state.bots.push({ id: additionalID, telegram_id: 54321, username: 'connected_bot', display_name: 'Connected bot', original_display_name: 'Connected bot', inherit_session_name: false, name_sync_pending: false, enabled: true, managed: false, can_manage_bots: true, status: 'connected' });
            body = bot || state.bots.at(-1);
          }
        } else if (/\/telegram-bots\/[^/]+\/profile-photo\/reset$/.test(url.pathname) && request.method() === 'POST') {
          assert.deepEqual(payload, { confirmed: true });
          if (state.profileResetFailure) {
            status = state.profileResetFailure.status;
            body = { code: state.profileResetFailure.code, message: 'Unsafe upstream error ' + token };
          } else {
            state.profileResets.push(decodeURIComponent(url.pathname.split('/').at(-3)));
            status = 204;
          }
          if (state.holdNextProfileReset) {
            state.holdNextProfileReset = false;
            await new Promise(resolve => { state.releaseProfileReset = resolve; state.onProfileResetStarted(); });
          }
        } else if (url.pathname.endsWith('/telegram-bots/managed') && request.method() === 'POST') {
          if (state.managedError) {
            status = state.managedError.status; body = { code: state.managedError.code, message: 'Unsafe upstream failure ' + token };
          } else if (state.managedFailure) {
            status = 409; body = { code: 'telegram_bot_manager_unavailable', message: 'Unsafe upstream failure ' + token };
          } else {
            state.creationNumber++;
            body = { id: 'intent-' + state.creationNumber, manager_id: payload.manager_id, requested_username: state.creationUsername, display_name: payload.display_name,
              launch_url: state.invalidCreationURL ? 'https://invalid.example/newbot' : 'https://t.me/newbot/primary_bot/' + state.creationUsername + '?name=' + encodeURIComponent(payload.display_name),
              expires_at: new Date(now + 15 * 60000).toISOString(), status: 'pending', bot_id: '' };
            state.pending.push(body);
            if (state.holdNextManagedCreate) {
              state.holdNextManagedCreate = false;
              await new Promise(resolve => { state.releaseManagedCreate = resolve; state.onManagedCreateStarted(); });
            }
          }
        } else {
          const id = decodeURIComponent(url.pathname.split('/').at(-1));
          if (request.method() === 'PATCH' && state.patchFailure) return route.fulfill({ status: 502, contentType: 'application/json', body: JSON.stringify({ code: 'telegram_bot_unavailable', message: 'Unsafe upstream error ' + token }) });
          if (request.method() === 'PATCH') {
            const bot = state.bots.find(bot => bot.id === id);
            Object.assign(bot, payload);
            if ('inherit_session_name' in payload) bot.name_sync_pending = true;
          }
          else if (request.method() === 'DELETE') { assert.equal(payload.confirmed, true); state.bots = state.bots.filter(bot => bot.id !== id); }
          status = 204;
        }
      }
      return route.fulfill({ status, contentType: 'application/json', body: status === 204 ? '' : JSON.stringify(body) });
    }
    const filename = /\.(js|css|svg)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.svg') ? 'image/svg+xml' : 'text/html',
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
  await page.waitForSelector('[data-bot-id="primary"]', { state: 'attached' });
  assert.equal(await page.locator('#telegram-bots-section').evaluate(section => section.open), false, 'Telegram bots starts collapsed');
  await page.locator('#telegram-bots-section > summary').click();
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
  async function create(name = 'Created ' + malicious, username = 'created_2_bot') {
    state.creationUsername = username;
    await page.locator('#bot-create').click();
    await page.locator('#bot-create-name').fill(name);
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
      assert.equal(await card(primaryID).getByRole('button', { name: 'Delete', exact: true }).count(), 0);
      assert.equal(await card(primaryID).getByRole('button', { name: 'Replace token', exact: true }).count(), 0);
      assert.equal(await card(primaryID).locator('.bot-toggle').getAttribute('aria-expanded'), 'false', 'Every bot starts collapsed');
      assert.equal(await card(primaryID).locator('.bot-toggle').getAttribute('aria-controls'), await card(primaryID).locator('.bot-content').getAttribute('id'), 'The disclosure identifies its controlled content');
      assert.equal(await card(primaryID).locator('.bot-content').isVisible(), false, 'Collapsed bots hide their details and settings');
      assert.deepEqual(await card(primaryID).getByRole('button').allTextContents(), ['Primary ' + malicious, 'Disable'], 'The primary header contains only its name and allowed controls');
      await card(primaryID).locator('.bot-toggle').press('Enter');
      assert.equal(await card(primaryID).locator('.bot-content').isVisible(), true, 'The bot disclosure works from the keyboard');
      assert.equal(await card(primaryID).getByRole('link', { name: 'Open Telegram' }).getAttribute('href'), 'https://t.me/primary_bot');
      assert.equal(await page.locator('#bot-info img').count(), 0, 'Bot names are plain text');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Bot cards fit the viewport');
      const nameSetting = id => card(id).getByRole('checkbox', { name: 'Inherit selected session name' });
      const photoReset = id => card(id).getByRole('button', { name: 'Reset profile photo >_', exact: true });
      assert.equal(await nameSetting(primaryID).isChecked(), false, 'Name inheritance is opt in, including the primary bot');
      assert.match(await card(primaryID).innerText(), /Original name · Session name; uses your private chat selection/);
      assert.match(await card(primaryID).innerText(), /default Telegram display name/);
      await refresh();
      assert.equal(await card(primaryID).locator('.bot-toggle').getAttribute('aria-expanded'), 'true', 'Bot refresh preserves expansion state');
      assert.equal(state.profileRefreshes.length, 0, 'Ordinary reads keep cached profile semantics');
      assert.equal(state.profileResets.length, 0, 'Opening and refreshing bots never changes their profile photos');
      assert.equal(await photoReset(primaryID).isEnabled(), true, 'An enabled primary bot can reset its photo');
      const beforeCancelledReset = state.mutations.length;
      let resetConfirmation = '';
      page.once('dialog', async dialog => { resetConfirmation = dialog.message(); await dialog.dismiss(); });
      await photoReset(primaryID).click();
      assert.match(resetConfirmation, /@primary_bot.*replaces its current Telegram profile photo/s, 'Reset confirmation names the target and replacement');
      assert.equal(state.mutations.length, beforeCancelledReset, 'Cancelling a photo reset sends no request');
      page.once('dialog', dialog => dialog.accept());
      await photoReset(primaryID).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.deepEqual(state.profileResets, [primaryID], 'The confirmed primary reset reaches the API');
      assert.equal(state.mutations.at(-1).path, '/tgw/api/v1/admin/telegram-bots/primary/profile-photo/reset');
      assert.equal(state.mutations.at(-1).method, 'POST');
      assert.deepEqual(state.mutations.at(-1).payload, { confirmed: true });
      assert.match(await page.locator('#bot-status').innerText(), /Profile photo reset for @primary_bot/);

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
      assert.equal(await card(additionalID).locator('.bot-toggle').getAttribute('aria-expanded'), 'false', 'Newly connected bots start collapsed');
      assert.deepEqual(await card(additionalID).getByRole('button').allTextContents(), ['Connected bot', 'Disable', 'Delete'], 'A collapsed bot shows only its name and enable/delete controls');
      await Promise.all([
        page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/dashboard')),
        page.clock.runFor(15010),
      ]);
      await page.waitForFunction(() => !loading && telegramLoadingEpoch === null);
      assert.equal(await card(primaryID).locator('.bot-toggle').getAttribute('aria-expanded'), 'true', 'The dashboard poll preserves expanded bots');
      assert.equal(await card(additionalID).locator('.bot-toggle').getAttribute('aria-expanded'), 'false', 'The dashboard poll preserves collapsed bots');
      await card(additionalID).getByRole('button', { name: 'Disable', exact: true }).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(await card(additionalID).locator('.bot-toggle').getAttribute('aria-expanded'), 'false', 'Disabling from the header keeps the bot collapsed');
      await card(additionalID).getByRole('button', { name: 'Enable', exact: true }).click();
      await page.waitForFunction(() => !telegramBusy);
      await card(additionalID).locator('.bot-toggle').press('Space');
      assert.equal(await card(additionalID).locator('.bot-content').isVisible(), true, 'Space opens bot settings');
      state.requireReauth = true;
      const beforeNameReauth = state.mutations.length;
      await nameSetting(primaryID).check();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.mutations.length, beforeNameReauth + 2, 'Name setting verifies and retries only an explicit fresh-auth rejection');
      assert.deepEqual(state.mutations.at(-1).payload, { inherit_session_name: true });
      assert.equal(await nameSetting(primaryID).isChecked(), true);
      assert.match(await card(primaryID).innerText(), /Name update queued/);
      state.bots[0].name_sync_pending = false;
      state.bots[0].display_name = state.bots[0].original_display_name + ' · Selected session';
      await refresh();
      assert.match(await card(primaryID).locator('h3').innerText(), /Selected session/);
      assert.equal(await nameSetting(additionalID).isChecked(), false, 'Each bot has its own preference');
      await nameSetting(primaryID).uncheck();
      await page.waitForFunction(() => !telegramBusy);
      assert.deepEqual(state.mutations.at(-1).payload, { inherit_session_name: false });
      assert.match(await page.locator('#bot-status').innerText(), /restores the name in the background/);
      state.bots[0].display_name = state.bots[0].original_display_name;
      state.bots[0].name_sync_pending = false;
      await refresh();
      state.patchFailure = true;
      const beforeNameFailure = state.mutations.length;
      await nameSetting(additionalID).check();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.mutations.length, beforeNameFailure + 1, 'A name setting failure is never replayed');
      assert.equal(await nameSetting(additionalID).isChecked(), false, 'Failed setting restores the last known preference');
      assert.match(await page.locator('#bot-error').innerText(), /Telegram could not complete/);
      state.patchFailure = false;
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
      assert.equal(await photoReset(additionalID).isDisabled(), true, 'Disabled bots cannot reset their photo');
      assert.match(await card(additionalID).locator('.bot-photo-help').innerText(), /Enable this bot/);
      await nameSetting(additionalID).check();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(await nameSetting(additionalID).isChecked(), true, 'A disabled bot can save its name preference');
      assert.match(await card(additionalID).innerText(), /Name changes resume when this bot is enabled/);
      await card(additionalID).getByRole('button', { name: 'Enable', exact: true }).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.bots.find(bot => bot.id === additionalID).enabled, true);
      state.bots.find(bot => bot.id === additionalID).name_error = 'name_update_unavailable';
      await refresh();
      assert.match(await card(additionalID).innerText(), /Telegram has not applied the name change yet/);
      state.bots.find(bot => bot.id === additionalID).name_error = 'name_webhook_moved';
      await refresh();
      assert.match(await card(additionalID).innerText(), /webhook moved.*Reconnect/s);
      state.bots.find(bot => bot.id === additionalID).name_error = token;
      await refresh();
      assert.equal((await card(additionalID).innerText()).includes(token), false, 'Raw name errors are never rendered');
      state.bots.find(bot => bot.id === additionalID).name_error = '';
      state.bots.find(bot => bot.id === additionalID).name_sync_pending = false;
      await refresh();

      const resetStarted = new Promise(resolve => { state.onProfileResetStarted = resolve; });
      state.holdNextProfileReset = true;
      page.once('dialog', dialog => dialog.accept());
      await photoReset(additionalID).click();
      await resetStarted;
      assert.equal(await photoReset(primaryID).isDisabled(), true, 'A running action locks other photo resets');
      assert.equal(await photoReset(additionalID).isDisabled(), true, 'A photo reset cannot be submitted twice');
      assert.match(await card(additionalID).locator('.bot-photo-help').innerText(), /Wait for the current bot action/);
      state.releaseProfileReset();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(await photoReset(additionalID).isEnabled(), true);

      const failedResetStarted = new Promise(resolve => { state.onProfileResetStarted = resolve; });
      state.holdNextProfileReset = true;
      state.profileResetFailure = { status: 502, code: 'telegram_bot_unavailable' };
      const beforeResetFailure = state.mutations.length;
      page.once('dialog', dialog => dialog.accept());
      await photoReset(additionalID).click();
      await failedResetStarted;
      await page.locator('#telegram-bots-section > summary').click();
      state.releaseProfileReset();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.mutations.length, beforeResetFailure + 1, 'A failed photo reset is not replayed');
      await page.locator('#telegram-bots-section .section-attention').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#telegram-bots-section').evaluate(section => section.open), false, 'A reset failure preserves the collapsed section');
      await page.locator('#telegram-bots-section > summary').click();
      assert.match(await page.locator('#bot-error').innerText(), /Telegram could not complete/);
      assert.doesNotMatch(await page.locator('#bot-status').innerText(), /Resetting the profile photo/, 'A failed reset does not keep a progress message');
      assert.equal(await page.locator('#telegram-bots-section .section-attention').isVisible(), false, 'Opening reveals the error and clears the summary indicator');
      assert.equal((await page.locator('body').innerText()).includes(token), false, 'Photo failures never expose raw Telegram errors');
      for (const [code, message] of [
        ['telegram_bot_disabled', /Enable this bot/],
        ['telegram_bot_profile_webhook_conflict', /Reconnect it here before resetting/],
        ['telegram_bot_owner_changed', /owner changed.*current token/],
      ]) {
        state.profileResetFailure = { status: 409, code };
        page.once('dialog', dialog => dialog.accept());
        await photoReset(additionalID).click();
        await page.waitForFunction(() => !telegramBusy);
        assert.match(await page.locator('#bot-error').innerText(), message);
      }
      state.profileResetFailure = null;
      state.requireReauth = true;
      const beforeResetReauth = state.mutations.length;
      const beforeResetCredentials = state.credentialCalls;
      page.once('dialog', dialog => dialog.accept());
      await photoReset(additionalID).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.mutations.length, beforeResetReauth + 2, 'Only an explicit fresh-auth rejection retries a photo reset');
      assert.equal(state.credentialCalls, beforeResetCredentials + 1);
      assert.deepEqual(state.mutations.at(-1).payload, { confirmed: true });
      state.requireReauth = true;
      await page.evaluate(() => { window.failCredentials = true; });
      const beforeResetReauthCancel = state.mutations.length;
      page.once('dialog', dialog => dialog.accept());
      await photoReset(additionalID).click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(state.mutations.length, beforeResetReauthCancel + 1, 'Cancelled verification does not replay the photo reset');
      assert.match(await page.locator('#bot-error').innerText(), /Verification was cancelled/);
      await page.evaluate(() => { window.failCredentials = false; });

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
      const defaultPhoto = page.getByRole('img', { name: 'Codex Gateway profile icon' });
      assert.equal(await defaultPhoto.isVisible(), true, 'Creation previews the default profile photo');
      assert.equal(await defaultPhoto.getAttribute('src'), '/tgw/admin/static/webui-icon.svg', 'The default profile photo uses the local Gateway icon');
      assert.equal(await defaultPhoto.evaluate(icon => icon.complete && icon.naturalWidth > 0), true, 'The profile icon loads successfully');
      assert.deepEqual(await defaultPhoto.evaluate(icon => ({ width: icon.width, height: icon.height })), { width: 48, height: 48 }, 'The profile preview stays compact');
      assert.match(await page.locator('.bot-profile-default').innerText(), /New bots use the Gateway icon as their profile photo/);
      assert.equal(await page.locator('#bot-create-dialog').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true, 'The creation form and profile preview fit the viewport');
      assert.equal(await page.locator('#bot-create-form input').count(), 1, 'Creation asks only for a display name and manager');
      assert.equal(await page.locator('#bot-create-username').count(), 0, 'There is no manual username field');
      assert.match(await page.locator('#bot-create-name-help').innerText(), /lowercase.*underscores.*_bot.*adds a number.*Telegram confirms availability/);
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
      await page.locator('#bot-create-name').fill('Blocked bot');
      await page.locator('#bot-create-submit').click();
      await page.waitForFunction(() => !telegramBusy);
      assert.match(await page.locator('#bot-create-error').innerText(), /Telegram still reports.*after refreshing its profile/);
      assert.doesNotMatch(await page.locator('#bot-create-error').innerText(), /refresh the bot list/i);
      state.managedFailure = false;
      for (const [status, code, message] of [
        [502, 'telegram_bot_username_unverified', /Unable to check Telegram usernames/],
        [409, 'telegram_bot_username_unavailable', /Choose a different display name/],
      ]) {
        state.managedError = { status, code };
        const beforeFailure = state.mutations.length;
        await page.locator('#bot-create-submit').click();
        await page.waitForFunction(() => !telegramBusy);
        assert.match(await page.locator('#bot-create-error').innerText(), message);
        assert.equal(await page.locator('#bot-create-name').inputValue(), 'Blocked bot', 'Username lookup errors preserve the display name');
        assert.equal(state.mutations.length, beforeFailure + 1, 'Username lookup failures are not automatically replayed');
      }
      state.managedError = null;
      if (screenshots) await page.screenshot({ path: path.join(screenshots, 'telegram-bot-create-' + width + '.png') });
      await page.locator('#bot-create-cancel').click();
      const managedCreateStarted = new Promise(resolve => { state.onManagedCreateStarted = resolve; });
      state.holdNextManagedCreate = true;
      const creating = create();
      await managedCreateStarted;
      assert.equal(await page.locator('#bot-create-status').innerText(), 'Checking Telegram username…');
      assert.equal(await page.locator('#bot-create-name').isDisabled(), true, 'Creation is locked while checking the username');
      state.releaseManagedCreate();
      await creating;
      assert.deepEqual(state.mutations.at(-1).payload, { manager_id: primaryID, display_name: 'Created ' + malicious }, 'The server receives the display name and generates the username');
      assert.equal(await page.locator('#bot-create-result').isVisible(), true);
      assert.match(await page.locator('#bot-create-result-status').innerText(), /Waiting for Telegram.*@created_2_bot.*15 minutes/, 'The result shows the username selected by the server');
      assert.equal(await page.locator('#bot-create-dialog').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true);
      const launch = await page.locator('#bot-create-launch').getAttribute('href');
      assert.match(launch, /^https:\/\/t.me\/newbot\/primary_bot\/created_2_bot\?name=/);
      const popupPromise = page.waitForEvent('popup');
      await page.locator('#bot-create-launch').click();
      const popup = await popupPromise;
      await popup.waitForLoadState();
      assert.equal(popup.url(), launch);
      assert.deepEqual(externalRequests, [launch]);
      await popup.close();
      state.pending[0].status = 'connected';
      state.pending[0].bot_id = createdID;
      state.bots.push({ id: createdID, telegram_id: 98765, username: 'created_2_bot', display_name: 'Created ' + malicious, enabled: true, managed: true, can_manage_bots: false, status: 'connected' });
      await page.clock.runFor(3100);
      await page.waitForSelector('[data-bot-id="' + createdID + '"]');
      assert.equal(await card(createdID).locator('.bot-toggle').getAttribute('aria-expanded'), 'false', 'A managed bot starts collapsed');
      assert.equal(await card(primaryID).locator('.bot-toggle').getAttribute('aria-expanded'), 'true', 'Pending creation polling preserves existing expansions');
      assert.match(await page.locator('#bot-create-result-status').innerText(), /is connected/);
      assert.equal(await page.locator('#bot-create-launch').isVisible(), false);
      assert.equal(state.profileRefreshes.length, 3, 'Pending-request polling does not force profile refresh');
      assert.equal(await page.evaluate(() => window.injected), undefined);
      await page.locator('#bot-create-done').click();
      await card(createdID).locator('.bot-toggle').click();
      assert.match(await card(createdID).innerText(), /Managed.*Same allowed owner and workers/s);
      await card(createdID).locator('.bot-toggle').click();

      await create('Expires', 'expires_bot');
      state.pending.at(-1).expires_at = new Date(await page.evaluate(() => Date.now()) + 1000).toISOString();
      await page.clock.runFor(3100);
      await page.waitForFunction(() => document.getElementById('bot-create-result-status').textContent.includes('expired'));
      assert.equal(await page.locator('#bot-create-launch').getAttribute('href'), null, 'Expired creation links cannot be opened');
      await page.locator('#bot-create-done').click();
      state.invalidCreationURL = true;
      await create('Invalid link', 'invalid_link_bot');
      assert.equal(await page.locator('#bot-create-error').isVisible(), true);
      assert.equal(await page.locator('#bot-create-launch').getAttribute('href'), null, 'Unexpected creation destinations never become links');
      state.invalidCreationURL = false;
      state.pending = state.pending.filter(item => item.requested_username !== 'invalid_link_bot');
      await page.locator('#bot-create-cancel').click();

      const beforeRemove = state.mutations.length;
      await card(additionalID).locator('.bot-toggle').click();
      assert.equal(await card(additionalID).locator('.bot-content').isVisible(), false);
      await card(additionalID).getByRole('button', { name: 'Delete', exact: true }).click();
      assert.equal(await page.locator('#bot-remove-dialog').evaluate(dialog => dialog.open), true);
      assert.match(await page.locator('#bot-remove-description').innerText(), /local bot records, chat bindings and notification subscriptions.*Worker sessions and project files will be kept/s);
      assert.match(await page.locator('#bot-remove-target').innerText(), /@connected_bot.*owner.*select this bot.*confirm deletion/s);
      assert.equal(await page.locator('#bot-remove-dialog').evaluate(dialog => dialog.scrollWidth <= dialog.clientWidth && dialog.getBoundingClientRect().width <= innerWidth), true);
      await page.locator('#bot-remove-cancel').click();
      assert.equal(state.mutations.length, beforeRemove, 'Cancelled removal does nothing');
      await card(additionalID).getByRole('button', { name: 'Delete', exact: true }).click();
      if (screenshots) await page.screenshot({ path: path.join(screenshots, 'telegram-bot-remove-' + width + '.png') });
      await page.locator('#bot-remove-confirm').click();
      await page.waitForFunction(() => !telegramBusy);
      assert.equal(await card(additionalID).count(), 0);
      assert.equal(await card(primaryID).count(), 1, 'Removing another bot preserves the primary bot');
      assert.equal(state.mutations.at(-1).method, 'DELETE');
      assert.deepEqual(state.mutations.at(-1).payload, { confirmed: true });
      assert.equal(await page.locator('#bot-remove-dialog').evaluate(dialog => dialog.open), true, 'Removal keeps Telegram deletion guidance open');
      assert.equal(await page.locator('#bot-remove-confirm').isVisible(), false, 'A completed removal cannot be submitted again');
      assert.match(await page.locator('#bot-remove-target').innerText(), /@connected_bot/, 'The target username survives gateway removal');
      assert.match(await page.locator('#bot-remove-status').innerText(), /Deletion on Telegram is a separate step/);
      const deleteURL = 'https://t.me/BotFather?start=deletebot';
      assert.equal(await page.locator('#bot-remove-launch').getAttribute('href'), deleteURL);
      const deletePopupPromise = page.waitForEvent('popup');
      await page.locator('#bot-remove-launch').click();
      const deletePopup = await deletePopupPromise;
      await deletePopup.waitForLoadState();
      assert.equal(deletePopup.url(), deleteURL, 'A real click opens the supported BotFather deletion flow');
      await deletePopup.close();
      assert.equal(state.mutations.length, beforeRemove + 1, 'BotFather guidance makes no remote deletion request');
      await page.locator('#bot-remove-cancel').click();
      assert.match(await page.locator('#bot-removed').innerText(), /@connected_bot.*owner.*select this bot.*confirm deletion/s);
      assert.equal(await page.locator('#bot-removed').getByRole('link', { name: 'Delete in BotFather' }).getAttribute('href'), deleteURL, 'BotFather remains accessible after the dialog closes');
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
      assert.match(await page.locator('#bot-removed').innerText(), /@connected_bot/, 'Opening Create keeps the completed removal guidance');
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
      assert.equal(await page.locator('#bot-removed').innerText(), '', 'Expiry clears removed bot guidance');
      assert.deepEqual(errors, []);
      await page.close();

      const expiry = await fixture(browser, width);
      expiry.state.bots.push({ id: additionalID, username: 'expiry_bot', display_name: 'Expiry bot', enabled: true, status: 'connected' });
      await expiry.refresh();
      await expiry.card(additionalID).getByRole('button', { name: 'Delete', exact: true }).click();
      assert.match(await expiry.page.locator('#bot-remove-target').innerText(), /@expiry_bot/);
      expiry.state.dashboardStatus = 401;
      await expiry.page.evaluate(() => load(false));
      await expiry.page.waitForSelector('#auth:not([hidden])');
      assert.equal(await expiry.page.locator('#bot-remove-dialog').evaluate(dialog => dialog.open), false, 'Expiry closes the removal dialog');
      assert.equal(await expiry.page.locator('#bot-remove-target').innerText(), '', 'Expiry clears the retained target username');
      assert.equal(await expiry.page.evaluate(() => telegramRemovalTarget === null && telegramRemovedBots.length === 0), true, 'Expiry clears removal snapshots');
      assert.equal(expiry.state.mutations.length, 0, 'Expiring a removal dialog never deletes the bot');
      assert.deepEqual(expiry.errors, []);
      await expiry.page.close();
    }
    console.log('Admin Telegram bot browser checks passed: mobile/desktop collapsed sections and bot cards, confirmed profile-photo reset/cancel/disabled/busy/error/fresh-auth behavior, attention indicators, opt-in session names, token privacy, connect/replace/takeover, profile refresh, managed creation/polling/expiry, safe rendering, gateway removal/BotFather guidance, CSRF, refresh recovery and sign-in expiry.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

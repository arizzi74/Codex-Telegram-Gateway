#!/usr/bin/env node
// Focused Audio console checks; no live gateway or downloaded models required.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const malicious = '<img src=x onerror="window.injected=true">';

function snapshot(availableCPUs = 8) {
  const threads = Math.max(1, Math.min(32, availableCPUs - 1));
  return {
    settings: { enabled: false, model: 'base', threads, reply_with_audio: false, tts_voice: 'auto' },
    resources: { logical_cpus: availableCPUs, available_cpus: availableCPUs, memory_total_bytes: 8 * 1024 ** 3, memory_available_bytes: 5 * 1024 ** 3, recommended_threads: threads, recommended_model: 'base' },
    models: [
      { name: 'base', label: 'Base', size_bytes: 147951465, installed: false, recommended: true, advanced: false },
      { name: 'small', label: 'Small', size_bytes: 487601967, installed: false, recommended: false, advanced: true },
    ],
    helper: { available: true }, tts: { available: false, voices: [{ id: 'auto', label: 'Match the voice note (Italian or English)' }, { id: 'italian', label: 'Italian' }, { id: 'english', label: 'English' }] },
    operation: { state: 'idle', model: '', downloaded_bytes: 0, total_bytes: 0, message: '' }, busy: false,
  };
}

async function fixture(browser, width, value = snapshot()) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const state = { audio: value, posts: [], postStatus: 202, readStatus: 200, sessionStatus: 200, credentialCalls: 0, requireReauth: false };
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('https://admin.test/**', async route => {
    const request = route.request();
    const url = new URL(request.url());
    let body = {};
    let status = 200;
    if (url.pathname.startsWith('/tgw/api/')) {
      if (url.pathname.endsWith('/login/options')) body = { passkey: true, password: false };
      else if (url.pathname.endsWith('/admin/session')) {
        status = state.sessionStatus;
        const now = await page.evaluate(() => Date.now());
        body = { authenticated: true, session_id: 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa', server_time: new Date(now).toISOString(), expires_at: new Date(now + 8 * 3600000).toISOString(), reauthenticated_at: new Date(now).toISOString() };
      } else if (url.pathname.endsWith('/login/begin')) body = { ceremony_id: 'test', publicKey: { challenge: 'dGVzdA', rpId: 'admin.test' } };
      else if (url.pathname.endsWith('/login/finish')) { state.credentialCalls++; state.sessionStatus = 200; body = { ok: true }; }
      else if (url.pathname.endsWith('/dashboard')) body = { workers: [], runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0 };
      else if (url.pathname.endsWith('/passkeys')) body = [];
      else if (url.pathname.endsWith('/admin/sessions')) body = { sessions: [] };
      else if (url.pathname.endsWith('/admin/password')) body = { enabled: false, username: '' };
      else if (url.pathname.endsWith('/admin/audio') && request.method() === 'GET') {
        status = state.readStatus;
        body = status === 200 ? state.audio : { message: 'Audio service temporarily unavailable.' };
      } else if (url.pathname.endsWith('/admin/audio/settings') && request.method() === 'POST') {
        const settings = JSON.parse(request.postData());
        state.posts.push({ settings, csrf: request.headers()['x-csrf-token'] });
        status = state.requireReauth ? 403 : state.postStatus;
        if (state.requireReauth) { state.requireReauth = false; body = { code: 'reauthentication_required' }; }
        else if (status === 202) {
          state.submitted = settings;
          state.audio.busy = true;
          state.audio.operation = { state: 'downloading', model: settings.model, downloaded_bytes: 25 * 1024 ** 2, total_bytes: 100 * 1024 ** 2, message: '' };
          body = state.audio;
        } else body = { code: status === 409 ? 'audio_busy' : 'invalid_audio_settings', message: status === 409 ? 'Audio setup is already running. Wait and refresh.' : 'Choose between 1 and 32 CPU threads.' };
      }
      return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    }
    const filename = /\.(js|css)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html',
      headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(() => {
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async () => {
      if (window.failCredentials) throw new DOMException('Verification cancelled.', 'NotAllowedError');
      return { id: 'passkey', rawId: new Uint8Array([1]).buffer, type: 'public-key',
        response: { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer } };
    } });
  });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'audio-csrf', url: 'https://admin.test/', secure: true, sameSite: 'Strict' }]);
  await page.clock.install();
  await page.goto('https://admin.test/tgw/admin/');
  await page.waitForFunction(() => !document.getElementById('audio-enabled').disabled);
  async function refresh() {
    await Promise.all([
      page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/admin/audio')),
      page.locator('#refresh').click(),
    ]);
    await page.waitForFunction(() => !document.getElementById('refresh').disabled);
  }
  async function poll() {
    const response = page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/admin/audio'));
    await page.clock.runFor(1100);
    await response;
  }
  async function finish(message = '') {
    state.audio.settings = { ...state.submitted };
    state.audio.models.find(model => model.name === state.submitted.model).installed = true;
    state.audio.busy = false;
    state.audio.operation = { state: 'ready', message };
    await poll();
    await page.waitForFunction(expected => document.getElementById('audio-status').textContent === expected, message || 'Audio settings applied.');
  }
  return { page, state, errors, refresh, poll, finish };
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const width of [390, 1280]) {
      const { page, state, errors, refresh, poll, finish } = await fixture(browser, width);
      const enabled = page.locator('#audio-enabled');
      const threads = page.locator('#audio-threads');
      const save = page.locator('#audio-save');
      assert.equal(await enabled.isChecked(), false);
      assert.equal(await threads.isDisabled(), true, 'Audio options are disabled until enabled');
      assert.equal(await page.locator('#audio-model-small').isDisabled(), true);
      assert.equal(await page.locator('#audio-engine-wrap').isVisible(), false, 'Engine choices appear only for audio replies');
      assert.match(await page.locator('#audio-resources').innerText(), /8.*Available CPU cores.*8.*Total RAM.*8 GiB/s);
      await enabled.check();
      assert.equal(await page.locator('#audio-model-base').isChecked(), true);
      assert.equal(await threads.inputValue(), '7');
      assert.match(await page.locator('#audio-threads-help').innerText(), /Recommended: 7 threads/);
      assert.equal(await page.locator('#audio-reply').isDisabled(), true, 'TTS cannot be enabled without the engine');
      state.audio.tts.available = true;
      await refresh();
      assert.equal(await enabled.isChecked(), true, 'Refresh preserves unsaved enable selection');
      await page.locator('#audio-model-small').check();
      await threads.fill('4');
      await page.locator('#audio-reply').check();
      assert.equal(await page.locator('#audio-voice-wrap').isVisible(), true);
      assert.equal(await page.locator('input[name="audio-engine"]').count(), 2, 'Exactly the two supported engines are offered');
      assert.equal(await page.locator('input[name="audio-engine"]:checked').count(), 0, 'A recommendation does not silently select an engine');
      assert.match(await page.locator('#audio-engine-pending').innerText(), /Choose an audio reply engine/);
      await save.click();
      assert.equal(state.posts.length, 0, 'Audio replies require an explicit engine selection');
      await page.locator('#audio-engine-piper').check();
      assert.match(await page.locator('#audio-voice-help').innerText(), /Italian voice.*English.*Italian accent/);
      assert.match(await page.locator('#audio-engine-wrap').innerText(), /3\.8 s total for 25\.2 s.*298 MiB.*77 MiB/s);
      assert.match(await page.locator('#audio-engine-wrap').innerText(), /78 s total for 20\.3 s.*5 GiB.*1\.38 GiB/s);
      assert.match(await page.locator('#audio-engine-wrap').innerText(), /6 GiB of available RAM.*8 GiB or larger host/);
      assert.match(await page.locator('#audio-engine-measurements').innerText(), /individual measurements, not averages.*3 threads.*4-core ARM/s);
      assert.match(await page.locator('#audio-engine-wrap').innerText(), /noncommercial license/);
      await page.locator('#audio-voice').selectOption('italian');
      await refresh();
      assert.equal(await page.locator('#audio-model-small').isChecked(), true);
      assert.equal(await threads.inputValue(), '4');
      assert.equal(await page.locator('#audio-reply').isChecked(), true);
      assert.equal(await page.locator('#audio-voice').inputValue(), 'italian');
      assert.equal(await page.locator('#audio-engine-piper').isChecked(), true, 'Refresh preserves the selected engine');
      assert.equal(await page.locator('#audio-unsaved').isVisible(), true);
      assert.equal(await save.innerText(), 'Download and apply');
      if (process.env.ADMIN_AUDIO_SCREENSHOT_DIR) {
        fs.mkdirSync(process.env.ADMIN_AUDIO_SCREENSHOT_DIR, { recursive: true });
        await page.locator('#audio-section').screenshot({ path: path.join(process.env.ADMIN_AUDIO_SCREENSHOT_DIR, 'admin-audio-' + width + '.png') });
      }
      await save.click();
      await page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
      assert.deepEqual(state.posts[0], { settings: { enabled: true, model: 'small', threads: 4, reply_with_audio: true, tts_voice: 'italian', tts_engine: 'piper' }, csrf: 'audio-csrf' });
      assert.equal(await enabled.isDisabled(), true, 'No concurrent audio changes during setup');
      assert.equal(await page.locator('#audio-progress').getAttribute('value'), '25');
      assert.match(await page.locator('#audio-effective').innerText(), /Voice notes disabled/);
      state.audio.operation.downloaded_bytes = 75 * 1024 ** 2;
      await poll();
      await page.waitForFunction(() => document.getElementById('audio-progress').value === 75);
      state.audio.busy = false;
      state.audio.operation = { state: 'failed', message: 'Model checksum failed. ' + malicious };
      await poll();
      await page.locator('#audio-error').waitFor({ state: 'visible' });
      assert.match(await page.locator('#audio-error').innerText(), /Model checksum failed/);
      assert.equal(await page.locator('#audio-model-small').isChecked(), true, 'Failed setup keeps the proposed settings for retry');
      assert.equal(await page.locator('#audio-section img').count(), 0, 'Server error remains text');
      assert.equal(await page.evaluate(() => window.injected), undefined);
      await save.click();
      await page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
      await finish();
      assert.equal(await save.isDisabled(), true);
      assert.equal(await page.locator('#audio-unsaved').isVisible(), false);
      assert.match(await page.locator('#audio-effective').innerText(), /Small · 4 threads · text and audio replies/);
      await page.reload();
      await page.waitForFunction(() => !document.getElementById('audio-enabled').disabled);
      assert.equal(await enabled.isChecked(), true, 'Saved settings survive a browser reload');
      assert.equal(await threads.inputValue(), '4', 'An existing thread selection is preserved');
      assert.equal(await page.locator('#audio-engine-piper').isChecked(), true, 'Saved engine selection survives reload');
      await page.locator('#audio-engine-qwen3').check();
      assert.equal(await page.locator('#audio-voice').inputValue(), 'italian', 'Switching engine preserves the language preference');
      assert.match(await page.locator('#audio-voice-help').innerText(), /Italian and English.*same model download/);
      await refresh();
      assert.equal(await page.locator('#audio-engine-qwen3').isChecked(), true, 'Refresh keeps an unsaved engine switch');
      await page.locator('#audio-discard').click();
      assert.equal(await page.locator('#audio-engine-piper').isChecked(), true, 'Discard restores the active engine');
      await threads.fill('32');
      await page.locator('#audio-use-recommended').click();
      assert.equal(await threads.inputValue(), '7');
      await page.locator('#audio-discard').click();
      assert.equal(await threads.inputValue(), '4');
      await threads.fill('33');
      await save.click();
      assert.equal(state.posts.length, 2, 'Out of range input cannot submit');
      await threads.fill('');
      await refresh();
      assert.equal(await threads.inputValue(), '', 'Refresh preserves a partially edited number');
      await threads.fill('5');
      state.postStatus = 409;
      await save.click();
      await page.waitForFunction(() => document.getElementById('audio-error').textContent.includes('already running'));
      assert.equal(await threads.inputValue(), '5');
      state.postStatus = 400;
      await save.click();
      await page.waitForFunction(() => document.getElementById('audio-error').textContent.includes('between 1 and 32'));
      state.postStatus = 202;
      await page.evaluate(() => { window.failCredentials = true; });
      state.requireReauth = true;
      const beforeCancellation = state.posts.length;
      await save.click();
      await page.waitForFunction(() => document.getElementById('audio-error').textContent.includes('Verification was cancelled'));
      assert.equal(state.posts.length, beforeCancellation + 1, 'Cancelled verification does not replay the rejected mutation');
      assert.equal(await threads.inputValue(), '5');
      await page.evaluate(() => { window.failCredentials = false; });
      state.requireReauth = true;
      await save.click();
      await page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
      assert.equal(state.credentialCalls, 1, 'An explicit recent-auth rejection verifies before resubmission');
      await finish();
      state.readStatus = 503;
      await refresh();
      assert.match(await page.locator('#audio-error').innerText(), /temporarily unavailable/);
      assert.equal(await threads.inputValue(), '5', 'Temporary read failure keeps known settings');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Audio console fits viewport ' + width);
      assert.deepEqual(await page.evaluate(() => ({ local: { ...localStorage }, session: { ...sessionStorage } })), { local: {}, session: {} }, 'Audio form does not persist browser storage');
      assert.deepEqual(errors, [], 'No browser errors at width ' + width);
      await page.close();
    }
    for (const cpus of [1, 64]) {
      const { page } = await fixture(browser, 390, snapshot(cpus));
      assert.equal(await page.locator('#audio-threads').inputValue(), cpus === 1 ? '1' : '32', 'Recommended threads stay within limits');
      await page.close();
    }
    const legacySpeech = snapshot(4);
    legacySpeech.tts.available = true;
    legacySpeech.settings = { ...legacySpeech.settings, enabled: true, reply_with_audio: true, tts_voice: 'english' };
    const legacy = await fixture(browser, 390, legacySpeech);
    assert.equal(await legacy.page.locator('input[name="audio-engine"]:checked').count(), 0, 'Legacy settings remain visibly unselected');
    assert.match(await legacy.page.locator('#audio-engine-pending').innerText(), /existing Pocket TTS engine remains active/);
    assert.match(await legacy.page.locator('#audio-effective').innerText(), /existing Pocket TTS/);
    await legacy.refresh();
    assert.equal(legacy.state.posts.length, 0, 'Reading legacy settings never submits a migration');
    await legacy.page.locator('#audio-threads').fill('2');
    await legacy.page.locator('#audio-save').click();
    assert.equal(legacy.state.posts.length, 0, 'An old audio reply preference still requires an explicit engine before switching');
    await legacy.page.locator('#audio-engine-qwen3').check();
    await legacy.page.locator('#audio-save').click();
    await legacy.page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
    assert.equal(legacy.state.submitted.tts_engine, 'qwen3');
    assert.equal(legacy.state.submitted.tts_voice, 'english');
    const cleanupWarning = 'Audio settings saved. The selected audio reply engine is active, but unused model files could not all be removed.';
    await legacy.finish(cleanupWarning);
    assert.match(await legacy.page.locator('#audio-status').innerText(), /selected audio reply engine is active.*could not all be removed/);
    assert.equal(await legacy.page.locator('#audio-error').isVisible(), false, 'Cleanup warning does not claim activation failed');
    assert.match(await legacy.page.locator('#audio-effective').innerText(), /Qwen3-TTS 1\.7B quantized/);
    await legacy.page.close();
    const unavailableSpeech = snapshot();
    unavailableSpeech.settings.enabled = true;
    unavailableSpeech.settings.reply_with_audio = true;
    const recovering = await fixture(browser, 390, unavailableSpeech);
    assert.equal(await recovering.page.locator('#audio-reply').isDisabled(), false, 'An unavailable engine still allows disabling a saved audio-reply preference');
    await recovering.page.locator('#audio-reply').uncheck();
    assert.equal(await recovering.page.locator('#audio-reply').isDisabled(), true);
    await recovering.page.locator('#audio-save').click();
    await recovering.page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
    assert.equal(recovering.state.submitted.reply_with_audio, false);
    await recovering.page.close();
    const stale = await fixture(browser, 390);
    await stale.page.locator('#audio-enabled').check();
    stale.state.sessionStatus = 401;
    await stale.page.locator('#audio-save').click();
    await stale.page.waitForFunction(() => !document.getElementById('console').hidden && !document.getElementById('audio-enabled').disabled);
    assert.equal(stale.state.posts.length, 0, 'A form cleared by session expiry cannot submit after a subsequent login');
    assert.equal(await stale.page.locator('#audio-enabled').isChecked(), false);
    await stale.page.close();
    const expiring = await fixture(browser, 390);
    await expiring.page.locator('#audio-enabled').check();
    await expiring.page.locator('#audio-save').click();
    await expiring.page.waitForFunction(() => !document.getElementById('audio-progress-wrap').hidden);
    expiring.state.readStatus = 401;
    await expiring.poll();
    await expiring.page.waitForFunction(() => document.getElementById('console').hidden);
    assert.equal(await expiring.page.locator('#audio-enabled').isDisabled(), true, 'Expired sessions clear and lock the form');
    assert.deepEqual(expiring.errors, [], 'An expired background poll has no unhandled rejection');
    await expiring.page.close();
    console.log('Admin Audio browser checks passed (mobile and desktop).');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

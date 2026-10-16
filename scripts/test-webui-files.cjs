#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const first = '11111111-2222-4333-8444-555555555555', second = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
const reportPath = '/home/ubuntu/CODEX/CodeNow/docs/NOW-LLM-ARTIFACT-BENCHMARK.md';
const reportName = path.basename(reportPath);
const reportLabel = 'Complete artifact matrix and performance report';
const targets = [
  [reportLabel, reportPath], ['Relative report', 'docs/report.md:12:3'], ['Home report', '~/report.md#L12'],
  ['Encoded file', 'file:///home/ubuntu/matrix%20report.md#L8'], ['Localhost file', 'file://localhost/home/ubuntu/report.md'],
  ['Spaced report', '/home/ubuntu/Complete report (final).md'], ['Bare filename', 'report.md:12']
];
const unsafe = [ ['Unsafe script', 'javascript:alert(1)'], ['Unsafe numeric script', 'javascript:12'], ['Unsafe data', 'data:text/html;base64,PHNjcmlwdD4='], ['Remote file', 'file://remote.invalid/report.md'], ['Remote relative', '//remote.invalid/report.md'], ['Fragment', '#section'], ['Null path', '/home/ubuntu/bad\0name.md'], ['Vertical tab path', '/home/ubuntu/bad\vname.md'], ['Terminal path', '/home/ubuntu/\u001b[31mname.md'], ['Nested null path', '/dir(foo)/bad\0name.md'], ['Nested tab path', '/dir(foo)/bad\vname.md'], ['Nested terminal path', '/dir(foo)/\u001b[31mname.md'] ];
const markdown = targets.map(([label, target]) => '[' + label + '](' + (target.includes(' ') ? '<' + target + '>' : target) + ')').join('\n\n') + '\n\n' + unsafe.map(([label, target]) => '[' + label + '](' + target + ')').join(' ') + '\n\n[Documentation](https://example.com/docs)\n\n> [Quoted report](./quoted.md)\n\n| File | Description |\n| --- | --- |\n| [Table report](./table.md) | Results |';
const sessions = [first, second].map((id, index) => ({ session_id: id, codex_thread_id: 'thread-' + id, worker_id: 'worker-' + index, worker_name: 'Artifact worker ' + (index + 1), runtime_name: 'Codex', name: 'Conversation ' + (index + 1), cwd: '/projects/demo', state: 'idle' }));
const contents = Buffer.from('# Complete artifact matrix\n\nPerformance results: 42 ms.\n');
(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, colorScheme: 'dark', acceptDownloads: true });
  const page = await context.newPage();
  const errors = [], requests = [], held = [];
  let mode = 'ok', status = 200, errorMessage = '', auth = true;
  await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'file-test-csrf', url: 'https://files.test', secure: true, sameSite: 'Strict' }]);
  await context.route('https://files.test/**', async route => {
    const request = route.request(), url = new URL(request.url());
    const json = (value, code = 200) => route.fulfill({ status: code, contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname === '/tgw/api/v1/admin/login/options') return json({ passkey: true, password: false });
    if (url.pathname === '/tgw/api/v1/admin/session') return json({ authenticated: auth, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString(), reauthenticated_at: new Date().toISOString() }, auth ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions }, auth ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/webui/telegram-bots') return json({ revision: 'fixture-bots', bots: [] });
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
    if (url.pathname === '/tgw/api/v1/webui/files') {
      const body = request.postDataJSON();
      requests.push({ ...body, csrf: request.headers()['x-csrf-token'], method: request.method() });
      if (mode === 'hold') { held.push({ route, body }); return; }
      if (mode === 'network') return route.abort('failed');
      if (status !== 200) return json({ message: errorMessage }, status);
      if (body.download) return route.fulfill({ status: 200, contentType: 'application/octet-stream', headers: { 'Content-Disposition': 'attachment; filename="' + reportName + '"', 'Content-Length': String(contents.length) }, body: contents });
      return json({ name: reportName, path: reportPath, size: mode === 'oversize' ? 128 * 1024 * 1024 + 1 : contents.length });
    }
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', headers: { 'Content-Security-Policy': "default-src 'self'; base-uri 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; object-src 'none'; connect-src 'self'" }, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await context.addInitScript(({ sessions, markdown }) => {
    window.fileSockets = []; window.fileAborts = []; window.fileCreatedURLs = []; window.fileRevokedURLs = [];
    const originalFetch = window.fetch.bind(window);
    window.fetch = (input, options) => {
      if (String(input).endsWith('/webui/files')) {
        const request = JSON.parse(options.body);
        options?.signal?.addEventListener('abort', () => window.fileAborts.push(request));
        if (request.download && window.fileResponseMode) {
          const headers = { 'Content-Type': 'application/octet-stream' };
          if (window.fileResponseMode !== 'missing') headers['Content-Length'] = window.fileResponseMode === 'truncated' ? '3' : window.fileResponseMode === 'overrun' ? '1' : String(128 * 1024 * 1024 + 1);
          return Promise.resolve(new Response(new Uint8Array([1, 2]), { headers }));
        }
      }
      return originalFetch(input, options);
    };
    const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
    URL.createObjectURL = blob => { const url = create(blob); window.fileCreatedURLs.push(url); return url; };
    URL.revokeObjectURL = url => { window.fileRevokedURLs.push(url); return revoke(url); };
    class Socket {
      static OPEN = 1;
      constructor(url) {
        const target = new URL(url); this.activity = target.pathname.endsWith('/activity'); this.sessionID = target.searchParams.get('session_id'); this.readyState = 0;
        window.fileSockets.push(this);
        setTimeout(() => { if (this.readyState === 3) return; this.readyState = 1; this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: sessions.map(session => ({ session_id: session.session_id, state: 'idle', worker_connectivity: 'connected', pending_questions: 0 })) } : { type: 'ready' }); }, 5);
      }
      emit(value) { if (value.type === 'activity_snapshot') value = { telegram_bots: [], telegram_bots_revision: 'fixture-bots', ...value }; if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
      send(raw) {
        const frame = JSON.parse(raw); let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: 'thread-' + this.sessionID } };
        if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'completed', startedAt: 1780000000 }], nextCursor: null };
        if (frame.method === 'thread/items/list') result = { data: [{ turnId: 'turn', item: { id: 'report', type: 'agentMessage', text: markdown, createdAt: 1780000000 } }], nextCursor: null };
        if (frame.method === 'gateway/questions') result = { questions: [] };
        if (frame.method === 'model/list') result = { data: [] };
        if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    }
    window.WebSocket = Socket;
  }, { sessions, markdown });
  page.on('pageerror', error => errors.push(error.message));
  const ready = () => page.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected');
  const link = label => page.locator('#messages').getByRole('link', { name: label, exact: true });
  const detailsReady = () => page.waitForFunction(() => document.querySelector('#file-message').textContent === 'Ready to download.');
  const close = async () => {
    await page.locator('#file-dialog').evaluate(dialog => {
      dialog.dataset.testCloseComplete = 'false';
      // Removing `open` is synchronous; the native close event runs later,
      // after the application's handler restores focus and aborts requests.
      dialog.addEventListener('close', () => { dialog.dataset.testCloseComplete = 'true'; }, { once: true });
    });
    await page.getByRole('button', { name: 'Close file details', exact: true }).click();
    await page.waitForFunction(() => { const dialog = document.querySelector('#file-dialog'); return !dialog.open && dialog.dataset.testCloseComplete === 'true'; });
  };
  const release = async (value = { name: 'STALE.md', path: '/stale/path.md', size: 2 }) => {
    const pending = held.splice(0);
    for (const { route, body } of pending) await route.fulfill(body.download ? { contentType: 'application/octet-stream', body: contents } : { contentType: 'application/json', body: JSON.stringify(value) }).catch(() => {});
  };
  try {
    await page.goto('https://files.test/tgw/webui/?session_id=' + first); await ready();
    assert.equal(requests.length, 0, 'Rendering file links makes no file requests');
    assert.equal(await link(reportLabel).getAttribute('title'), reportPath, 'The full worker path is exposed without replacing its label');
    assert.equal(await link(reportLabel).getAttribute('target'), null, 'Local files stay in this application');
    assert.equal(await link('Documentation').getAttribute('target'), '_blank', 'External links keep existing browser behavior');
    assert.equal(await link('Documentation').getAttribute('href'), 'https://example.com/docs');
    for (const [label] of unsafe) assert.equal(await page.locator('#messages').getByRole('link', { name: label, exact: true }).count(), 0, label + ' stays plain text');
    assert.equal(await page.locator('#messages script, #messages img, #messages a[href^="javascript:"]').count(), 0);
    assert.equal(await link('Quoted report').count(), 1, 'Nested Markdown keeps file actions');
    assert.equal(await link('Table report').count(), 1, 'Table Markdown keeps file actions');
    assert.equal(await page.locator('#messages table a.file-link').count(), 1);
    for (const [label, target] of targets) {
      await link(label).click(); await detailsReady();
      assert.deepEqual(requests.at(-1), { session_id: first, path: target, download: false, csrf: 'file-test-csrf', method: 'POST' }, 'The raw target and its rendered session are sent with CSRF protection');
      assert.equal(await page.locator('#file-name').textContent(), reportName);
      assert.equal(await page.locator('#file-path').textContent(), reportPath);
      assert.match(await page.locator('#file-size').textContent(), /bytes/);
      assert.equal(await page.locator('#file-worker').textContent(), 'Artifact worker 1');
      assert.equal(requests.filter(request => request.download).length, 0, 'Metadata never downloads the file automatically');
      await close();
      assert.equal(await link(label).evaluate(element => element === document.activeElement), true, 'Closing returns focus to the link');
    }
    await page.locator('#prompt').fill('Keep this unsent draft');
    const originalURL = page.url();
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
      await link(reportLabel).click(); await detailsReady();
      assert.equal(await page.locator('#file-dialog').evaluate(dialog => { const bounds = dialog.getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight && dialog.scrollWidth <= dialog.clientWidth; }), true, 'File details fit the ' + width + 'px viewport');
      if (process.env.WEBUI_SCREENSHOTS && width !== 320) {
        fs.mkdirSync(process.env.WEBUI_SCREENSHOTS, { recursive: true });
        await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-files-' + (width === 1440 ? 'desktop' : 'mobile') + '.png') });
      }
      const downloadEvent = page.waitForEvent('download');
      await page.getByRole('button', { name: 'Download', exact: true }).click();
      const download = await downloadEvent;
      assert.equal(download.suggestedFilename(), reportName);
      assert.deepEqual(fs.readFileSync(await download.path()), contents, 'Downloaded bytes are intact at ' + width + 'px');
      assert.equal(page.url(), originalURL, 'Download preserves the conversation URL');
      assert.equal(await page.locator('#prompt').inputValue(), 'Keep this unsent draft', 'Download preserves the composer');
      assert.equal(await page.locator('#messages article').count(), 1, 'Download preserves the transcript');
      await page.waitForFunction(() => document.querySelector('#file-message').textContent === 'Download started.');
      await close();
      await page.waitForFunction(() => window.fileCreatedURLs.every(url => window.fileRevokedURLs.includes(url)));
    }
    status = 404; errorMessage = 'The file no longer exists on this worker.';
    await link(reportLabel).click();
    await page.waitForFunction(() => document.querySelector('#file-message').textContent.includes('no longer exists'));
    assert.equal(await page.locator('#file-download').isDisabled(), true);
    assert.equal(await page.locator('#file-retry').isVisible(), true);
    status = 200; await page.locator('#file-retry').click(); await detailsReady(); await close();
    status = 503; errorMessage = 'This worker is offline. Reconnect it and try again.';
    await link(reportLabel).click(); await page.waitForFunction(() => document.querySelector('#file-message').textContent.includes('offline')); await close();
    status = 200; mode = 'network';
    await link(reportLabel).click(); await page.waitForFunction(() => document.querySelector('#file-message').textContent.includes('Check your connection')); await close();
    mode = 'oversize';
    await link(reportLabel).click(); await page.waitForFunction(() => document.querySelector('#file-message').textContent.includes('128 MiB'));
    assert.equal(await page.locator('#file-download').isDisabled(), true); await close();
    mode = 'ok'; await link(reportLabel).click(); await detailsReady();
    status = 409; errorMessage = 'The file changed. Open its details again.';
    await page.locator('#file-download').click(); await page.waitForFunction(() => document.querySelector('#file-message').textContent.includes('file changed'));
    assert.equal(await page.locator('#file-download').isEnabled(), true, 'A rejected download remains retryable'); await close(); status = 200;
    mode = 'ok'; await link(reportLabel).click(); await detailsReady();
    const urlsBeforeInvalid = await page.evaluate(() => window.fileCreatedURLs.length);
    for (const responseMode of ['missing', 'truncated', 'overrun', 'oversize']) {
      await page.evaluate(value => { window.fileResponseMode = value; }, responseMode);
      await page.locator('#file-download').click();
      await page.waitForFunction(() => document.querySelector('#file-message').dataset.error === 'true');
      assert.equal(await page.evaluate(() => window.fileCreatedURLs.length), urlsBeforeInvalid, responseMode + ' download bytes never create a usable download');
      assert.equal(await page.locator('#file-download').isEnabled(), true, responseMode + ' download remains retryable');
    }
    await page.evaluate(() => { window.fileResponseMode = ''; }); await close();

    mode = 'hold'; const abortsBefore = await page.evaluate(() => window.fileAborts.length);
    await link(reportLabel).click(); await page.waitForFunction(() => document.querySelector('#file-dialog').open);
    while (!held.length) await page.waitForTimeout(10);
    await close();
    await page.waitForFunction(count => window.fileAborts.length > count, abortsBefore);
    mode = 'ok'; await link('Relative report').click(); await detailsReady(); await release();
    assert.equal(await page.locator('#file-name').textContent(), reportName, 'Late metadata cannot overwrite a new file dialog'); await close();

    mode = 'hold'; await link(reportLabel).click(); while (!held.length) await page.waitForTimeout(10);
    await page.locator('.session-button[data-session-id="' + second + '"]').evaluate(button => button.click()); await ready();
    assert.equal(await page.locator('#file-dialog').isVisible(), false, 'Switching sessions closes the file dialog');
    await release(); assert.equal(await page.locator('#file-name').textContent(), '', 'Old metadata cannot enter the selected session');
    const beforeStale = requests.length;
    await page.evaluate(first => {
      const article = document.createElement('article'); article.className = 'message'; article.dataset.fileSessionId = first;
      article.append(window.CodexFormat.markdown('[Old rendered file](/old-session.md)')); document.querySelector('#messages').append(article);
      article.querySelector('a').click(); article.remove();
    }, first);
    assert.equal(requests.length, beforeStale, 'A stale rendered link cannot request a file in the newly selected session');
    mode = 'ok'; await link(reportLabel).click(); await detailsReady();
    assert.equal(requests.at(-1).session_id, second, 'Newly rendered links use their own session');
    mode = 'hold'; await page.locator('#file-download').click(); while (!held.length) await page.waitForTimeout(10);
    const countDuringDownload = requests.length;
    await page.locator('#file-download').evaluate(button => button.click());
    assert.equal(requests.length, countDuringDownload, 'A pending download cannot be submitted twice');
    const urlsBeforeDisconnect = await page.evaluate(() => window.fileCreatedURLs.length);
    await page.evaluate(() => document.querySelector('#disconnect').click());
    assert.equal(await page.locator('#file-dialog').isVisible(), false, 'Disconnect closes pending downloads');
    assert.equal(await page.evaluate(() => window.fileAborts.at(-1).download), true, 'Disconnect aborts the download fetch');
    await release();
    assert.equal(await page.evaluate(() => window.fileCreatedURLs.length), urlsBeforeDisconnect, 'A late download cannot create a stale URL');
    const requestsBeforeDisconnectedClick = requests.length;
    await link(reportLabel).click();
    assert.match(await page.locator('#file-message').textContent(), /Reconnect to this session/);
    assert.equal(await page.locator('#file-download').isDisabled(), true);
    assert.equal(requests.length, requestsBeforeDisconnectedClick, 'A disconnected viewer explains how to continue without fetching');
    await close();
    mode = 'ok'; await page.locator('#reconnect').click(); await ready();
    auth = false; status = 401; errorMessage = 'Sign in to continue.';
    await link(reportLabel).click(); await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#file-dialog').isVisible(), false, 'Expired authentication closes the dialog');
    assert.equal(await page.locator('#file-path').textContent(), '', 'Expired authentication clears file details');
    assert.equal(await page.locator('#messages article').count(), 0, 'The existing authentication flow clears the transcript');
    assert.deepEqual(errors, [], 'File actions cause no browser errors');
    console.log('Web UI file browser checks passed (desktop, mobile, downloads, errors and session races).');
  } finally { await release(); await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const first = '11111111-2222-4333-8444-555555555555', second = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
const sessions = [first, second].map((id, index) => ({ session_id: id, codex_thread_id: 'thread-' + id, worker_id: 'worker', worker_name: 'Test worker', runtime_name: 'Codex', name: 'Conversation ' + (index + 1) + ' with a very long session name '.repeat(5), cwd: '/projects/demo', state: 'idle' }));
const initialBots = [
  { id: 'alpha', display_name: 'Enki Codex', session_id: '', username: 'enki_username', telegram_name: 'Enki Codex - inherited session suffix' },
  { id: 'beta', display_name: 'Orbit ' + 'a long original bot name '.repeat(14), session_id: second },
  { id: 'hostile', display_name: '<img src=x onerror="window.injected=true">', session_id: '' },
];

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const width of [1440, 390, 320]) {
      const context = await browser.newContext({ viewport: { width, height: width === 1440 ? 1000 : 844 }, colorScheme: 'dark' });
      const page = await context.newPage();
      const errors = [], posts = [], heldPosts = [], heldReads = [], loginRequests = [];
      const pages = [page];
      let bots = structuredClone(initialBots), revision = 1, listedSessions = sessions, mode = 'ok', holdReads = false;
      const snapshot = () => ({ revision: 'bots-' + revision, bots: structuredClone(bots) });
      const waitForRequest = async requests => {
        for (let attempt = 0; attempt < 500; attempt++) {
          if (requests.length) return;
          await new Promise(resolve => setTimeout(resolve, 10));
        }
        throw new Error('Expected a held bot API request');
      };
      await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'bot-test-csrf', url: 'https://bots.test', secure: true, sameSite: 'Strict' }]);
      await context.route('https://bots.test/**', async route => {
        const request = route.request(), url = new URL(request.url());
        const json = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
        if (url.pathname === '/tgw/api/v1/admin/login/options') return json({ passkey: true, password: false });
        if (url.pathname === '/tgw/api/v1/admin/session') return json({ authenticated: true, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString(), reauthenticated_at: new Date().toISOString() });
        if (url.pathname.startsWith('/tgw/api/v1/admin/login/')) { loginRequests.push(url.pathname); return json({}); }
        if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions: listedSessions });
        if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
        if (url.pathname === '/tgw/api/v1/webui/telegram-bots') {
          if (request.method() === 'GET') {
            if (holdReads) { heldReads.push({ route, snapshot: snapshot() }); return; }
            return json(snapshot());
          }
          const body = request.postDataJSON(); posts.push({ body, csrf: request.headers()['x-csrf-token'], method: request.method() });
          if (mode === 'hold') { heldPosts.push({ route, body }); return; }
          if (mode === 'network') return route.abort('failed');
          if (mode === 'reject') return json({ message: 'SECRET_BOT_TOKEN_NEVER_DISPLAY' }, 400);
          if (mode === 'unauthenticated') return json({ message: 'SECRET_BOT_TOKEN_NEVER_DISPLAY' }, 401);
          bots = bots.map(bot => ({ ...bot, session_id: bot.id === body.bot_id ? body.session_id : bot.session_id === body.session_id ? '' : bot.session_id }));
          revision++;
          return json(snapshot());
        }
        const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
        if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
        return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', headers: { 'Content-Security-Policy': "default-src 'self'; base-uri 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; object-src 'none'; connect-src 'self'" }, body: fs.readFileSync(path.join(assets, filename)) });
      });
      await context.addInitScript(({ sessions }) => {
        window.botSockets = []; window.botFrames = []; window.botActivityRows = sessions.map(session => ({ session_id: session.session_id, state: 'idle', worker_connectivity: 'connected', pending_questions: 0 }));
        class Socket {
          static OPEN = 1;
          constructor(url) {
            const target = new URL(url); this.activity = target.pathname.endsWith('/activity'); this.sessionID = target.searchParams.get('session_id'); this.url = url; this.readyState = 0; this.sequence = 0;
            window.botSockets.push(this); if (this.activity) window.botActivity = this;
            setTimeout(() => {
              if (this.readyState === 3) return;
              this.readyState = 1; this.onopen?.({});
              // HTTP owns the initial bot inventory in this fixture; a real v3
              // snapshot then confirms it before activity events are emitted.
              if (!this.activity) this.emit({ type: 'ready' });
            }, 5);
          }
          emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
          close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
          send(raw) {
            const frame = JSON.parse(raw); window.botFrames.push({ sessionID: this.sessionID, ...frame });
            let result = {};
            if (frame.method === 'thread/resume') result = { thread: { id: 'thread-' + this.sessionID }, model: 'model', reasoningEffort: 'high' };
            if (frame.method === 'thread/turns/list') result = { data: [], nextCursor: null };
            if (frame.method === 'thread/items/list') result = { data: [{ turnId: 'turn', item: { id: 'message-' + this.sessionID, type: 'agentMessage', text: 'Saved conversation ' + this.sessionID } }], nextCursor: null };
            if (frame.method === 'gateway/questions') result = { questions: [] };
            if (frame.method === 'model/list') result = { data: [] };
            if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
            setTimeout(() => this.emit({ id: frame.id, result }), 1);
          }
        }
        window.WebSocket = Socket;
      }, { sessions });
      page.on('pageerror', error => errors.push(error.message));
      await page.clock.install({ time: Date.now() });
      const ready = target => target.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden && !document.querySelector('#telegram-bot').disabled);
      const value = (expected, target = page) => target.waitForFunction(expected => document.querySelector('#telegram-bot').value === expected, expected);
      const selectSession = async (id, target = page) => {
        if (width <= 650) await target.locator('#show-sessions').click();
        await target.locator('.session-button[data-session-id="' + id + '"]').click();
        await target.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden);
      };
      const pushSnapshot = async (target = page) => target.evaluate(data => {
        const socket = window.botActivity;
        socket.emit({ type: 'activity_snapshot', version: 3, sequence: socket.sequence, revision: socket.sequence, sessions: window.botActivityRows, telegram_bots: data.bots, telegram_bots_revision: data.revision });
      }, snapshot());
      const publish = async (targets = pages) => {
        revision++;
        for (const target of targets) await target.evaluate(data => {
          const socket = window.botActivity;
          socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, revision: socket.sequence, event: 'telegram_bots_changed', telegram_bots: data.bots, telegram_bots_revision: data.revision });
        }, snapshot());
      };
      try {
        await page.goto('https://bots.test/tgw/webui/');
        await page.waitForFunction(() => document.querySelectorAll('.session-button').length === 2);
        assert.equal(await page.locator('#telegram-bot').isDisabled(), true, 'Choosing a bot requires a selected session');
        await selectSession(first); await ready(page); await pushSnapshot();
        assert.equal(await page.locator('#telegram-bot').inputValue(), '', 'A new session starts with no bot');
        assert.equal(await page.getByRole('combobox', { name: 'Telegram bot' }).count(), 1);
        assert.deepEqual(await page.locator('#telegram-bot option').allTextContents(), ['<no bot>', ...initialBots.map(bot => bot.display_name)]);
        assert.equal(await page.locator('#telegram-bot img').count(), 0, 'Names are rendered as text');
        assert.equal(await page.evaluate(() => window.injected), undefined);
        assert.equal(await page.evaluate(() => new URL(window.botActivity.url).searchParams.get('v')), '3');
        const layout = await page.locator('#connection').evaluate(row => {
          const line = row.getBoundingClientRect(), select = row.querySelector('select').getBoundingClientRect(), text = row.querySelector('#connection-text').getBoundingClientRect();
          return { fits: select.right <= innerWidth && select.left >= text.right && document.documentElement.scrollWidth <= innerWidth, oneRow: Math.abs(select.top + select.height / 2 - (line.top + line.height / 2)) <= 1, height: line.height, ellipsis: getComputedStyle(row.querySelector('#connection-text')).textOverflow };
        });
        assert.equal(layout.fits, true, width + 'px keeps the selector inside the viewport and right of the text');
        assert.equal(layout.oneRow, true); assert.ok(layout.height <= 35, 'The connection line stays compact'); assert.equal(layout.ellipsis, 'ellipsis');
        await selectSession(second); await value('beta');
        await selectSession(first); await value('');
        const beforeAssignment = snapshot();
        await page.locator('#telegram-bot').selectOption('alpha'); await value('alpha'); await ready(page);
        assert.deepEqual(posts.at(-1), { body: { session_id: first, bot_id: 'alpha' }, csrf: 'bot-test-csrf', method: 'POST' });
        await page.evaluate(data => {
          const socket = window.botActivity;
          socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, event: 'telegram_bots_changed', telegram_bots: data.bots, telegram_bots_revision: data.revision });
        }, beforeAssignment);
        await value('alpha');
        assert.equal(await page.locator('#telegram-bot').inputValue(), 'alpha', 'A delayed activity event cannot overwrite a newer HTTP confirmation');
        await selectSession(second); await page.locator('#telegram-bot').selectOption('alpha'); await value('alpha'); await ready(page);
        await selectSession(first); await value('');
        assert.equal(bots.find(bot => bot.id === 'beta').session_id, '', 'Choosing a new bot disconnects the session’s former bot');
        await selectSession(second); await page.locator('#telegram-bot').selectOption(''); await value(''); await ready(page);
        assert.equal(posts.at(-1).body.bot_id, '', 'The literal no-bot choice clears the assignment');

        const peer = await context.newPage(); pages.push(peer); peer.on('pageerror', error => errors.push(error.message));
        await peer.goto('https://bots.test/tgw/webui/?session_id=' + first); await ready(peer); await pushSnapshot(peer);
        const historyBefore = await page.evaluate(() => window.botFrames.filter(frame => frame.method === 'thread/items/list').length);
        const viewersBefore = await page.evaluate(() => window.botSockets.filter(socket => !socket.activity).length);
        bots = bots.map(bot => ({ ...bot, session_id: bot.id === 'alpha' ? first : '' })); await publish();
        await value('alpha', peer); await value('');
        bots = bots.map(bot => ({ ...bot, display_name: bot.id === 'alpha' ? 'Enki original name after /setbotname' : bot.display_name, session_id: bot.id === 'alpha' ? second : '' })); await publish();
        await value('alpha'); await value('', peer);
        assert.equal(await page.locator('#telegram-bot option[value="alpha"]').textContent(), 'Enki original name after /setbotname');
        assert.equal(await page.evaluate(() => window.botFrames.filter(frame => frame.method === 'thread/items/list').length), historyBefore, 'Out-of-band bot updates preserve conversation history');
        assert.equal(await page.evaluate(() => window.botSockets.filter(socket => !socket.activity).length), viewersBefore, 'Out-of-band bot updates preserve the selected viewer');
        await peer.close(); pages.pop();
        const postsBeforeLegacySnapshot = posts.length;
        bots = bots.map(bot => ({ ...bot, session_id: ['alpha', 'beta'].includes(bot.id) ? second : '' })); await publish(); await value('alpha');
        assert.equal(posts.length, postsBeforeLegacySnapshot, 'Displaying legacy duplicate bindings never silently detaches a bot');
        bots = bots.map(bot => ({ ...bot, session_id: bot.id === 'alpha' ? second : '' })); await publish();

        await selectSession(first); mode = 'hold'; await page.locator('#telegram-bot').selectOption('beta');
        await page.waitForFunction(() => document.querySelector('#telegram-bot').disabled);
        await waitForRequest(heldPosts);
        const heldPost = heldPosts.shift(), countWhileBusy = posts.length;
        await selectSession(second); await value('alpha');
        assert.equal(await page.locator('#telegram-bot').isDisabled(), true, 'A pending mutation disables further choices after switching session');
        await page.locator('#telegram-bot').evaluate(select => { select.value = 'hostile'; select.dispatchEvent(new Event('change', { bubbles: true })); });
        assert.equal(posts.length, countWhileBusy, 'A busy change event cannot start a competing mutation');
        const staleAck = { revision: 'stale-post', bots: bots.map(bot => ({ ...bot, session_id: bot.id === 'beta' ? first : '' })) };
        bots = bots.map(bot => ({ ...bot, session_id: bot.id === 'beta' ? second : '' })); await publish(); await value('beta');
        mode = 'ok'; await heldPost.route.fulfill({ contentType: 'application/json', body: JSON.stringify(staleAck) }); await ready(page); await value('beta');
        await selectSession(first); await value(''); await selectSession(second); await value('beta');
        assert.equal(heldPost.body.session_id, first, 'Switching never changes an in-flight request’s captured session');

        holdReads = true; await page.locator('#refresh-sessions').evaluate(button => button.click());
        await waitForRequest(heldReads);
        bots = bots.map(bot => ({ ...bot, session_id: bot.id === 'alpha' ? second : '' })); await publish(); await value('alpha');
        holdReads = false;
        for (const held of heldReads.splice(0)) await held.route.fulfill({ contentType: 'application/json', body: JSON.stringify(held.snapshot) });
        await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled); await value('alpha');
        await page.evaluate(data => {
          const socket = window.botActivity;
          socket.emit({ type: 'activity_event', version: 3, sequence: socket.sequence, event: 'telegram_bots_changed', telegram_bots: data.bots, telegram_bots_revision: 'stale-event' });
        }, staleAck);
        await value('alpha');
        await page.evaluate(() => { window.oldBotHandler = window.botActivity.onmessage; window.oldBotSequence = window.botActivity.sequence; window.botActivity.close(1006); });
        await page.clock.fastForward(1100);
        await page.waitForFunction(() => window.botSockets.filter(socket => socket.activity).length >= 2);
        await pushSnapshot();
        await page.evaluate(data => window.oldBotHandler({ data: JSON.stringify({ type: 'activity_event', version: 3, sequence: window.oldBotSequence + 1, event: 'telegram_bots_changed', telegram_bots: data.bots, telegram_bots_revision: 'stale-transport' }) }), staleAck);
        await value('alpha');

        for (const failure of ['reject', 'network']) {
          mode = failure; await page.locator('#telegram-bot').selectOption('beta'); await ready(page); await value('alpha');
          assert.match(await page.locator('#telegram-bot-status').textContent(), /Could not update/);
          assert.equal((await page.locator('body').textContent()).includes('SECRET_BOT_TOKEN'), false);
          await page.clock.fastForward(6001); assert.equal(await page.locator('#telegram-bot-status').isHidden(), true, 'Assignment errors expire inline');
        }
        mode = 'ok';
        listedSessions = [sessions[0]];
        await page.evaluate(id => {
          const socket = window.botActivity; window.botActivityRows = window.botActivityRows.filter(row => row.session_id !== id);
          socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, event: 'session_removed', session_id: id, session: null });
        }, second);
        await page.waitForFunction(() => document.querySelector('#telegram-bot').disabled);
        assert.equal(await page.locator('#telegram-bot').isDisabled(), true, 'A stale selected session cannot be rebound');
        await selectSession(first); await ready(page);
        holdReads = true; await page.locator('#refresh-sessions').evaluate(button => button.click()); await waitForRequest(heldReads);
        mode = 'unauthenticated'; await page.locator('#telegram-bot').selectOption('alpha');
        await page.waitForFunction(() => !document.querySelector('#auth').hidden);
        holdReads = false;
        for (const held of heldReads.splice(0)) await held.route.fulfill({ contentType: 'application/json', body: JSON.stringify(held.snapshot) }).catch(() => {});
        assert.equal(await page.locator('#telegram-bot').isDisabled(), true);
        assert.deepEqual(await page.locator('#telegram-bot option').allTextContents(), ['<no bot>'], 'Authentication expiry clears private metadata');
        assert.deepEqual(loginRequests, [], 'Bot changes use the existing session without opening a sign-in ceremony');
        assert.deepEqual(errors, []);
        console.log('Telegram bot selector checks passed at ' + width + 'px');
      } finally { await context.close(); }
    }
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

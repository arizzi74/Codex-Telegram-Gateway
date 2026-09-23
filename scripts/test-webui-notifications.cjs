#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { MessageChannel } = require('node:worker_threads');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const subscriptionID = '11111111-2222-4333-8444-555555555555';
const sessionID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
const publicKey = Buffer.from([4, ...Array(64).fill(7)]).toString('base64url');
const storageKey = 'codex-webui-push-subscription';

async function workerChecks() {
  const handlers = new Map(), notifications = [], opened = [];
  let windows = [];
  const self = {
    location: { origin: 'https://push.test' },
    addEventListener: (name, action) => handlers.set(name, action),
    skipWaiting: async () => {},
    registration: { showNotification: async (title, options) => notifications.push({ title, options }) },
    clients: { claim: async () => {}, matchAll: async () => windows, openWindow: async url => opened.push(url) }
  };
  vm.runInNewContext(fs.readFileSync(path.join(assets, 'webui-sw.js'), 'utf8'), { self, URL, MessageChannel, setTimeout: callback => setTimeout(callback, 5), clearTimeout });
  const run = async (name, event) => {
    let finished;
    handlers.get(name)({ ...event, waitUntil: result => { finished = result; } });
    await finished;
  };
  assert.equal(handlers.has('fetch'), false, 'The service worker never intercepts or caches authenticated traffic');
  const payload = { type: 'turn_finished', title: 'Codex', body: 'Sensitive transcript text must not become notification content', tag: subscriptionID, url: '/tgw/webui/?session_id=' + sessionID };
  await run('push', { data: { json: () => payload } });
  await run('push', { data: { json: () => payload } });
  assert.equal(notifications.length, 2, 'Every delivered push displays an OS notification, including while a page is open');
  assert.equal(notifications[0].title, 'Turn finished');
  assert.equal(notifications[0].options.body, 'A Codex turn has finished.');
  assert.equal(notifications[0].options.data.url, payload.url);
  assert.equal(notifications[0].options.tag, subscriptionID);
  await run('push', { data: { json: () => { throw Error('Invalid push'); } } });
  assert.equal(notifications.length, 3, 'Malformed pushes still produce the visible generic notification required by iPhone');
  assert.equal(notifications[2].options.data.url, '/tgw/webui/');
  await run('push', { data: { json: () => ({ url: 'https://evil.test/tgw/webui/?session_id=' + sessionID, tag: '<bad>' }) } });
  assert.equal(notifications[3].options.data.url, '/tgw/webui/');
  assert.equal(notifications[3].options.tag, undefined);
  let closed = 0;
  await run('notificationclick', { notification: { data: { url: payload.url }, close: () => closed++ } });
  assert.deepEqual(opened, [payload.url], 'Closed-app notifications open the gateway session link');
  let focused = 0, message;
  windows = [{ url: 'https://push.test/tgw/webui/', focus: async () => focused++, postMessage: (value, ports) => { message = value; ports[0].postMessage({ type: 'notification-handled' }); ports[0].close(); } }];
  await run('notificationclick', { notification: { data: { url: payload.url }, close: () => closed++ } });
  assert.equal(message.type, 'codex-notification-open');
  assert.equal(message.url, payload.url);
  assert.equal(focused, 1);
  assert.equal(opened.length, 1, 'An existing current page selects the session without reloading its drafts');
  windows = [{ url: 'https://push.test/tgw/webui/', focus: async () => {}, postMessage: () => {} }];
  await run('notificationclick', { notification: { data: { url: 'javascript:alert(1)' }, close: () => closed++ } });
  assert.equal(opened.at(-1), '/tgw/webui/', 'An old tab that cannot handle messages falls back to a safe gateway URL');
  assert.equal(closed, 3);
}

(async () => {
  await workerChecks();
  const browser = await chromium.launch({ headless: true });
  const pages = [];
  async function setup(options = {}) {
    const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
    await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'push-csrf', url: 'https://push.test/', secure: true, sameSite: 'Strict' }]);
    const page = await context.newPage(); pages.push(context);
    const state = { calls: [], enabled: false, subscriptionID, nextSubscriptionID: '', failSubscribe: false, failUnsubscribe: false, holdSubscribe: false, held: null, expired: 0 };
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.route('https://push.test/**', async route => {
      const url = new URL(route.request().url());
      if (url.pathname.startsWith('/tgw/api/v1/webui/push/')) {
        const body = route.request().postDataJSON();
        state.calls.push({ path: url.pathname, method: route.request().method(), body, query: url.search, csrf: route.request().headers()['x-csrf-token'] });
        if (url.pathname.endsWith('/config')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ supported: options.supported !== false, public_key: publicKey, scope: 'all', subscribed: state.enabled && url.searchParams.get('subscription_id') === state.subscriptionID }) });
        if (url.pathname.endsWith('/subscribe')) {
          if (state.holdSubscribe) { state.held = route; return; }
          if (state.failSubscribe) return route.fulfill({ status: 503, body: 'Try later' });
          state.enabled = true;
          if (state.nextSubscriptionID) state.subscriptionID = state.nextSubscriptionID;
          return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ subscription_id: state.subscriptionID, scope: 'all', subscribed: true }) });
        }
        if (state.failUnsubscribe) return route.fulfill({ status: 503, body: 'Try later' });
        if (body.subscription_id === state.subscriptionID) state.enabled = false;
        return route.fulfill({ status: 204 });
      }
      if (url.pathname.endsWith('/webui-notifications.js')) return route.fulfill({ contentType: 'text/javascript', body: fs.readFileSync(path.join(assets, 'webui-notifications.js')) });
      return route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><body><button id="toggle-notifications" aria-describedby="notifications-status"></button><p id="notifications-status" role="status"></p><script src="/tgw/webui/static/webui-notifications.js"></script></body></html>' });
    });
    await page.addInitScript(({ options, key }) => {
      window.testPush = { requests: 0, permissionGestures: [], registrations: [], subscriptions: [], unsubscribed: 0, expired: 0, permission: options.permission || 'default', subscribed: !!localStorage.getItem(key) };
      if (options.iphone) {
        Object.defineProperty(navigator, 'userAgent', { value: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)' });
        Object.defineProperty(navigator, 'platform', { value: 'iPhone' });
        Object.defineProperty(navigator, 'standalone', { value: !!options.standalone });
      }
      const subscription = {
        endpoint: 'https://push.example.test/device',
        toJSON: () => ({ endpoint: 'https://push.example.test/device', keys: { auth: 'browser-auth-key', p256dh: 'browser-public-key' } }),
        unsubscribe: async () => { window.testPush.unsubscribed++; window.testPush.subscribed = false; return true; }
      };
      const registration = { scope: location.origin + '/tgw/webui/', active: {}, pushManager: {
        getSubscription: async () => window.testPush.subscribed ? subscription : null,
        subscribe: async params => { window.testPush.subscriptions.push({ userVisibleOnly: params.userVisibleOnly, keyLength: params.applicationServerKey.length }); window.testPush.subscribed = true; return subscription; }
      } };
      const serviceWorker = new EventTarget();
      serviceWorker.getRegistration = async () => window.testPush.subscribed || window.testPush.registrations.length ? registration : undefined;
      serviceWorker.register = async (url, options) => { window.testPush.registrations.push({ url, ...options }); return registration; };
      Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: serviceWorker });
      if (options.noPush) { delete window.PushManager; delete window.Notification; }
      else {
        window.PushManager = function () {};
        window.Notification = { get permission() { return window.testPush.permission; }, requestPermission: async () => {
          window.testPush.requests++; window.testPush.permissionGestures.push(navigator.userActivation.isActive);
          window.testPush.permission = options.answer || 'granted'; return window.testPush.permission;
        } };
      }
    }, { options, key: storageKey });
    async function open() {
      await page.goto('https://push.test/tgw/webui/');
      await page.evaluate(() => {
        window.notifications = window.CodexNotifications.create({ expired: () => { window.testPush.expired++; window.notifications.setAuthenticated(false); } });
        window.notifications.setAuthenticated(true);
      });
      await page.waitForFunction(() => !document.querySelector('#toggle-notifications').textContent.includes('Updating'));
    }
    await open();
    return { page, state, errors, open, context };
  }
  try {
    const first = await setup(), { page, state } = first;
    assert.equal(await page.locator('#toggle-notifications').textContent(), 'Enable notifications');
    assert.equal(await page.evaluate(() => window.testPush.requests), 0, 'Page load never prompts for permission');
    assert.equal(await page.evaluate(() => window.testPush.registrations.length), 0, 'Page load does not install or subscribe a new worker');
    await page.locator('#toggle-notifications').click();
    await page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    assert.deepEqual(await page.evaluate(() => window.testPush.permissionGestures), [true], 'Permission is requested within the explicit click gesture');
    assert.deepEqual(await page.evaluate(() => window.testPush.registrations), [{ url: '/tgw/webui/sw.js', scope: '/tgw/webui/', updateViaCache: 'none' }]);
    assert.deepEqual(await page.evaluate(() => window.testPush.subscriptions), [{ userVisibleOnly: true, keyLength: 65 }]);
    assert.match(await page.locator('#notifications-status').textContent(), /All sessions.*closed/);
    assert.deepEqual(await page.evaluate(() => ({ ...localStorage })), { [storageKey]: subscriptionID }, 'Only the opaque device registration ID is persisted');
    assert.equal(state.calls.find(call => call.path.endsWith('/subscribe')).csrf, 'push-csrf');
    assert.equal(state.calls.find(call => call.path.endsWith('/subscribe')).body.subscription.endpoint, 'https://push.example.test/device');
    const subscribedCalls = () => state.calls.filter(call => call.path.endsWith('/subscribe')).length;
    const beforeVisibility = subscribedCalls();
    await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    assert.equal(subscribedCalls(), beforeVisibility, 'Visibility refresh does not repeatedly write the subscription');
    await first.open();
    // Reload starts another authenticated lifecycle. The persisted opt-in is
    // rebound to this login, without asking for permission or subscribing again.
    await page.evaluate(() => { window.testPush.permission = 'granted'; window.notifications.setAuthenticated(false); window.notifications.setAuthenticated(true); });
    await page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    assert.equal(await page.evaluate(() => window.testPush.requests), 0);
    assert.equal(await page.evaluate(() => window.testPush.subscriptions.length), 0);
    assert.ok(subscribedCalls() > beforeVisibility, 'An existing opted-in device is rebound to the new authenticated login');
    state.failUnsubscribe = true;
    await page.locator('#toggle-notifications').click();
    await page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Finish disabling notifications');
    assert.equal(await page.evaluate(() => window.testPush.subscribed), false, 'A failed gateway cleanup still stops browser delivery');
    assert.equal(await page.evaluate(key => localStorage.getItem(key), storageKey), subscriptionID, 'The ID is retained only to retry gateway cleanup');
    state.failUnsubscribe = false;
    await page.locator('#toggle-notifications').click();
    await page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Enable notifications');
    assert.equal(await page.evaluate(key => localStorage.getItem(key), storageKey), null);
    assert.equal(state.calls.filter(call => call.path.endsWith('/unsubscribe')).at(-1).body.subscription_id, subscriptionID);
    assert.deepEqual(first.errors, []);

    const renewed = await setup({ permission: 'granted' });
    await renewed.page.locator('#toggle-notifications').click();
    await renewed.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    renewed.state.nextSubscriptionID = '11111111-2222-4333-8444-666666666666';
    await renewed.page.evaluate(() => { window.notifications.setAuthenticated(false); window.notifications.setAuthenticated(true); });
    await renewed.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    assert.equal(await renewed.page.evaluate(key => localStorage.getItem(key), storageKey), renewed.state.nextSubscriptionID, 'A renewed browser endpoint keeps its new server registration handle');
    assert.equal(renewed.state.calls.filter(call => call.path.endsWith('/unsubscribe')).at(-1).body.subscription_id, subscriptionID, 'The obsolete endpoint registration is removed after renewal');
    assert.equal(await renewed.page.evaluate(() => window.testPush.unsubscribed), 0, 'Renewal does not revoke the active browser subscription');
    assert.equal(renewed.state.enabled, true);
    await renewed.page.locator('#toggle-notifications').click();
    await renewed.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Enable notifications');
    assert.equal(renewed.state.calls.filter(call => call.path.endsWith('/unsubscribe')).at(-1).body.subscription_id, renewed.state.nextSubscriptionID);

    const denied = await setup({ answer: 'denied' });
    await denied.page.locator('#toggle-notifications').click();
    await denied.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Notifications blocked');
    assert.equal(await denied.page.locator('#toggle-notifications').isDisabled(), true);
    assert.equal(denied.state.calls.some(call => call.method === 'POST'), false);
    assert.equal(await denied.page.evaluate(() => window.testPush.registrations.length), 0);
    const iphone = await setup({ iphone: true, noPush: true });
    assert.match(await iphone.page.locator('#notifications-status').textContent(), /Add to Home Screen.*installed app/);
    assert.equal(await iphone.page.locator('#toggle-notifications').isDisabled(), true);
    assert.equal(iphone.state.calls.length, 0, 'The Home Screen hint remains available when Safari omits Push APIs');
    const installed = await setup({ iphone: true, standalone: true });
    await installed.page.locator('#toggle-notifications').click();
    await installed.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Disable notifications');
    assert.deepEqual(await installed.page.evaluate(() => window.testPush.permissionGestures), [true]);

    const failure = await setup(); failure.state.failSubscribe = true;
    await failure.page.locator('#toggle-notifications').click();
    await failure.page.waitForFunction(() => document.querySelector('#notifications-status').textContent.includes('503'));
    assert.equal(await failure.page.evaluate(() => window.testPush.subscribed), false, 'An unconfirmed enable cannot leave a hidden active browser subscription');
    assert.equal(await failure.page.evaluate(key => localStorage.getItem(key), storageKey), null);
    assert.equal(failure.state.calls.filter(call => call.method === 'POST').length, 1, 'Failed subscription writes are not automatically replayed');
    const stale = await setup(); stale.state.holdSubscribe = true;
    await stale.page.locator('#toggle-notifications').click();
    while (!stale.state.held) await new Promise(resolve => setTimeout(resolve, 5));
    await stale.page.evaluate(() => window.notifications.setAuthenticated(false));
    await stale.state.held.fulfill({ status: 401, body: 'Expired old request' });
    await stale.page.waitForTimeout(20);
    assert.equal(await stale.page.evaluate(() => window.testPush.expired), 0, 'An old failed subscription response cannot expire a newer authentication view');
    assert.equal(await stale.page.locator('#toggle-notifications').isDisabled(), true);
    assert.equal(await stale.page.evaluate(key => localStorage.getItem(key), storageKey), null);
    const revoked = await setup({ permission: 'granted' });
    await revoked.page.evaluate(({ key, id }) => { localStorage.setItem(key, id); window.testPush.subscribed = true; document.dispatchEvent(new Event('visibilitychange')); }, { key: storageKey, id: subscriptionID });
    await revoked.page.waitForFunction(() => document.querySelector('#toggle-notifications').textContent === 'Enable notifications');
    assert.equal(revoked.state.calls.filter(call => call.method === 'POST').length, 0, 'A disabled gateway registration is never silently re-enabled');
    const unsupported = await setup({ supported: false });
    assert.equal(await unsupported.page.locator('#toggle-notifications').isDisabled(), true);
    for (const instance of [renewed, denied, iphone, installed, failure, stale, revoked, unsupported]) assert.deepEqual(instance.errors, []);
    console.log('Web Push checks passed: gesture-only permission, iPhone Home Screen guidance, scoped worker, all-session opt-in, opaque-only persistence, auth rebinding without re-enabling revoked records, disable recovery, stale replies, no foreground duplicates or transcript caching, safe click routing.');
  } finally { for (const context of pages) await context.close(); await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

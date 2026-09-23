'use strict';
(() => {
  const scope = '/tgw/webui/', api = '/tgw/api/v1/webui/push';
  const key = 'codex-webui-push-subscription';
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  window.CodexNotifications = { create({ expired }) {
    const button = document.getElementById('toggle-notifications');
    const status = document.getElementById('notifications-status');
    let authenticated = false, generation = 0, busy = false, config = null, boundGeneration = -1;
    let registration = null, subscription = null, savedID = '', message = '';
    const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
    const appleMobile = /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
    const available = () => isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
    function readID() {
      try { const value = localStorage.getItem(key) || ''; return uuid.test(value) ? value : ''; } catch (_) { return ''; }
    }
    function saveID(value) {
      savedID = value;
      try { if (value) localStorage.setItem(key, value); else localStorage.removeItem(key); } catch (_) { /* Current page can still disable its subscription. */ }
    }
    function render() {
      button.disabled = busy || !authenticated;
      button.setAttribute('aria-pressed', String(!!(config?.subscribed && subscription)));
      if (!authenticated) { button.textContent = 'Notifications'; status.textContent = 'Sign in to manage notifications.'; }
      else if (appleMobile && !standalone()) {
        button.textContent = 'Enable notifications'; button.disabled = true;
        status.textContent = 'On iPhone or iPad: use Share → Add to Home Screen, open the installed app, then enable notifications.';
      } else if (!available()) {
        button.textContent = 'Notifications unavailable'; button.disabled = true;
        status.textContent = 'Use an HTTPS browser with Web Push support.';
      } else if (busy) { button.textContent = 'Updating notifications…'; status.textContent = message || 'Checking this device…'; }
      else if (!config) { button.textContent = 'Retry notifications setup'; status.textContent = message || 'Could not check notification settings.'; }
      else if (!config.supported) { button.textContent = 'Notifications unavailable'; button.disabled = true; status.textContent = 'Notifications are not configured on this gateway.'; }
      else if (config.subscribed && savedID) {
        button.textContent = subscription ? 'Disable notifications' : 'Finish disabling notifications';
        status.textContent = message || (subscription ? 'On · All sessions, even when this app is closed.' : 'This device is no longer subscribed. Remove its gateway registration.');
      } else if (Notification.permission === 'denied') {
        button.textContent = 'Notifications blocked'; button.disabled = true;
        status.textContent = 'Allow notifications for this app in your browser or device settings, then reopen it.';
      } else { button.textContent = 'Enable notifications'; status.textContent = message || 'Notify this device when any session finishes a turn.'; }
    }
    async function request(path, method, body, ticket) {
      const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
      const response = await fetch(api + path, { method, credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(15000),
        ...(body ? { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(body) } : {}) });
      if (!authenticated || ticket !== generation) throw new Error('Notification settings changed.');
      if (response.status === 401) { expired(); throw new Error('Sign in to manage notifications.'); }
      if (!response.ok) throw new Error('Could not update notifications (' + response.status + '). Try again.');
      const result = response.status === 204 ? null : await response.json();
      if (!authenticated || ticket !== generation) throw new Error('Notification settings changed.');
      return result;
    }
    async function refresh() {
      if (!authenticated || busy || (appleMobile && !standalone()) || !available()) { render(); return; }
      const ticket = generation; busy = true; message = ''; render();
      try {
        savedID = readID() || savedID;
        const currentConfig = await request('/config' + (savedID ? '?subscription_id=' + encodeURIComponent(savedID) : ''), 'GET', null, ticket);
        if (typeof currentConfig?.supported !== 'boolean' || (currentConfig.supported && (typeof currentConfig.public_key !== 'string' || currentConfig.scope !== 'all'))) throw new Error('Unexpected notification settings.');
        let currentRegistration = await navigator.serviceWorker.getRegistration(scope);
        if (currentRegistration && new URL(currentRegistration.scope).pathname !== scope) currentRegistration = null;
        const currentSubscription = currentRegistration ? await currentRegistration.pushManager.getSubscription() : null;
        if (ticket !== generation || !authenticated) return;
        if (currentConfig.subscribed && savedID && currentSubscription && Notification.permission === 'granted' && boundGeneration !== ticket) {
          // Keep an existing opt-in attached to this login for explicit logout.
          // A removed/disabled registration is never silently enabled again.
          const previousID = savedID;
          const rebound = await request('/subscribe', 'POST', { subscription: currentSubscription.toJSON() }, ticket);
          if (!uuid.test(rebound?.subscription_id || '') || rebound.subscribed !== true || rebound.scope !== 'all') throw new Error('Could not confirm the existing notification registration.');
          saveID(rebound.subscription_id);
          boundGeneration = ticket;
          if (previousID !== savedID) {
            // Browsers may renew an opted-in endpoint. Retain the new device
            // handle even if removal of the obsolete endpoint needs retry.
            try { await request('/unsubscribe', 'POST', { subscription_id: previousID }, ticket); } catch (_) { /* Expired endpoints are also removed by delivery failures. */ }
            if (ticket !== generation || !authenticated) return;
          }
        }
        config = currentConfig; registration = currentRegistration; subscription = currentSubscription;
      } catch (error) { if (ticket === generation && authenticated) { config = null; message = error.message; } }
      finally { if (ticket === generation) { busy = false; render(); } }
    }
    function serverKey(value) {
      const decoded = atob(value.replace(/-/g, '+').replace(/_/g, '/'));
      const bytes = Uint8Array.from(decoded, character => character.charCodeAt(0));
      if (bytes.length !== 65 || bytes[0] !== 4) throw new Error('The gateway notification key is invalid.');
      return bytes;
    }
    async function readyWorker() {
      const worker = await navigator.serviceWorker.register(scope + 'sw.js', { scope, updateViaCache: 'none' });
      if (worker.active) return worker;
      await new Promise((resolve, reject) => {
        const installing = worker.installing || worker.waiting;
        if (!installing) { reject(new Error('Notification setup did not finish. Try again.')); return; }
        const timer = setTimeout(() => finish(new Error('Notification setup timed out. Try again.')), 15000);
        function finish(error) { clearTimeout(timer); installing.removeEventListener('statechange', changed); error ? reject(error) : resolve(); }
        function changed() { if (installing.state === 'activated') finish(); else if (installing.state === 'redundant') finish(new Error('Notification setup failed. Try again.')); }
        installing.addEventListener('statechange', changed); changed();
      });
      return worker;
    }
    async function enable() {
      if (busy || !authenticated || !config?.supported) return;
      const ticket = generation;
      // This call is made directly in the click handler, before any network
      // await, preserving the user gesture required by iOS and other browsers.
      let permission;
      try { permission = Notification.permission === 'granted' ? Promise.resolve('granted') : Notification.requestPermission(); }
      catch (error) { message = error.message || 'Could not ask for notification permission.'; render(); return; }
      busy = true; message = ''; render();
      let browserSubscription = null;
      try {
        const result = await permission;
        if (ticket !== generation || !authenticated) return;
        if (result !== 'granted') { message = result === 'denied' ? 'Notifications were blocked.' : 'Notifications stay off until permission is allowed.'; return; }
        const applicationServerKey = serverKey(config.public_key);
        const currentRegistration = await readyWorker();
        if (ticket !== generation || !authenticated) return;
        registration = currentRegistration;
        browserSubscription = await registration.pushManager.getSubscription();
        if (ticket !== generation || !authenticated) return;
        if (browserSubscription?.options?.applicationServerKey && String(new Uint8Array(browserSubscription.options.applicationServerKey)) !== String(applicationServerKey)) {
          await browserSubscription.unsubscribe(); browserSubscription = null;
          if (ticket !== generation || !authenticated) return;
        }
        browserSubscription ||= await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey });
        if (ticket !== generation || !authenticated) return;
        const response = await request('/subscribe', 'POST', { subscription: browserSubscription.toJSON() }, ticket);
        if (!uuid.test(response?.subscription_id || '') || response.scope !== 'all' || response.subscribed !== true) throw new Error('The gateway did not confirm this subscription. Try again.');
        saveID(response.subscription_id); subscription = browserSubscription;
        config.subscribed = true; boundGeneration = ticket; message = '';
      } catch (error) {
        if (ticket === generation && authenticated) {
          // A lost response must not leave this device receiving notifications
          // while the UI claims enabling failed. A later explicit retry is safe.
          if (browserSubscription) { try { await browserSubscription.unsubscribe(); } catch (_) { /* Gateway cleanup is retried on the next explicit setup. */ } }
          if (ticket !== generation || !authenticated) return;
          subscription = null; message = error.message || 'Could not enable notifications. Try again.';
        }
      } finally { if (ticket === generation) { busy = false; render(); } }
    }
    async function disable() {
      if (busy || !authenticated || !savedID) return;
      const ticket = generation, targetID = savedID; busy = true; message = ''; render();
      try {
        if (subscription) {
          await subscription.unsubscribe();
          if (ticket !== generation || !authenticated) return;
          subscription = null;
        }
        await request('/unsubscribe', 'POST', { subscription_id: targetID }, ticket);
        saveID(''); config.subscribed = false; message = 'Notifications are off on this device.';
      } catch (error) { if (ticket === generation && authenticated) message = subscription ? error.message : 'Notifications stopped on this device. Gateway cleanup failed; retry to finish.'; }
      finally { if (ticket === generation) { busy = false; render(); } }
    }
    button.addEventListener('click', () => {
      if (busy || !authenticated) return;
      if (!config) refresh();
      else if (config.subscribed && savedID) disable();
      else if (available() && (!appleMobile || standalone()) && Notification.permission !== 'denied') enable();
    });
    window.addEventListener('storage', event => {
      if (event.key === key) { generation++; busy = false; config = null; savedID = readID(); refresh(); }
    });
    document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
    render();
    return { setAuthenticated(value) {
      if (value === authenticated) return;
      authenticated = value; generation++; busy = false;
      if (value) refresh(); else { config = null; registration = null; subscription = null; message = ''; render(); }
    } };
  } };
})();

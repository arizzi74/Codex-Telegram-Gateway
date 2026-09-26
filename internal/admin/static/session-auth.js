'use strict';
// Shared passkey login lifecycle. Only the server establishes authentication;
// cross-tab messages contain no credentials and only request a fresh check.
(() => {
  const api = '/tgw/api/v1/admin';
  const channelName = 'codex-gateway-auth';
  function cookie(name) { return document.cookie.split('; ').find(value => value.startsWith(name + '='))?.slice(name.length + 1) || ''; }
  function b64(value) { return btoa(String.fromCharCode(...new Uint8Array(value))).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', ''); }
  function unb64(value) { const text = value.replaceAll('-', '+').replaceAll('_', '/'); return Uint8Array.from(atob(text + '='.repeat((4 - text.length % 4) % 4)), char => char.charCodeAt(0)); }
  function publicOptions(options) {
    const value = structuredClone(options.publicKey);
    value.challenge = unb64(value.challenge);
    if (value.user?.id) value.user.id = unb64(value.user.id);
    for (const key of ['allowCredentials', 'excludeCredentials']) for (const credential of value[key] || []) credential.id = unb64(credential.id);
    return value;
  }
  function serialize(credential) {
    const response = credential.response;
    const value = { id: credential.id, rawId: b64(credential.rawId), type: credential.type, response: { clientDataJSON: b64(response.clientDataJSON) } };
    if (response.attestationObject) {
      value.response.attestationObject = b64(response.attestationObject);
      if (response.getTransports) value.response.transports = response.getTransports();
    } else {
      value.response.authenticatorData = b64(response.authenticatorData);
      value.response.signature = b64(response.signature);
      if (response.userHandle) value.response.userHandle = b64(response.userHandle);
    }
    if (credential.getClientExtensionResults) value.clientExtensionResults = credential.getClientExtensionResults();
    return value;
  }
  function create(options = {}) {
    let session = null, checkedAt = 0, checkedWall = 0, remainingAtCheck = 0, serial = 0, timer = null, loginPromise = null, destroyed = false, channel = null;
    const banner = document.createElement('section'); banner.className = 'session-auth-banner'; banner.hidden = true; banner.setAttribute('aria-label', 'Browser sign-in');
    const text = document.createElement('span'); text.className = 'session-auth-text'; text.setAttribute('role', 'status');
    const button = document.createElement('button'); button.type = 'button'; button.textContent = 'Continue with passkey';
    const status = document.createElement('small'); status.className = 'session-auth-status'; status.setAttribute('role', 'status'); status.hidden = true;
    banner.append(text, button, status);
    (options.mount || document.body).prepend(banner);
    function message(value = '') { status.textContent = value; status.hidden = !value; options.onStatus?.(value); }
    function remaining() { return remainingAtCheck - Math.max(performance.now() - checkedAt, Date.now() - checkedWall); }
    function schedule() {
      clearTimeout(timer); timer = null;
      if (!session || destroyed) { banner.hidden = true; options.onWarning?.(null); return; }
      const ms = remaining();
      if (ms <= 0) { expire('expired'); verify('expiry-check').catch(() => {}); return; }
      const warning = ms <= 300000;
      banner.hidden = !warning;
      if (warning) text.textContent = 'Sign-in expires in ' + Math.max(1, Math.ceil(ms / 60000)) + ' min. Your running work will continue.';
      options.onWarning?.(warning ? { remainingMS: ms, expiresAt: session.expires_at } : null);
      timer = setTimeout(schedule, warning ? Math.min(ms, 30000) : ms - 300000);
    }
    function expire(reason = 'expired') {
      serial++; const hadSession = session !== null; session = null; clearTimeout(timer); timer = null;
      banner.hidden = true; message(); options.onWarning?.(null);
      // Repeated transport failures can report the same expired cookie.
      if (hadSession || !options.isLocked?.()) options.onExpired?.(reason);
    }
    async function verify(reason = 'check') {
      if (destroyed) return null;
      const ticket = ++serial;
      const startedAt = performance.now(), startedWall = Date.now();
      const response = await fetch(api + '/session', { credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10000) });
      if (ticket !== serial || destroyed) return session;
      if (response.status === 401) { expire(reason); return null; }
      if (!response.ok) throw new Error('Could not check sign-in. Check your connection and try again.');
      const value = await response.json();
      if (ticket !== serial || destroyed) return session;
      const expiry = Date.parse(value.expires_at), server = Date.parse(value.server_time);
      if (value.authenticated !== true || typeof value.session_id !== 'string' || !value.session_id || !Number.isFinite(expiry) || !Number.isFinite(server)) throw new Error('Could not verify browser sign-in. Reload and try again.');
      const previous = session;
      session = value; remainingAtCheck = expiry - server; checkedAt = startedAt; checkedWall = startedWall;
      if (remaining() <= 0) { expire('expired'); return null; }
      if (reason !== 'renewal-recovery' || !previous || previous.session_id !== value.session_id) message();
      schedule();
      if (!previous || previous.session_id !== value.session_id || reason === 'login') {
        await options.onAuthenticated?.(value, { reason, rotated: !!previous && previous.session_id !== value.session_id });
      }
      options.onVerified?.(value, { reason });
      return value;
    }
    function broadcast(kind = 'renewed') {
      const value = { kind, nonce: crypto.randomUUID() };
      if (channel) channel.postMessage(value);
      else { try { localStorage.setItem(channelName, JSON.stringify(value)); localStorage.removeItem(channelName); } catch (_) { /* Optional coordination only. */ } }
    }
    async function post(path, body) {
      const response = await fetch(api + path, { method: 'POST', credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(20000), headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': cookie('__Host-telegramgw-csrf') }, body: JSON.stringify(body) });
      if (!response.ok) throw new Error('Passkey sign-in failed. Please try again.');
      return response.json();
    }
    function login() {
      if (loginPromise) return loginPromise;
      loginPromise = (async () => {
        button.disabled = true; options.onRenewing?.(true); message('Waiting for your passkey…');
        try {
          const begin = await post('/login/begin', {});
          const credential = await navigator.credentials.get({ publicKey: publicOptions(begin) });
          if (!credential) throw new DOMException('Cancelled', 'NotAllowedError');
          // Fence work tied to the old cookie before the server revokes it.
          // Old WebSockets may close immediately when login/finish commits.
          serial++;
          await options.onBeforeRotate?.();
          await post('/login/finish', { ceremony_id: begin.ceremony_id, credential: serialize(credential) });
          // Invalidate checks initiated before the cookie rotated. Verify the
          // new cookie before reconnecting transports or rendering any data.
          serial++;
          const result = await verify('login');
          if (!result) throw new Error('Sign-in could not be confirmed. Try again.');
          broadcast('renewed');
          return result;
        } catch (error) {
          message(error.name === 'NotAllowedError' || error.name === 'AbortError' ? 'Passkey cancelled. Tap the button to try again.' : error.message || 'Sign-in failed. Please try again.');
          throw error;
        } finally { button.disabled = false; loginPromise = null; options.onRenewing?.(false); }
      })();
      return loginPromise;
    }
    function foreground() {
      if (!document.hidden) {
        // Clear sensitive content immediately if the local deadline elapsed
        // while mobile timers were suspended, before the network round-trip.
        if (session && remaining() <= 0) expire('expired');
        verify('foreground').catch(error => message(error.message));
      }
    }
    function incoming(value) { if (value && ['renewed', 'logout', 'revoked'].includes(value.kind)) verify(value.kind === 'renewed' ? 'other-tab' : value.kind).catch(error => message(error.message)); }
    function storage(event) { if (event.key === channelName && event.newValue) { try { incoming(JSON.parse(event.newValue)); } catch (_) { /* Ignore malformed hints. */ } } }
    try { channel = new BroadcastChannel(channelName); channel.onmessage = event => incoming(event.data); } catch (_) { window.addEventListener('storage', storage); }
    button.addEventListener('click', () => login().catch(() => {}));
    document.addEventListener('visibilitychange', foreground);
    window.addEventListener('pageshow', foreground);
    window.addEventListener('online', foreground);
    return {
      verify, login, expire, broadcast, snapshot: () => session,
      ensureFresh: async () => { const value = await verify('sensitive-action'); if (value && Date.parse(value.server_time) - Date.parse(value.reauthenticated_at) < 300000) return value; return login(); },
      destroy() { destroyed = true; serial++; clearTimeout(timer); channel?.close(); banner.remove(); document.removeEventListener('visibilitychange', foreground); window.removeEventListener('pageshow', foreground); window.removeEventListener('online', foreground); window.removeEventListener('storage', storage); },
    };
  }
  window.CodexSessionAuth = { create, cookie, publicOptions, serialize };
})();

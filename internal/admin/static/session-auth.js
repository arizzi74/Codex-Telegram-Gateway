'use strict';
// Shared browser login lifecycle. Only the server establishes authentication;
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
  // Passwords exist only in these inputs and the pending request. They never
  // enter browser storage, cross-tab messages, or application status text.
  function passwordDialog(config = {}) {
    return new Promise((resolve, reject) => {
      const previousFocus = document.activeElement;
      const dialog = document.createElement('dialog'); dialog.className = 'session-password-dialog';
      const headingID = 'password-heading-' + crypto.randomUUID();
      dialog.setAttribute('aria-labelledby', headingID);
      const form = document.createElement('form');
      const heading = document.createElement('h2'); heading.id = headingID; heading.textContent = config.title || 'Sign in with password';
      const description = document.createElement('p'); description.id = headingID + '-description'; description.textContent = config.description || 'Enter your gateway username and password.'; dialog.setAttribute('aria-describedby', description.id);
      function field(labelText, name, type, autocomplete) {
        const label = document.createElement('label'); label.textContent = labelText;
        const input = document.createElement('input'); input.name = name; input.type = type; input.autocomplete = autocomplete; input.required = true;
        input.spellcheck = false; input.autocapitalize = 'none'; label.append(input); form.append(label); return input;
      }
      form.append(heading, description);
      const username = field('Username', 'username', 'text', 'username'); username.value = config.username || '';
      const password = field(config.newPassword ? 'New password' : 'Password', 'password', 'password', config.newPassword ? 'new-password' : 'current-password');
      const confirmation = config.newPassword ? field('Confirm new password', 'confirm-password', 'password', 'new-password') : null;
      if (config.newPassword) { const hint = document.createElement('p'); hint.className = 'session-password-hint'; hint.textContent = 'Use 12–256 bytes. Spaces are kept exactly as entered.'; form.append(hint); }
      const error = document.createElement('p'); error.className = 'session-password-error'; error.setAttribute('role', 'alert'); error.hidden = true;
      const actions = document.createElement('div'); actions.className = 'session-password-actions';
      const submit = document.createElement('button'); submit.type = 'submit'; submit.textContent = config.submitLabel || 'Sign in';
      const cancel = document.createElement('button'); cancel.type = 'button'; cancel.textContent = 'Cancel'; cancel.className = 'session-password-cancel';
      actions.append(submit, cancel); form.append(error, actions); dialog.append(form); document.body.append(dialog);
      let busy = false, settled = false;
      function clear() { username.value = ''; password.value = ''; if (confirmation) confirmation.value = ''; }
      function finish(value, failure) {
        if (settled) return; settled = true; clear(); dialog.close(); dialog.remove();
        setTimeout(() => { if (previousFocus?.isConnected && !previousFocus.disabled) previousFocus.focus(); }, 0);
        if (failure) reject(failure); else resolve(value);
      }
      function cancelled() { if (!busy) finish(null, new DOMException('Password sign-in cancelled.', 'NotAllowedError')); }
      cancel.addEventListener('click', cancelled);
      dialog.addEventListener('cancel', event => { event.preventDefault(); cancelled(); });
      dialog.addEventListener('close', () => { if (!settled) finish(null, new DOMException('Password sign-in cancelled.', 'NotAllowedError')); });
      form.addEventListener('submit', async event => {
        event.preventDefault(); if (busy) return;
        const name = username.value.trim(), secret = password.value, confirmationValue = confirmation?.value;
        const encoder = new TextEncoder();
        let problem = '';
        if (!name || encoder.encode(name).length > 64 || /\p{Cc}/u.test(name)) problem = 'Use a username of 1–64 bytes without control characters.';
        else if (encoder.encode(secret).length < 12 || encoder.encode(secret).length > 256) problem = 'Use a password of 12–256 bytes.';
        else if (confirmation && secret !== confirmationValue) problem = 'The passwords do not match.';
        if (problem) { error.textContent = problem; error.hidden = false; return; }
        // Clear every input before submitting, including when a request fails.
        clear(); busy = true; submit.disabled = true; cancel.disabled = true; username.disabled = true; password.disabled = true; if (confirmation) confirmation.disabled = true; error.hidden = true; form.setAttribute('aria-busy', 'true');
        try { finish(await config.onSubmit({ username: name, password: secret })); }
        catch (failure) {
          if (config.closeOnError?.(failure)) { finish(null, failure); return; }
          error.textContent = failure.message || 'The request failed. Please try again.'; error.hidden = false;
        } finally {
          busy = false; submit.disabled = false; cancel.disabled = false; username.disabled = false; password.disabled = false; if (confirmation) confirmation.disabled = false; form.removeAttribute('aria-busy');
          if (!settled) username.focus();
        }
      });
      dialog.showModal(); (username.value ? password : username).focus();
    });
  }
  function create(options = {}) {
    let session = null, checkedAt = 0, checkedWall = 0, remainingAtCheck = 0, serial = 0, timer = null, loginPromise = null, destroyed = false, channel = null;
    let loginOptions = { passkey: true, password: false }, optionsSerial = 0;
    const banner = document.createElement('section'); banner.className = 'session-auth-banner'; banner.hidden = true; banner.setAttribute('aria-label', 'Browser sign-in');
    const text = document.createElement('span'); text.className = 'session-auth-text'; text.setAttribute('role', 'status');
    const button = document.createElement('button'); button.type = 'button'; button.textContent = 'Continue with passkey';
    const passwordButton = document.createElement('button'); passwordButton.type = 'button'; passwordButton.className = 'session-auth-password'; passwordButton.textContent = 'Use password'; passwordButton.hidden = true;
    const status = document.createElement('small'); status.className = 'session-auth-status'; status.setAttribute('role', 'status'); status.hidden = true;
    banner.append(text, button, passwordButton, status);
    (options.mount || document.body).prepend(banner);
    function message(value = '') { status.textContent = value; status.hidden = !value; options.onStatus?.(value); }
    function hasPassword() { return loginOptions.password || session?.authentication_method === 'password'; }
    function updateLoginOptions() { passwordButton.hidden = !hasPassword(); options.onOptions?.({ ...loginOptions, password: hasPassword() }); }
    async function refreshLoginOptions() {
      const ticket = ++optionsSerial;
      try {
        const response = await fetch(api + '/login/options', { credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10000) });
        if (!response.ok || destroyed || ticket !== optionsSerial) return loginOptions;
        const value = await response.json();
        if (typeof value.password !== 'boolean' || destroyed || ticket !== optionsSerial) return loginOptions;
        loginOptions = { passkey: true, password: value.password }; updateLoginOptions();
      } catch (_) { /* Older gateways and optional failures keep passkey access. */ }
      return loginOptions;
    }
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
      updateLoginOptions();
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
      updateLoginOptions();
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
      if (!response.ok) {
        const failure = new Error(response.status === 429 ? 'Too many sign-in attempts. Wait a minute and try again.' : path === '/login/password' ? response.status === 401 ? 'Username or password is incorrect, or password sign-in is disabled.' : 'Password sign-in failed. Please try again.' : 'Passkey sign-in failed. Please try again.');
        failure.status = response.status; throw failure;
      }
      return response.json();
    }
    function login(method = 'passkey') {
      if (loginPromise) return loginPromise;
      loginPromise = (async () => {
        button.disabled = true; passwordButton.disabled = true; options.onRenewing?.(true, method); message(method === 'password' ? 'Enter your gateway password.' : 'Waiting for your passkey…');
        try {
          if (method === 'password') {
            return await passwordDialog({
              // Ambiguous failures can happen after cookie rotation. Close the
              // prompt and let onRenewing recover against the server once.
              closeOnError: error => ![400, 401, 429].includes(error.status),
              onSubmit: async credentials => {
                serial++;
                await options.onBeforeRotate?.();
                await post('/login/password', credentials);
                serial++;
                const result = await verify('login');
                if (!result) throw new Error('Sign-in could not be confirmed. Try again.');
                broadcast('renewed'); return result;
              },
            });
          }
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
          message(error.name === 'NotAllowedError' || error.name === 'AbortError' ? (method === 'password' ? 'Password sign-in cancelled. Choose a sign-in method to try again.' : 'Passkey cancelled. Tap the button to try again.') : error.message || 'Sign-in failed. Please try again.');
          throw error;
        } finally { button.disabled = false; passwordButton.disabled = false; loginPromise = null; options.onRenewing?.(false, method); }
      })();
      return loginPromise;
    }
    function foreground() {
      if (!document.hidden) {
        refreshLoginOptions();
        // Clear sensitive content immediately if the local deadline elapsed
        // while mobile timers were suspended, before the network round-trip.
        if (session && remaining() <= 0) expire('expired');
        verify('foreground').catch(error => message(error.message));
      }
    }
    function incoming(value) { if (value && ['renewed', 'logout', 'revoked'].includes(value.kind)) { refreshLoginOptions(); verify(value.kind === 'renewed' ? 'other-tab' : value.kind).catch(error => message(error.message)); } }
    function storage(event) { if (event.key === channelName && event.newValue) { try { incoming(JSON.parse(event.newValue)); } catch (_) { /* Ignore malformed hints. */ } } }
    try { channel = new BroadcastChannel(channelName); channel.onmessage = event => incoming(event.data); } catch (_) { window.addEventListener('storage', storage); }
    button.addEventListener('click', () => login().catch(() => {}));
    passwordButton.addEventListener('click', () => login('password').catch(() => {}));
    document.addEventListener('visibilitychange', foreground);
    window.addEventListener('pageshow', foreground);
    window.addEventListener('online', foreground);
    refreshLoginOptions();
    function reauthenticate(value = session) { return login(value?.authentication_method === 'password' && hasPassword() ? 'password' : 'passkey'); }
    return {
      verify, login, reauthenticate, expire, broadcast, refreshLoginOptions, hasPassword, snapshot: () => session,
      ensureFresh: async () => { const previous = session; const value = await verify('sensitive-action'); if (value && Date.parse(value.server_time) - Date.parse(value.reauthenticated_at) < 300000) return value; return reauthenticate(value || previous); },
      destroy() { destroyed = true; serial++; clearTimeout(timer); channel?.close(); banner.remove(); document.removeEventListener('visibilitychange', foreground); window.removeEventListener('pageshow', foreground); window.removeEventListener('online', foreground); window.removeEventListener('storage', storage); },
    };
  }
  window.CodexSessionAuth = { create, cookie, publicOptions, serialize, passwordDialog };
})();

'use strict';

const api = '/tgw/api/v1/admin';
const $ = id => document.getElementById(id);
const pageSize = 10;
let csrf = cookie('__Host-telegramgw-csrf');
let authenticated = false;
let loading = false;
let reloadPending = false;
let authEpoch = 0;
let rotationPending = false;
let sessionAuth = null;
let dashboard = null;
let sessionPage = 0;
let passwordSettings = null;
let passwordSettingsBusy = false;
let audioSnapshot = null;
let audioDraft = null;
let audioPending = null;
let audioSaving = false;
let audioSaveSequence = 0;
let audioLoadingEpoch = null;
let audioPollTimer = null;
let audioError = '';
let audioNotice = '';
// Enrollment URLs exist only in this open dialog, never in browser storage.
let workerEnrollment = null;
let enrollmentBusy = false;
let enrollmentSequence = 0;
let enrollmentTimer = null;
// Only the built-in web interface and a single canonical session link are
// allowed after login. Never turn notification links into an open redirect.
const requestedReturn = new URLSearchParams(location.search).get('next') || '';
const returnToWebUI = requestedReturn === '/tgw/webui/' ||
  (requestedReturn.length === '/tgw/webui/?session_id='.length + 36 && /^\/tgw\/webui\/\?session_id=[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(requestedReturn))
  ? requestedReturn : '';

function b64(bytes) {
  return btoa(String.fromCharCode(...new Uint8Array(bytes))).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}
function unb64(s) {
  s = s.replaceAll('-', '+').replaceAll('_', '/');
  return Uint8Array.from(atob(s + '='.repeat((4 - s.length % 4) % 4)), x => x.charCodeAt(0));
}
function cookie(name) {
  return document.cookie.split('; ').find(x => x.startsWith(name + '='))?.slice(name.length + 1) || '';
}
function msg(message) { $('message').textContent = message; }
async function call(path, opts = {}) {
  const headers = { ...(opts.headers || {}), 'Content-Type': 'application/json' };
  if (opts.method && opts.method !== 'GET') headers['X-CSRF-Token'] = cookie('__Host-telegramgw-csrf');
  const response = await fetch(api + path, { credentials: 'same-origin', ...opts, headers });
  if (!response.ok) {
    const error = new Error(response.status === 401 ? 'Sign in required.' : `Request failed (${response.status}). Please try again.`);
    error.status = response.status;
    try { const detail = await response.json(); error.code = detail.code; if (detail.message) error.message = detail.message; } catch (_) { /* Non-JSON errors retain the safe generic message. */ }
    throw error;
  }
  return response.status === 204 ? null : response.json();
}
async function sensitiveCall(path, opts, isCurrent = () => true) {
  await sessionAuth.ensureFresh();
  if (!isCurrent()) throw new Error('Your sign-in changed. Review your settings and try again.');
  try { return await call(path, opts); } catch (error) {
    // Only this explicit server rejection proves that the mutation did not run.
    // Network failures and ambiguous responses must never replay a mutation.
    if (error.status !== 403 || error.code !== 'reauthentication_required') throw error;
    await sessionAuth.reauthenticate();
    if (!isCurrent()) throw new Error('Your sign-in changed. Review your settings and try again.');
    return call(path, opts);
  }
}
function publicOptions(options) {
  const p = options.publicKey;
  for (const key of ['challenge', 'user']) {
    if (p[key]?.id) p[key].id = unb64(p[key].id);
    else if (p[key]) p[key] = unb64(p[key]);
  }
  for (const key of ['excludeCredentials', 'allowCredentials']) {
    if (p[key]) p[key].forEach(value => value.id = unb64(value.id));
  }
  return p;
}
function serialize(credential) {
  const r = credential.response;
  const output = { id: credential.id, rawId: b64(credential.rawId), type: credential.type, response: { clientDataJSON: b64(r.clientDataJSON) } };
  if (r.attestationObject) {
    output.response.attestationObject = b64(r.attestationObject);
    if (r.getTransports) output.response.transports = r.getTransports();
  } else {
    output.response.authenticatorData = b64(r.authenticatorData);
    output.response.signature = b64(r.signature);
    if (r.userHandle) output.response.userHandle = b64(r.userHandle);
  }
  if (credential.getClientExtensionResults) output.clientExtensionResults = credential.getClientExtensionResults();
  return output;
}
async function enroll(bootstrap = '') {
  if (!bootstrap) await sessionAuth.ensureFresh();
  msg('Preparing your passkey…');
  const begin = await (bootstrap ? call : sensitiveCall)('/passkeys/register/begin', { method: 'POST', body: JSON.stringify({ bootstrap_token: bootstrap }) });
  const credential = await navigator.credentials.create({ publicKey: publicOptions(begin) });
  if (!credential) throw new Error('Passkey creation was cancelled.');
  await call('/passkeys/register/finish', { method: 'POST', body: JSON.stringify({ ceremony_id: begin.ceremony_id, bootstrap_token: bootstrap, credential: serialize(credential) }) });
  $('bootstrap-token').value = '';
  msg('Passkey saved. You can now sign in.');
}
async function login() {
  await sessionAuth.login();
}

// Build dynamic content with text nodes; session titles, messages, and names are untrusted.
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}
function number(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value.toLocaleString() : '—';
}
function date(value) {
  if (!value || String(value).startsWith('0001-')) return null;
  const parsed = new Date(value);
  return Number.isFinite(parsed.getTime()) ? parsed : null;
}
function timestamp(value) { return date(value)?.toLocaleString() || '—'; }
function relative(value) {
  const parsed = date(value);
  if (!parsed) return '—';
  const seconds = Math.max(0, Math.floor((Date.now() - parsed.getTime()) / 1000));
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}
function duration(value) {
  const parsed = date(value);
  if (!parsed) return '—';
  const minutes = Math.max(0, Math.floor((Date.now() - parsed.getTime()) / 60000));
  if (minutes < 1) return 'less than a minute';
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
  return `${Math.floor(minutes / 1440)}d ${Math.floor(minutes % 1440 / 60)}h`;
}
function words(value) { return String(value || 'Unknown').replaceAll('_', ' '); }
function badge(value) {
  const good = ['connected', 'running', 'active', 'idle', 'online'].includes(value);
  const bad = ['unavailable', 'disconnected', 'failed', 'error', 'mismatch', 'revoked'].includes(value);
  return el('span', `badge${good ? ' good' : bad ? ' bad' : ''}`, words(value));
}
function fact(list, label, value, options = {}) {
  const item = el('div', options.wide ? 'wide' : '');
  item.append(el('dt', '', label), el('dd', options.mono ? 'mono' : '', value === undefined || value === null || value === '' ? '—' : value));
  list.append(item);
}
function button(label, className, action) {
  const control = el('button', className, label);
  control.type = 'button';
  control.addEventListener('click', async () => {
    control.disabled = true;
    try { await perform(action); } finally { control.disabled = false; }
  });
  return control;
}
function showError(error) {
  if (error.status === 401) {
    sessionAuth.expire('expired');
    return;
  }
  $('dashboard-error').textContent = error.message || 'Unable to refresh. Please try again.';
  $('dashboard-error').hidden = false;
}
function lockConsole(reason) {
  authEpoch++;
  authenticated = false;
  dashboard = null;
  reloadPending = false;
  closeWorkerEnrollment();
  passwordSettings = null;
  renderPasswordSettings();
  clearTimeout(audioPollTimer);
  audioSnapshot = null;
  audioDraft = null;
  audioPending = null;
  audioSaveSequence++;
  audioSaving = false;
  audioError = '';
  audioNotice = '';
  renderAudio();
  $('console').hidden = true;
  $('logout').hidden = true;
  $('auth').hidden = false;
  for (const id of ['session-list', 'bot-info', 'worker-list', 'keys', 'browser-sessions']) $(id).replaceChildren();
  const instruction = sessionAuth.hasPassword() ? 'Choose a sign-in method to continue.' : 'Continue with your passkey.';
  msg(reason === 'expired' ? 'Your session expired. ' + instruction : reason?.startsWith('Signed out') ? reason : instruction);
}
function signedOut(message) {
  // The same tab may have navigated here from the web UI. Its optional draft
  // key is not an authentication token, but explicit sign-out should discard it.
  try { sessionStorage.removeItem('codex-webui-encrypted-text-draft-v1'); } catch (_) { /* Storage may be disabled. */ }
  sessionAuth.broadcast('logout');
  sessionAuth.expire(message);
}
async function perform(action) {
  try { await action(); } catch (error) { showError(error); }
}

function renderBot(bot) {
  const root = $('bot-info');
  root.replaceChildren();
  if (!bot) {
    root.append(el('p', 'muted', 'Bot information is not available.'));
    return;
  }
  const heading = el('div', 'bot-title');
  heading.append(el('strong', '', bot.display_name || 'Telegram bot'));
  if (bot.username) {
    if (/^[A-Za-z0-9_]{5,32}$/.test(bot.username)) {
      const link = el('a', '', '@' + bot.username);
      link.href = 'https://t.me/' + bot.username;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      heading.append(link);
    } else heading.append(el('span', '', '@' + bot.username));
  }
  heading.append(badge(bot.status));
  root.append(heading);
  const facts = el('dl', 'facts');
  fact(facts, 'Webhook', words(bot.webhook_status));
  fact(facts, 'Pending Telegram updates', number(bot.pending_updates));
  fact(facts, 'Allowed users', number(bot.allowed_user_count));
  fact(facts, 'Allowed chats', bot.allowed_chat_count === 0 ? 'Any chat with an allowed user' : number(bot.allowed_chat_count));
  fact(facts, 'Bot ID', bot.id || '—', { mono: true });
  fact(facts, 'Checked', timestamp(bot.checked_at));
  if (bot.webhook_url) fact(facts, 'Webhook address', bot.webhook_url, { wide: true, mono: true });
  root.append(facts);
  if (bot.last_error) {
    const warning = el('p', 'notice error', bot.last_error);
    if (date(bot.last_error_at)) warning.append(el('span', '', ' · ' + timestamp(bot.last_error_at)));
    root.append(warning);
  }
}
function visibleSessions() { return (dashboard?.sessions || []).filter(session => !session.archived); }
function sessionName(session) { return session.name || session.preview || 'Untitled session'; }
function renderSessionFilters() {
  const select = $('session-state');
  const selected = select.value;
  const all = el('option', '', 'All states');
  all.value = '';
  select.replaceChildren(all);
  for (const state of [...new Set(visibleSessions().map(session => session.state).filter(Boolean))].sort()) {
    const option = el('option', '', words(state));
    option.value = state;
    select.append(option);
  }
  select.value = [...select.options].some(option => option.value === selected) ? selected : '';
}
function contextUsage(stats) {
  if (typeof stats.context_tokens !== 'number') return '—';
  if (typeof stats.context_window !== 'number' || stats.context_window <= 0) return number(stats.context_tokens);
  return `${number(stats.context_tokens)} / ${number(stats.context_window)} (${Math.round(stats.context_tokens / stats.context_window * 100)}%)`;
}
function messageCount(value, stats) {
  const count = number(value);
  return stats.history_complete === false && count !== '—' ? '≥' + count : count;
}
function renderSession(session, expanded) {
  const stats = session.stats || {};
  const card = el('article', 'session-card');
  card.dataset.sessionId = session.session_id;
  const heading = el('div', 'session-title');
  heading.append(el('h3', '', sessionName(session)));
  const badges = el('div', 'badges');
  badges.append(badge(session.state));
  if (session.pending_approvals > 0) badges.append(el('span', 'badge warning', `${number(session.pending_approvals)} awaiting approval`));
  if (session.queued_commands > 0) badges.append(el('span', 'badge', `${number(session.queued_commands)} queued`));
  heading.append(badges);
  card.append(heading);
  card.append(el('p', 'session-location muted', [session.worker_name, session.runtime_name].filter(Boolean).join(' · ') || 'Worker information unavailable'));
  card.append(el('p', 'session-path', session.cwd || 'Directory unavailable'));
  const timing = el('div', 'session-timing');
  if (date(stats.active_since)) {
    const active = el('p', '', 'Active for ' + duration(stats.active_since));
    active.title = 'Active since ' + timestamp(stats.active_since);
    timing.append(active);
  }
  const last = el('p', '', 'Last message ' + relative(stats.last_message_at));
  last.title = timestamp(stats.last_message_at);
  timing.append(last);
  if (stats.model) timing.append(el('p', '', [stats.model, stats.reasoning_effort].filter(Boolean).join(' · ')));
  card.append(timing);
  const metrics = el('dl', 'session-metrics');
  fact(metrics, 'Prompts', messageCount(stats.prompt_count, stats));
  fact(metrics, 'Assistant replies', messageCount(stats.assistant_message_count, stats));
  fact(metrics, 'Total tokens', number(stats.total_tokens));
  fact(metrics, 'Context tokens', number(stats.context_tokens));
  card.append(metrics);
  if (stats.history_complete === false) card.append(el('p', 'token-note', 'Partial history · message counts may still be loading.'));
  const lastMessage = el('div', 'last-message');
  lastMessage.append(el('p', 'last-message-label', 'Last message' + (stats.last_message_role ? ' · ' + words(stats.last_message_role) : '')));
  lastMessage.append(el('p', 'last-message-text', stats.last_message || 'Not available'));
  card.append(lastMessage);
  const details = el('details');
  details.open = expanded;
  details.append(el('summary', '', 'Session details and token breakdown'));
  const facts = el('dl', 'facts');
  fact(facts, 'Session created', timestamp(stats.created_at));
  fact(facts, 'Active since', timestamp(stats.active_since));
  fact(facts, 'Last message', timestamp(stats.last_message_at));
  fact(facts, 'Last session event', timestamp(session.last_event_at));
  fact(facts, 'Input tokens', number(stats.input_tokens));
  fact(facts, 'Cached input tokens', number(stats.cached_input_tokens));
  fact(facts, 'Output tokens', number(stats.output_tokens));
  fact(facts, 'Reasoning output tokens', number(stats.reasoning_output_tokens));
  fact(facts, 'Context usage', contextUsage(stats), { wide: true });
  fact(facts, 'Model', stats.model);
  fact(facts, 'Reasoning effort', stats.reasoning_effort);
  fact(facts, 'Git branch', session.git_branch);
  fact(facts, 'Worker connection', words(session.worker_connectivity));
  fact(facts, 'Codex version', session.codex_version);
  fact(facts, 'Runtime generation', number(session.runtime_generation));
  fact(facts, 'Loaded in runtime', session.loaded ? 'Yes' : 'No');
  fact(facts, 'Pending approvals', number(session.pending_approvals));
  fact(facts, 'Queued commands', number(session.queued_commands));
  fact(facts, 'Statistics checked', timestamp(stats.observed_at));
  fact(facts, 'Worker last seen', timestamp(session.last_seen));
  fact(facts, 'First seen by gateway', timestamp(session.discovered_at));
  fact(facts, 'Session ID', session.session_id, { wide: true, mono: true });
  fact(facts, 'Codex thread ID', session.codex_thread_id, { wide: true, mono: true });
  fact(facts, 'Runtime ID', session.runtime_id, { wide: true, mono: true });
  fact(facts, 'Worker ID', session.worker_id, { wide: true, mono: true });
  if (session.active_turn_id) fact(facts, 'Active turn ID', session.active_turn_id, { wide: true, mono: true });
  details.append(facts);
  if (stats.history_complete === false) details.append(el('p', 'token-note', 'Message counts marked ≥ are lower bounds from the available history.'));
  details.append(el('p', 'token-note', 'Token totals show the latest recorded cumulative usage. Cached tokens are included in input tokens; reasoning tokens are included in output tokens. Unavailable statistics appear as —.'));
  card.append(details);
  return card;
}
function renderSessions() {
  const all = visibleSessions();
  const query = $('session-search').value.trim().toLocaleLowerCase();
  const state = $('session-state').value;
  const filtered = all.filter(session => (!state || session.state === state) && (!query || [sessionName(session), session.cwd, session.worker_name, session.runtime_name, session.git_branch, session.session_id].some(value => String(value || '').toLocaleLowerCase().includes(query))));
  filtered.sort((a, b) => (date(b.stats?.last_message_at)?.getTime() || date(b.stats?.created_at)?.getTime() || 0) - (date(a.stats?.last_message_at)?.getTime() || date(a.stats?.created_at)?.getTime() || 0));
  const pages = Math.max(1, Math.ceil(filtered.length / pageSize));
  sessionPage = Math.max(0, Math.min(sessionPage, pages - 1));
  const expanded = new Set([...$('session-list').querySelectorAll('.session-card')].filter(card => card.querySelector('details').open).map(card => card.dataset.sessionId));
  $('session-list').replaceChildren();
  const pageSessions = filtered.slice(sessionPage * pageSize, (sessionPage + 1) * pageSize);
  for (const session of pageSessions) $('session-list').append(renderSession(session, expanded.has(session.session_id)));
  if (!filtered.length) $('session-list').append(el('p', 'empty', all.length ? 'No sessions match these filters.' : 'No user sessions have been reported yet.'));
  $('session-count').textContent = filtered.length ? `Showing ${sessionPage * pageSize + 1}–${sessionPage * pageSize + pageSessions.length} of ${number(filtered.length)} sessions${filtered.length !== all.length ? ` (${number(all.length)} total)` : ''}` : `${number(filtered.length)} sessions`;
  $('session-pagination').hidden = pages <= 1;
  $('session-prev').disabled = sessionPage === 0;
  $('session-next').disabled = sessionPage >= pages - 1;
  $('session-page').textContent = `Page ${sessionPage + 1} of ${pages}`;
}
function renderWorkers(workers) {
  $('worker-list').replaceChildren();
  for (const worker of workers) {
    const row = el('div', 'worker');
    const info = el('div');
    info.append(el('b', '', worker.Name), el('div', 'muted', [worker.Connectivity, [worker.OS, worker.Arch].filter(Boolean).join('/'), worker.Version && 'v' + worker.Version].filter(Boolean).join(' · ')), el('div', 'muted', 'Worker ID: ' + worker.ID));
    const actions = el('div', 'worker-actions');
    if (worker.Enabled !== false) {
      actions.append(button('Rotate token', 'secondary', async () => {
        const result = await sensitiveCall('/workers/' + encodeURIComponent(worker.ID) + '/rotate-token', { method: 'POST', body: '{}' });
        showToken(worker.ID, result.token);
        await load(false);
      }));
      actions.append(button('Revoke', 'danger', async () => {
        if (confirm('Revoke this worker?')) {
          await sensitiveCall('/workers/' + encodeURIComponent(worker.ID), { method: 'DELETE', body: '{}' });
          await load(false);
        }
      }));
    } else {
      actions.append(badge('revoked'));
      actions.append(button('Delete', 'danger', async () => {
        if (confirm(`Permanently delete revoked worker "${worker.Name || worker.ID}"?\n\nThis irreversibly removes the worker and all related gateway database records, including sessions, history, and routing. Project files and native Codex sessions are untouched.\n\nThis cannot be undone.`)) {
          await sensitiveCall('/workers/' + encodeURIComponent(worker.ID) + '/purge', { method: 'DELETE', body: JSON.stringify({ confirmed: true }) });
          await load(false);
        }
      }));
    }
    row.append(info, actions);
    $('worker-list').append(row);
  }
  if (!workers.length) $('worker-list').append(el('p', 'muted', 'No enrolled workers.'));
}
function renderKeys(keys) {
  $('keys').replaceChildren();
  for (const key of keys || []) {
    const row = el('div', 'key');
    const info = el('div');
    info.append(el('b', '', key.id.slice(0, 18) + '…'), el('div', 'muted', 'Added ' + timestamp(key.created_at)));
    row.append(info);
    if (!key.revoked_at) row.append(button('Revoke', 'danger', async () => {
      await sensitiveCall('/passkeys/' + encodeURIComponent(key.id), { method: 'DELETE', body: '{}' });
      await load();
    }));
    else row.append(badge('revoked'));
    $('keys').append(row);
  }
  if (!keys?.length) $('keys').append(el('p', 'muted', 'No passkeys available.'));
}
function renderBrowserSessions(sessions) {
  $('browser-sessions').replaceChildren();
  for (const session of sessions || []) {
    const row = el('div', 'key browser-session');
    const info = el('div');
    info.append(el('b', '', (session.browser_label || 'Browser') + (session.current ? ' · This browser' : '')),
      el('div', 'muted', 'Signed in ' + timestamp(session.created_at)),
      el('div', 'muted', 'Last seen ' + timestamp(session.last_seen_at || session.created_at)),
      el('div', 'muted', 'Expires ' + timestamp(session.expires_at)));
    if (['password', 'passkey'].includes(session.authentication_method)) info.append(el('div', 'muted', 'Sign-in method: ' + (session.authentication_method === 'password' ? 'Password' : 'Passkey')));
    row.append(info, button('Sign out', 'danger', async () => {
      if (!confirm(session.current ? 'Sign out of this browser?' : 'End access for this browser session?')) return;
      await call('/sessions/' + encodeURIComponent(session.session_id), { method: 'DELETE', body: '{}' });
      if (session.current) signedOut('Signed out.');
      else await load(false);
    }));
    $('browser-sessions').append(row);
  }
}
function renderPasswordSettings() {
  $('password-edit').hidden = !passwordSettings;
  $('password-disable').hidden = !passwordSettings?.enabled;
  $('password-edit').textContent = passwordSettings?.enabled ? 'Change username or password' : 'Enable password sign-in';
  $('password-edit').disabled = passwordSettingsBusy;
  $('password-disable').disabled = passwordSettingsBusy;
  $('password-settings-status').textContent = passwordSettings
    ? passwordSettings.enabled ? 'Enabled for ' + passwordSettings.username + '. Changing or disabling this password signs out browsers using it.' : 'Password sign-in is disabled.'
    : 'Password settings are unavailable. Refresh to try again.';
}
async function loadPasswordSettings(epoch) {
  try {
    const settings = await call('/password', { cache: 'no-store' });
    if (epoch !== authEpoch) return;
    passwordSettings = typeof settings?.enabled === 'boolean' && typeof settings.username === 'string' ? settings : null;
  } catch (error) {
    if (epoch !== authEpoch) return;
    if (error.status === 401) throw error;
    passwordSettings = null;
  }
  renderPasswordSettings();
}
async function passwordSettingsChanged(message) {
  // Password changes revoke password sessions, including this browser when it
  // signed in that way. Check the cookie before refreshing protected data.
  await sessionAuth.refreshLoginOptions();
  sessionAuth.broadcast('revoked');
  const value = await sessionAuth.verify('password-settings');
  if (!value) { msg(message + ' Sign in again to continue.'); return; }
  await load();
  $('password-settings-status').textContent += ' ' + message;
}
async function editPassword() {
  if (passwordSettingsBusy || !passwordSettings) return;
  const settings = passwordSettings;
  passwordSettingsBusy = true; renderPasswordSettings();
  try {
    await sessionAuth.ensureFresh();
    const changed = await window.CodexSessionAuth.passwordDialog({
      title: settings.enabled ? 'Change password sign-in' : 'Enable password sign-in',
      description: settings.enabled ? 'Enter a username and new password. Browsers signed in with the previous password will be signed out.' : 'Choose a gateway username and password. Your passkeys can still sign you in.',
      username: settings.username, newPassword: true,
      submitLabel: settings.enabled ? 'Save password' : 'Enable password sign-in',
      onSubmit: async credentials => {
        if (!confirm(settings.enabled ? 'Change password sign-in and sign out all browsers using the current password?' : 'Enable username and password sign-in for this gateway?')) return false;
        await sensitiveCall('/password', { method: 'PUT', body: JSON.stringify(credentials), cache: 'no-store' });
        return true;
      },
    });
    if (changed) await passwordSettingsChanged(settings.enabled ? 'Password sign-in updated.' : 'Password sign-in enabled.');
  } catch (error) {
    if (error.name !== 'NotAllowedError' && error.name !== 'AbortError') showError(error);
  } finally { passwordSettingsBusy = false; if (authenticated) renderPasswordSettings(); }
}
async function disablePassword() {
  if (passwordSettingsBusy || !passwordSettings?.enabled) return;
  if (!confirm('Disable password sign-in and sign out all browsers using it? Your passkeys will still work.')) return;
  passwordSettingsBusy = true; renderPasswordSettings();
  try {
    await sensitiveCall('/password', { method: 'DELETE', body: '{}', cache: 'no-store' });
    await passwordSettingsChanged('Password sign-in disabled.');
  } catch (error) {
    if (error.name !== 'NotAllowedError' && error.name !== 'AbortError') showError(error);
  } finally { passwordSettingsBusy = false; if (authenticated) renderPasswordSettings(); }
}
function audioSettings(value) {
  if (typeof value?.enabled !== 'boolean' || !['base', 'small'].includes(value.model) ||
      !Number.isInteger(value.threads) || value.threads < 1 || value.threads > 32 || typeof value.reply_with_audio !== 'boolean' ||
      (value.tts_voice !== undefined && !['auto', 'italian', 'english'].includes(value.tts_voice)) ||
      (value.tts_engine !== undefined && !['', 'piper', 'qwen3'].includes(value.tts_engine))) return null;
  return { enabled: value.enabled, model: value.model, threads: value.threads, reply_with_audio: value.reply_with_audio, tts_voice: value.tts_voice || 'auto', tts_engine: value.tts_engine || '' };
}
function sameAudioSettings(a, b) {
  return !!a && !!b && ['enabled', 'model', 'threads', 'reply_with_audio', 'tts_voice', 'tts_engine'].every(key => a[key] === b[key]);
}
function audioDirty() { return !!audioDraft && !sameAudioSettings(audioDraft, audioSnapshot?.settings); }
function audioBytes(value) {
  if (!Number.isFinite(value) || value < 0) return 'Unavailable';
  return value >= 1024 ** 3 ? (value / 1024 ** 3).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' GiB'
    : (value / 1024 ** 2).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' MiB';
}
function recommendedAudioThreads() {
  const resources = audioSnapshot?.resources;
  const recommendation = resources?.recommended_threads;
  return Number.isInteger(recommendation) ? Math.max(1, Math.min(32, recommendation))
    : Math.max(1, Math.min(32, (resources?.available_cpus || 2) - 1));
}
function renderAudio() {
  const ready = !!audioSnapshot && !!audioDraft;
  const busy = audioSaving || !!audioSnapshot?.busy;
  const dirty = audioDirty();
  const enabled = ready && audioDraft.enabled;
  $('audio-enabled').disabled = !ready || busy;
  $('audio-enabled').checked = !!audioDraft?.enabled;
  $('audio-options').disabled = !enabled || busy;
  for (const model of ['base', 'small']) {
    $('audio-model-' + model).checked = audioDraft?.model === model;
    const info = audioSnapshot?.models?.find(item => item.name === model);
    $('audio-' + model + '-install').textContent = info ? (info.installed ? 'Downloaded' : 'Download required') + ' · ' + audioBytes(info.size_bytes) : '';
  }
  // Status refreshes never replace a partially edited number field.
  if (document.activeElement !== $('audio-threads')) $('audio-threads').value = audioDraft?.threads ?? '';
  const recommended = recommendedAudioThreads();
  $('audio-use-recommended').disabled = !enabled || busy;
  $('audio-threads-help').textContent = ready
    ? `Recommended: ${recommended} ${recommended === 1 ? 'thread' : 'threads'}. Use one fewer available CPU core, with a minimum of 1 and a maximum of 32. You can adjust this.`
    : 'Use one fewer available CPU core, with a minimum of 1 and a maximum of 32 threads.';
  $('audio-reply').checked = !!audioDraft?.reply_with_audio;
  // A gateway whose speech engine became unavailable must still let an
  // administrator turn off an already saved audio-reply preference.
  $('audio-reply').disabled = !enabled || busy || (!audioSnapshot?.tts?.available && !audioDraft?.reply_with_audio);
  $('audio-reply-help').textContent = audioSnapshot?.tts?.available
    ? 'Send the final text reply and an audio version with the same content.'
    : 'Audio replies require the local speech engine. Voice transcription can be used on its own.';
  const replies = !!audioDraft?.reply_with_audio;
  $('audio-engine-wrap').hidden = !replies;
  $('audio-engine-wrap').disabled = !enabled || busy || !audioSnapshot?.tts?.available;
  for (const engine of ['piper', 'qwen3']) {
    const choice = $('audio-engine-' + engine);
    choice.checked = audioDraft?.tts_engine === engine;
    choice.required = enabled && replies;
  }
  const pendingEngine = $('audio-engine-pending');
  pendingEngine.hidden = !replies || !!audioDraft?.tts_engine;
  pendingEngine.textContent = audioSnapshot?.settings.reply_with_audio && !audioSnapshot.settings.tts_engine
    ? 'The existing Pocket TTS engine remains active. Choose an engine and apply settings to switch. Piper / Miro is recommended for this CPU.'
    : 'Choose an audio reply engine before applying settings. Piper / Miro is recommended for this CPU.';
  const voice = $('audio-voice');
  const voices = audioSnapshot?.tts?.voices || [];
  voice.replaceChildren();
  for (const choice of voices) {
    if (!['auto', 'italian', 'english'].includes(choice.id)) continue;
    const option = el('option', '', choice.label || choice.id);
    option.value = choice.id;
    voice.append(option);
  }
  voice.value = audioDraft?.tts_voice || 'auto';
  voice.disabled = !enabled || busy || !audioSnapshot?.tts?.available || !replies;
  $('audio-voice-wrap').hidden = !replies;
  $('audio-voice-help').textContent = audioDraft?.tts_engine === 'piper'
    ? 'Piper / Miro uses an Italian voice for every language. The language preference is saved; English is spoken with an Italian accent.'
    : audioDraft?.tts_engine === 'qwen3'
      ? 'Qwen3-TTS supports Italian and English with the same model download. Automatic matching follows the voice note language.'
      : 'Choose an engine to see its language support. Your language preference is preserved.';
  const resources = $('audio-resources');
  resources.replaceChildren();
  if (ready) {
    fact(resources, 'CPU cores', number(audioSnapshot.resources?.logical_cpus));
    fact(resources, 'Available CPU cores', number(audioSnapshot.resources?.available_cpus));
    fact(resources, 'Total RAM', audioBytes(audioSnapshot.resources?.memory_total_bytes));
    fact(resources, 'Available RAM', audioBytes(audioSnapshot.resources?.memory_available_bytes));
  }
  const operation = audioSnapshot?.operation || {};
  const appliedWarning = operation.state === 'ready' && operation.message && operation.message !== 'Audio settings saved.' ? operation.message : '';
  const states = { idle: 'Audio settings are ready.', queued: 'Audio setup is queued…', checking: 'Checking local audio files…', verifying: 'Verifying the local speech model…', downloading: 'Downloading audio model files…', warming: 'Checking the local speech voice…', preflight: 'Checking the audio helper and model…', applying: 'Applying audio settings…', ready: 'Audio settings applied.', failed: 'Audio setup failed. Your previous settings remain active.' };
  $('audio-status').textContent = audioSaving ? 'Submitting audio settings…' : ready
    ? audioNotice || appliedWarning || (audioSnapshot.busy || operation.state === 'failed' ? states[operation.state] || 'Setting up audio…' : audioSnapshot.settings.enabled ? 'Voice notes are enabled.' : 'Voice notes are disabled.')
    : audioError ? 'Audio settings are unavailable. Refresh to try again.' : 'Loading audio settings…';
  const error = audioError || (operation.state === 'failed' ? operation.message || 'Unable to apply audio settings. Check available disk space and memory, then try again.' : '');
  $('audio-error').textContent = error;
  $('audio-error').hidden = !error;
  const downloading = !!audioSnapshot?.busy && operation.state === 'downloading';
  $('audio-progress-wrap').hidden = !downloading;
  const total = operation.total_bytes;
  const downloaded = operation.downloaded_bytes;
  if (total > 0 && downloaded >= 0) $('audio-progress').value = Math.min(100, downloaded / total * 100);
  else $('audio-progress').removeAttribute('value');
  $('audio-progress-text').textContent = downloading
    ? total > 0 ? `${audioBytes(downloaded)} of ${audioBytes(total)} downloaded` : 'Downloading model files…' : '';
  if (ready && audioSnapshot.busy && operation.message && operation.state !== 'failed') $('audio-status').textContent += ' ' + operation.message;
  const effective = audioSnapshot?.settings;
  $('audio-effective').textContent = effective ? 'Current settings: ' + (effective.enabled
    ? `${effective.model === 'small' ? 'Small' : 'Base'} · ${effective.threads} ${effective.threads === 1 ? 'thread' : 'threads'}${effective.reply_with_audio ? ' · text and audio replies · ' + ({ piper: 'Piper / Miro', qwen3: 'Qwen3-TTS 1.7B quantized' }[effective.tts_engine] || 'existing Pocket TTS (choose an engine to switch)') : ''}`
    : 'Voice notes disabled.') : '';
  const selectedModel = audioSnapshot?.models?.find(item => item.name === audioDraft?.model);
  $('audio-save').textContent = busy ? 'Applying…' : enabled && selectedModel && !selectedModel.installed ? 'Download and apply' : 'Apply audio settings';
  $('audio-save').disabled = !ready || busy || (!dirty && operation.state !== 'failed');
  $('audio-discard').disabled = !ready || busy || !dirty;
  $('audio-unsaved').hidden = !dirty || busy;
  $('audio-section').setAttribute('aria-busy', String(busy));
}
function acceptAudioSnapshot(value) {
  const settings = audioSettings(value?.settings);
  if (!settings) throw new Error('The gateway returned invalid audio settings. Refresh to try again.');
  const preserveDraft = audioDirty() || audioSaving || !!audioPending;
  audioSnapshot = { ...value, settings };
  if (!preserveDraft) audioDraft = { ...settings };
  if (audioPending && !value.busy) {
    if (value.operation?.state === 'ready' && sameAudioSettings(settings, audioPending)) {
      if (sameAudioSettings(audioDraft, audioPending)) audioDraft = { ...settings };
      audioNotice = value.operation?.message && value.operation.message !== 'Audio settings saved.'
        ? value.operation.message : 'Audio settings applied.';
      audioPending = null;
    } else if (value.operation?.state === 'failed') audioPending = null;
  }
  renderAudio();
}
function scheduleAudioPoll() {
  clearTimeout(audioPollTimer);
  if (authenticated && audioSnapshot?.busy) audioPollTimer = setTimeout(() => {
    if (!rotationPending && !document.hidden) loadAudioSettings(authEpoch).catch(showError);
    else scheduleAudioPoll();
  }, 1000);
}
async function loadAudioSettings(epoch) {
  if (audioLoadingEpoch === epoch || rotationPending) return;
  audioLoadingEpoch = epoch;
  try {
    const value = await call('/audio', { cache: 'no-store' });
    if (epoch !== authEpoch) return;
    audioError = '';
    acceptAudioSnapshot(value);
  } catch (error) {
    if (epoch !== authEpoch) return;
    if (error.status === 401) throw error;
    audioError = error.message || 'Unable to read audio settings. Refresh to try again.';
    renderAudio();
  } finally {
    if (audioLoadingEpoch === epoch) audioLoadingEpoch = null;
    scheduleAudioPoll();
  }
}
function editAudio() {
  if (!audioDraft || audioSaving || audioSnapshot?.busy) return;
  audioDraft.enabled = $('audio-enabled').checked;
  audioDraft.model = $('audio-model-small').checked ? 'small' : 'base';
  const threads = $('audio-threads').valueAsNumber;
  // Keep invalid input in the draft so a refresh cannot silently save a
  // different thread count. Native form validation explains the allowed range.
  audioDraft.threads = !audioDraft.enabled && (!Number.isInteger(threads) || threads < 1 || threads > 32)
    ? audioSnapshot.settings.threads : Number.isFinite(threads) ? threads : null;
  audioDraft.reply_with_audio = $('audio-reply').checked;
  const engine = document.querySelector('input[name="audio-engine"]:checked');
  if (engine && ['piper', 'qwen3'].includes(engine.value)) audioDraft.tts_engine = engine.value;
  if (['auto', 'italian', 'english'].includes($('audio-voice').value)) audioDraft.tts_voice = $('audio-voice').value;
  audioNotice = '';
  renderAudio();
}
async function saveAudio(event) {
  event.preventDefault();
  if (!audioDraft || audioSaving || audioSnapshot?.busy || !$('audio-form').reportValidity()) return;
  const submitted = audioSettings(audioDraft);
  if (!submitted) return;
  const sequence = ++audioSaveSequence;
  audioSaving = true;
  audioPending = { ...submitted };
  audioError = '';
  audioNotice = '';
  renderAudio();
  try {
    const value = await sensitiveCall('/audio/settings', { method: 'POST', body: JSON.stringify(submitted), cache: 'no-store' },
      () => authenticated && sequence === audioSaveSequence);
    if (!authenticated || sequence !== audioSaveSequence) return;
    acceptAudioSnapshot(value);
  } catch (error) {
    if (sequence !== audioSaveSequence) return;
    audioPending = null;
    if (error.status === 401) { sessionAuth.expire('expired'); return; }
    if (authenticated) audioError = error.name === 'NotAllowedError' || error.name === 'AbortError'
      ? 'Verification was cancelled. Your changes have not been submitted.'
      : error.message || 'Unable to apply audio settings. Refresh to check the current status before retrying.';
  } finally {
    if (sequence === audioSaveSequence) {
      audioSaving = false;
      if (authenticated) { renderAudio(); scheduleAudioPoll(); }
    }
  }
}
async function load(includeKeys = true) {
  // A request issued between cookie revocation and login/finish response can
  // return a stale 401 after the new login is verified. Pause both periodic
  // and manual reads until the rotation is resolved.
  if (rotationPending) { reloadPending = true; return; }
  if (loading) { reloadPending = true; return; }
  loading = true;
  const epoch = authEpoch;
  $('refresh').disabled = true;
  try {
    csrf = cookie('__Host-telegramgw-csrf');
    const data = await call('/dashboard');
    if (epoch !== authEpoch) return;
    dashboard = data;
    authenticated = true;
    if (returnToWebUI) { location.replace(returnToWebUI); return; }
    $('auth').hidden = true;
    $('console').hidden = false;
    $('logout').hidden = false;
    $('dashboard-error').hidden = true;
    for (const [id, value] of Object.entries({ workers: data.workers?.length || 0, runtimes: data.runtimes?.length || 0, sessions: visibleSessions().length, approvals: data.pending_approvals, commands: data.queued_commands })) $(id).textContent = number(value);
    renderBot(data.bot);
    renderSessionFilters();
    renderSessions();
    renderWorkers(data.workers || []);
    $('last-refreshed').textContent = 'Updated ' + new Date().toLocaleTimeString() + ' · Refreshes every 15s';
    if (includeKeys) {
      const keys = await call('/passkeys');
      if (epoch !== authEpoch) return;
      renderKeys(keys);
    }
    const browsers = await call('/sessions');
    if (epoch !== authEpoch) return;
    renderBrowserSessions(browsers.sessions);
    if (includeKeys) await loadPasswordSettings(epoch);
    await loadAudioSettings(epoch);
  } catch (error) {
    if (epoch !== authEpoch) return;
    if (!authenticated && error.status !== 401) msg(error.message);
    showError(error);
  } finally {
    loading = false;
    $('refresh').disabled = false;
    if (reloadPending && authenticated) { reloadPending = false; queueMicrotask(() => load()); }
  }
}
function showToken(workerID, token) {
  prompt('Worker ID: ' + workerID + '\n\nCopy this replacement token now; it will not be shown again. Update the worker’s private token file and restart its service:', token);
}
function clearEnrollmentLink() {
  $('enrollment-link').hidden = true;
  $('enrollment-url').value = '';
  $('enrollment-code').textContent = '';
  $('enrollment-copy-status').textContent = '';
}
function updateEnrollment() {
  const remaining = workerEnrollment ? Math.max(0, Math.ceil((workerEnrollment.expiresAt - Date.now()) / 1000)) : 0;
  if (workerEnrollment && !remaining) {
    workerEnrollment.usable = false;
    workerEnrollment.url = '';
    clearEnrollmentLink();
    if (!enrollmentBusy) $('enrollment-status').textContent = 'This URL has expired. Create a new URL to continue.';
  } else if (workerEnrollment?.usable && !enrollmentBusy) {
    $('enrollment-status').textContent = `Use once · expires in ${Math.floor(remaining / 60)}:${String(remaining % 60).padStart(2, '0')}`;
  }
  for (const id of ['enrollment-access', 'enrollment-new', 'enrollment-cancel', 'enrollment-close']) $(id).disabled = enrollmentBusy;
  $('enrollment-copy').disabled = enrollmentBusy || !workerEnrollment?.usable || !remaining;
}
function closeWorkerEnrollment() {
  enrollmentSequence++;
  enrollmentBusy = false;
  workerEnrollment = null;
  clearInterval(enrollmentTimer);
  enrollmentTimer = null;
  clearEnrollmentLink();
  $('enrollment-error').hidden = true;
  $('enrollment-status').textContent = '';
  if ($('worker-enrollment').open) $('worker-enrollment').close();
}
async function enrollmentCall(path, opts, current) {
  await sessionAuth.ensureFresh();
  if (!current() || !authenticated) throw new Error('Sign in again, then create a new URL.');
  const epoch = authEpoch;
  try {
    const result = await call(path, { ...opts, cache: 'no-store', signal: AbortSignal.timeout(20000) });
    if (!current() || epoch !== authEpoch || !authenticated) throw new Error('Your sign-in changed. Create a new URL to continue.');
    return result;
  } catch (error) {
    // Even a confirmed fresh-auth rejection requires a deliberate new action.
    // A network error can mean the mutation succeeded: never replay it here.
    if (error.status === 403 && error.code === 'reauthentication_required') {
      await sessionAuth.reauthenticate();
      throw new Error('Sign-in confirmed. Choose Create new URL or Cancel and revoke to try again.');
    }
    throw error;
  }
}
async function runEnrollment(action) {
  if (enrollmentBusy) return;
  const sequence = ++enrollmentSequence;
  const current = () => sequence === enrollmentSequence && $('worker-enrollment').open;
  enrollmentBusy = true;
  $('enrollment-error').hidden = true;
  $('enrollment-copy-status').textContent = '';
  updateEnrollment();
  try { await action(current); } catch (error) {
    if (current()) {
      if (error.status === 401) { sessionAuth.expire('expired'); return; }
      $('enrollment-error').textContent = error.status === 403 && error.code !== 'reauthentication_required'
        ? 'Access denied. Sign in again before retrying.' : error.message || 'The request failed. Choose an action to try again.';
      $('enrollment-error').hidden = false;
      $('enrollment-status').textContent = workerEnrollment
        ? 'The previous URL may still be valid until it is used or expires. Retry revocation before creating another URL.'
        : 'No URL is available. A failed request may have completed; any URL it created expires within 10 minutes. Choose Create new URL to try again.';
    }
  } finally {
    if (current()) { enrollmentBusy = false; updateEnrollment(); }
  }
}
async function revokeEnrollment(current) {
  if (!workerEnrollment) return;
  workerEnrollment.usable = false;
  workerEnrollment.url = '';
  clearEnrollmentLink();
  if (workerEnrollment.expiresAt > Date.now()) {
    $('enrollment-status').textContent = 'Revoking the previous URL…';
    try { await enrollmentCall('/worker-enrollments/' + workerEnrollment.id, { method: 'DELETE', body: '{}' }, current); } catch (error) {
      // A consumed URL is no longer an unused enrollment. This confirmed
      // absence is safe to replace; an uncertain failure still stops here.
      if (error.status !== 404) throw error;
    }
  }
  workerEnrollment = null;
}
async function createEnrollment(current) {
  await revokeEnrollment(current);
  $('enrollment-status').textContent = 'Creating your one-use URL…';
  const access = $('enrollment-access').value;
  const result = await enrollmentCall('/worker-enrollments', { method: 'POST', body: JSON.stringify({ service_access: access }) }, current);
  const validID = typeof result?.enrollment_id === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(result.enrollment_id) && result.enrollment_id !== '00000000-0000-0000-0000-000000000000';
  const expiresAt = Date.parse(result?.expires_at);
  if (validID) workerEnrollment = { id: result.enrollment_id, expiresAt: Number.isFinite(expiresAt) ? expiresAt : Date.now() + 600000, usable: false, url: '' };
  const enrollmentURL = result?.enrollment_url;
  let url;
  try { url = new URL(enrollmentURL); } catch (_) { /* Invalid responses stay out of the dialog. */ }
  if (!validID || !Number.isFinite(expiresAt) || expiresAt <= Date.now() || result.service_access !== access || !url ||
      typeof enrollmentURL !== 'string' || /\s/.test(enrollmentURL) ||
      url.protocol !== 'https:' || url.origin !== location.origin || url.username || url.password || url.pathname !== '/tgw/enroll/' || url.search || !/^#[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{12}$/i.test(url.hash)) {
    throw new Error('The gateway returned an invalid enrollment URL. Revoke it before creating another URL.');
  }
  // Preserve the supplied URL after validation. Hostname case or an explicit
  // HTTPS port can be meaningful to an installer even when origins match.
  workerEnrollment = { id: result.enrollment_id, expiresAt, usable: true, url: enrollmentURL };
  $('enrollment-url').value = enrollmentURL;
  $('enrollment-code').textContent = url.hash.slice(1);
  $('enrollment-link').hidden = false;
}
function openWorkerEnrollment() {
  if ($('worker-enrollment').open) return;
  closeWorkerEnrollment();
  $('enrollment-access').value = 'restricted';
  $('worker-enrollment').showModal();
  enrollmentTimer = setInterval(updateEnrollment, 1000);
  runEnrollment(createEnrollment);
}
async function copyEnrollmentURL() {
  updateEnrollment();
  if (!workerEnrollment?.usable || enrollmentBusy) return;
  const url = workerEnrollment.url;
  try {
    if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
    await navigator.clipboard.writeText(url);
    if (workerEnrollment?.url === url && $('worker-enrollment').open) $('enrollment-copy-status').textContent = 'URL copied. Paste it into the installer.';
  } catch (_) {
    if (workerEnrollment?.url !== url || !$('worker-enrollment').open) return;
    $('enrollment-url').focus();
    $('enrollment-url').select();
    $('enrollment-copy-status').textContent = 'URL selected. Copy it with Ctrl+C or Command+C, or use your device’s Copy action.';
  }
}
$('login').addEventListener('click', () => login().catch(error => msg(error.message)));
$('password-login').addEventListener('click', () => sessionAuth.login('password').catch(() => {}));
$('password-edit').addEventListener('click', editPassword);
$('password-disable').addEventListener('click', disablePassword);
$('audio-form').addEventListener('submit', saveAudio);
for (const id of ['audio-enabled', 'audio-model-base', 'audio-model-small', 'audio-reply', 'audio-voice', 'audio-engine-piper', 'audio-engine-qwen3']) $(id).addEventListener('change', editAudio);
$('audio-threads').addEventListener('input', editAudio);
$('audio-use-recommended').addEventListener('click', () => {
  if (!audioDraft || audioSaving || audioSnapshot?.busy) return;
  audioDraft.threads = recommendedAudioThreads();
  $('audio-threads').value = audioDraft.threads;
  audioNotice = '';
  renderAudio();
});
$('audio-discard').addEventListener('click', () => {
  if (!audioSnapshot || audioSaving || audioSnapshot.busy) return;
  audioDraft = { ...audioSnapshot.settings };
  audioNotice = '';
  audioError = '';
  renderAudio();
});
$('enroll').addEventListener('click', () => enroll($('bootstrap-token').value).catch(error => msg(error.message)));
$('add-passkey').addEventListener('click', () => perform(async () => { await enroll(); await load(); }));
$('new-worker').addEventListener('click', openWorkerEnrollment);
$('enrollment-access').addEventListener('change', () => runEnrollment(createEnrollment));
$('enrollment-new').addEventListener('click', () => runEnrollment(createEnrollment));
$('enrollment-copy').addEventListener('click', copyEnrollmentURL);
$('enrollment-cancel').addEventListener('click', () => runEnrollment(async current => { await revokeEnrollment(current); closeWorkerEnrollment(); await load(false); }));
$('enrollment-close').addEventListener('click', () => { closeWorkerEnrollment(); load(false); });
$('worker-enrollment').addEventListener('cancel', event => { event.preventDefault(); if (!enrollmentBusy) { closeWorkerEnrollment(); load(false); } });
$('logout').addEventListener('click', () => perform(async () => {
  // An installed web app can outlive a login cookie. Disable this browser's
  // existing device before signing out, including after a fresh admin login.
  let subscriptionID = '';
  try { subscriptionID = localStorage.getItem('codex-webui-push-subscription') || ''; } catch (_) { /* Optional device preference. */ }
  if (subscriptionID) {
    try {
      await fetch('/tgw/api/v1/webui/push/unsubscribe', { method: 'POST', credentials: 'same-origin', signal: AbortSignal.timeout(5000),
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': cookie('__Host-telegramgw-csrf') }, body: JSON.stringify({ subscription_id: subscriptionID }) });
    } catch (_) { /* Server logout also revokes devices bound to this login. */ }
  }
  await call('/logout', { method: 'POST', body: '{}' });
  signedOut('Signed out.');
  try {
    if (navigator.serviceWorker) await Promise.race([
      navigator.serviceWorker.getRegistration('/tgw/webui/').then(registration => registration?.pushManager?.getSubscription()).then(subscription => subscription?.unsubscribe()),
      new Promise(resolve => setTimeout(resolve, 3000)),
    ]);
  } catch (_) { /* Browser cleanup must not prevent completion of sign-out. */ }
  try { localStorage.removeItem('codex-webui-push-subscription'); } catch (_) { /* Storage can be disabled. */ }
  location.reload();
}));
$('logout-everywhere').addEventListener('click', () => perform(async () => {
  if (!confirm('Sign out of every browser, including this one?')) return;
  await call('/sessions/revoke-all', { method: 'POST', body: '{}' });
  signedOut('Signed out everywhere.');
}));
$('refresh').addEventListener('click', () => load());
for (const id of ['session-search', 'session-state']) $(id).addEventListener(id === 'session-search' ? 'input' : 'change', () => { sessionPage = 0; renderSessions(); });
$('session-prev').addEventListener('click', () => { sessionPage--; renderSessions(); });
$('session-next').addEventListener('click', () => { sessionPage++; renderSessions(); });
setInterval(() => { if (authenticated && !document.hidden) load(false); }, 15000);
sessionAuth = window.CodexSessionAuth.create({
  mount: $('auth-warning'),
  isLocked: () => !authenticated,
  onBeforeRotate: () => {
    authEpoch++; rotationPending = true;
    $('console').inert = true; $('logout').disabled = true;
    $('worker-enrollment').inert = true;
  },
  onAuthenticated: async () => {
    rotationPending = false; $('console').inert = false; $('logout').disabled = false;
    $('worker-enrollment').inert = false;
    authEpoch++; authenticated = true; await load();
  },
  onExpired: lockConsole,
  onRenewing: busy => {
    $('login').disabled = busy;
    $('password-login').disabled = busy;
    if (!busy && rotationPending) {
      // A failed finish may have left either the old or new cookie valid.
      // Check once before resuming reads; never replay the login mutation.
      sessionAuth.verify('rotation-recovery').then(value => {
        rotationPending = false;
        if (value) return load();
      }).catch(showError).finally(() => {
        rotationPending = false; $('console').inert = false; $('logout').disabled = false;
        $('worker-enrollment').inert = false;
      });
    }
  },
  onOptions: options => {
    $('password-login').hidden = !options.password;
    $('auth-title').textContent = options.password ? 'Gateway access' : 'Passkey access';
    $('auth-description').textContent = options.password ? 'Use a personal passkey or your gateway password to administer this gateway.' : 'Use a personal passkey to administer this gateway.';
  },
  onStatus: msg,
});
sessionAuth.verify('initial').catch(error => { if (error.status !== 401) msg(error.message); });

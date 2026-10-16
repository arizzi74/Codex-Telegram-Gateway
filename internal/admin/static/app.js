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
const expandedSessions = new Set();
const expandedSessionDetails = new Set();
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
// Telegram tokens stay in the submitted request's lifetime, never in storage.
let telegramBots = null;
let telegramLoadingEpoch = null;
let telegramReadPromise = null;
let telegramPollTimer = null;
let telegramBusy = false;
let telegramSequence = 0;
let telegramTokenTarget = null;
let telegramCreationID = '';
let telegramRemovalTarget = null;
let telegramRemovedBots = [];
const expandedTelegramBots = new Set();
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
    try { const detail = await response.json(); error.code = detail.code; error.takeoverRequired = detail.takeover_required === true; if (detail.message) error.message = detail.message; } catch (_) { /* Non-JSON errors retain the safe generic message. */ }
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
  expandedSessions.clear();
  expandedSessionDetails.clear();
  reloadPending = false;
  closeWorkerEnrollment();
  resetTelegramBots();
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
  // Keep the configured primary bot visible if an older gateway has no list API.
  if (telegramBots) { renderTelegramBots(); return; }
  const root = $('bot-info');
  root.replaceChildren();
  if (!bot) {
    root.append(el('p', 'muted', 'Bot information is not available.'));
    return;
  }
  root.append(telegramBotCard({ ...bot, legacy: true, enabled: true, telegram_id: bot.id, id: '' }));
  $('bot-create').disabled = false;
}
function telegramBotCard(bot) {
  const card = el('article', 'telegram-bot');
  card.dataset.botId = String(bot.id || '');
  const key = String(bot.id || 'primary');
  const content = el('div', 'bot-content');
  content.id = 'bot-content-' + key;
  content.hidden = !expandedTelegramBots.has(key);
  const heading = el('div', 'bot-title');
  const title = el('h3');
  const toggle = el('button', 'bot-toggle', bot.display_name || 'Telegram bot');
  toggle.type = 'button';
  toggle.setAttribute('aria-expanded', String(!content.hidden));
  toggle.setAttribute('aria-controls', content.id);
  toggle.addEventListener('click', () => {
    content.hidden = !content.hidden;
    toggle.setAttribute('aria-expanded', String(!content.hidden));
    if (content.hidden) expandedTelegramBots.delete(key);
    else expandedTelegramBots.add(key);
  });
  title.append(toggle);
  heading.append(title);
  if (bot.id) {
    const actions = el('div', 'bot-header-actions');
    actions.append(button(bot.enabled === false ? 'Enable' : 'Disable', 'secondary', () => toggleTelegramBot(bot)));
    if (!bot.legacy) actions.append(button('Delete', 'danger', () => openBotRemoval(bot)));
    for (const control of actions.querySelectorAll('button')) control.disabled = telegramBusy;
    heading.append(actions);
  }
  card.append(heading, content);
  const badges = el('div', 'badges');
  badges.append(badge(bot.enabled === false ? 'disabled' : bot.status || 'connected'));
  if (bot.legacy) badges.append(el('span', 'badge', 'Primary'));
  else badges.append(el('span', 'badge', bot.managed ? 'Managed' : 'Connected'));
  content.append(badges);
  const identity = el('div', 'bot-identity');
  if (bot.username) identity.append(el('span', '', '@' + bot.username));
  if (/^[A-Za-z0-9_]{5,32}$/.test(bot.username || '')) {
    const link = el('a', 'bot-open', 'Open Telegram');
    link.href = 'https://t.me/' + bot.username;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
    identity.append(link);
  }
  if (identity.childNodes.length) content.append(identity);
  const facts = el('dl', 'facts');
  if (bot.webhook_status) fact(facts, 'Webhook', words(bot.webhook_status));
  if (bot.pending_updates !== undefined) fact(facts, 'Pending Telegram updates', number(bot.pending_updates));
  if (bot.allowed_user_count !== undefined) fact(facts, 'Allowed users', number(bot.allowed_user_count));
  if (bot.allowed_chat_count !== undefined) fact(facts, 'Allowed chats', bot.allowed_chat_count === 0 ? 'Any chat with an allowed user' : number(bot.allowed_chat_count));
  fact(facts, 'Bot ID', bot.telegram_id || '—', { mono: true });
  if (bot.checked_at) fact(facts, 'Checked', timestamp(bot.checked_at));
  if (bot.webhook_url) fact(facts, 'Webhook address', bot.webhook_url, { wide: true, mono: true });
  if (bot.managed) fact(facts, 'Access', 'Same allowed owner and workers as this gateway', { wide: true });
  content.append(facts);
  if (bot.id) {
    const nameSetting = el('div', 'bot-name-setting');
    const label = el('label', 'bot-name-toggle');
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.checked = bot.inherit_session_name === true;
    checkbox.disabled = telegramBusy;
    checkbox.dataset.botNameSetting = String(bot.id);
    const help = el('p', 'muted', 'Original name · Session name; uses your private chat selection. Updates the default Telegram display name.');
    help.id = 'bot-name-help-' + bot.id;
    checkbox.setAttribute('aria-describedby', help.id);
    checkbox.addEventListener('change', () => toggleTelegramBotSessionName(bot, checkbox.checked));
    label.append(checkbox, el('span', '', 'Inherit selected session name'));
    nameSetting.append(label, help);
    if (bot.original_display_name) nameSetting.append(el('p', 'muted', 'Original name: ' + bot.original_display_name));
    let nameStatus = '';
    if (bot.enabled === false && (bot.inherit_session_name || bot.name_sync_pending)) nameStatus = 'Name changes resume when this bot is enabled.';
    else if (bot.name_error === 'name_webhook_moved') nameStatus = 'This bot’s webhook moved. Reconnect it here to resume name updates.';
    else if (bot.name_error) nameStatus = 'Telegram has not applied the name change yet. Check connectivity and refresh to see its status.';
    else if (bot.name_sync_pending) nameStatus = 'Name update queued; Telegram may take a moment to apply it.';
    if (nameStatus) {
      const status = el('p', bot.name_error ? 'notice error' : 'muted', nameStatus);
      status.setAttribute('role', 'status');
      nameSetting.append(status);
    }
    content.append(nameSetting);
    const actions = el('div', 'bot-actions');
    if (!bot.legacy) {
      const replace = button('Replace token', 'secondary', () => openBotToken(bot));
      replace.disabled = telegramBusy;
      actions.append(replace);
    }
    const reset = button('Reset profile photo >_', 'secondary', () => resetTelegramBotProfilePhoto(bot));
    reset.dataset.botDisabled = String(bot.enabled === false);
    reset.disabled = telegramBusy || bot.enabled === false;
    const photoHelp = el('p', 'muted bot-photo-help', telegramPhotoResetHelp(bot.enabled === false));
    photoHelp.id = 'bot-photo-help-' + bot.id;
    photoHelp.dataset.botDisabled = String(bot.enabled === false);
    reset.setAttribute('aria-describedby', photoHelp.id);
    actions.append(reset);
    content.append(actions, photoHelp);
  }
  if (bot.legacy) content.append(el('p', 'token-note', 'Primary bot · Its token is managed in the gateway configuration.'));
  if (bot.last_error || bot.error) {
    const warning = el('p', 'notice error', 'Telegram reported a connection problem. Refresh to check the bot’s current status.');
    if (date(bot.last_error_at)) warning.append(el('span', '', ' · ' + timestamp(bot.last_error_at)));
    content.append(warning);
  }
  return card;
}
function telegramManagers() {
  // BotFather changes can outpace the cached capability check. Creating the
  // link checks the chosen enabled bot again on the gateway.
  return (telegramBots?.bots || []).filter(bot => bot.enabled !== false);
}
function telegramCreationURL(pending) {
  let url;
  try { url = new URL(pending.launch_url); } catch (_) { return ''; }
  // Only the Telegram bot-creation route is a valid external destination.
  if (url.protocol !== 'https:' || url.hostname !== 't.me' || url.port || url.username || url.password || url.hash ||
      !/^\/newbot\/[A-Za-z0-9_]{5,32}\/[A-Za-z0-9_]{5,32}$/.test(url.pathname)) return '';
  return url.href;
}
function telegramPendingActive(pending) {
  return pending.status === 'pending' && Date.parse(pending.expires_at) > Date.now();
}
function telegramPendingText(pending) {
  const username = pending.requested_username ? '@' + pending.requested_username : 'Your new bot';
  if (pending.status === 'connected') return username + ' is connected. It is available in the bot list.';
  if (pending.status === 'failed') return username + ' could not be connected. Refresh the bot list before trying again.';
  if (pending.status === 'cancelled') return 'Creation of ' + username + ' was cancelled.';
  if (!telegramPendingActive(pending)) return 'The gateway confirmation window for ' + username + ' expired. Create a new request, or connect the bot with its token if you already created it in Telegram.';
  const minutes = Math.max(1, Math.ceil((Date.parse(pending.expires_at) - Date.now()) / 60000));
  return 'Waiting for Telegram to confirm ' + username + ' · Gateway confirmation window closes in ' + minutes + ' minute' + (minutes === 1 ? '' : 's') + '.';
}
function renderTelegramManagers() {
  const select = $('bot-manager');
  const previous = select.value;
  const managers = telegramManagers();
  select.replaceChildren();
  for (const bot of managers) {
    const option = el('option', '', bot.username ? '@' + bot.username : bot.display_name || 'Manager bot');
    option.value = String(bot.id);
    select.append(option);
  }
  if (!managers.length) {
    const option = el('option', '', 'No manager bot available'); option.value = ''; select.append(option);
  } else if (managers.some(bot => String(bot.id) === previous)) select.value = previous;
  select.disabled = telegramBusy || !managers.length;
  $('bot-create-submit').disabled = telegramBusy || !managers.length;
  $('bot-manager-help').textContent = managers.length
    ? 'The gateway refreshes the selected bot’s Telegram profile and checks Bot Management Mode when creating the link.'
    : 'Connect or enable a bot first, then enable its Bot Management Mode in @BotFather.';
}
function renderTelegramCreation() {
  if (!$('bot-create-dialog').open || !telegramCreationID) return;
  const pending = telegramBots?.pending.find(item => item.id === telegramCreationID);
  if (!pending) return;
  $('bot-create-result-status').textContent = telegramPendingText(pending);
  const url = telegramPendingActive(pending) ? telegramCreationURL(pending) : '';
  $('bot-create-launch').hidden = !url;
  if (url) $('bot-create-launch').href = url;
  else $('bot-create-launch').removeAttribute('href');
}
function renderTelegramBots() {
  const root = $('bot-info');
  root.replaceChildren();
  for (const bot of telegramBots?.bots || []) {
    const details = bot.legacy ? { ...dashboard?.bot, ...bot, telegram_id: bot.telegram_id || dashboard?.bot?.id } : bot;
    root.append(telegramBotCard(details));
  }
  if (!telegramBots?.bots.length) root.append(el('p', 'muted', 'No Telegram bots are connected.'));
  $('bot-create').disabled = telegramBusy;
  $('bot-connect').disabled = telegramBusy;
  const pendingRoot = $('bot-pending');
  pendingRoot.replaceChildren();
  for (const pending of telegramBots?.pending || []) {
    // Completed requests are represented by their bot card.
    if (pending.status === 'connected') continue;
    const card = el('article', 'telegram-pending');
    card.dataset.pendingId = String(pending.id);
    card.append(el('h3', '', pending.display_name || 'Managed bot creation'), el('p', 'muted', telegramPendingText(pending)));
    const actions = el('div', 'bot-actions');
    const url = telegramPendingActive(pending) ? telegramCreationURL(pending) : '';
    if (url) {
      const link = el('a', 'button-link secondary', 'Continue in Telegram');
      link.href = url; link.target = '_blank'; link.rel = 'noopener noreferrer'; actions.append(link);
    }
    actions.append(button('Refresh status', 'secondary', () => loadTelegramBots()));
    card.append(actions);
    pendingRoot.append(card);
  }
  renderTelegramManagers();
  renderTelegramCreation();
  renderRemovedTelegramBots();
}
function scheduleTelegramPoll() {
  clearTimeout(telegramPollTimer);
  const namePending = telegramBots?.bots.some(bot => bot.enabled !== false && bot.name_sync_pending && !bot.name_error);
  if (authenticated && (telegramBots?.pending.some(telegramPendingActive) || namePending)) telegramPollTimer = setTimeout(() => {
    if (!rotationPending && !document.hidden) loadTelegramBots();
    else scheduleTelegramPoll();
  }, 3000);
}
async function loadTelegramBots(epoch = authEpoch, freshAfterMutation = false, refreshMetadata = false) {
  if (!authenticated || rotationPending) return;
  if (telegramLoadingEpoch === epoch) {
    if (!freshAfterMutation && !refreshMetadata) return;
    // A poll that started before the write can return the old list. Wait for
    // that read, then obtain a snapshot that includes the completed mutation.
    // Explicit profile refreshes must also keep their POST after a cached GET.
    await telegramReadPromise?.catch(() => {});
    if (epoch === authEpoch && authenticated) return loadTelegramBots(epoch, freshAfterMutation, refreshMetadata);
    return;
  }
  telegramLoadingEpoch = epoch;
  let request;
  try {
    request = refreshMetadata
      ? call('/telegram-bots/refresh', { method: 'POST', cache: 'no-store' })
      : call('/telegram-bots', { cache: 'no-store' });
    telegramReadPromise = request;
    const value = await request;
    if (epoch !== authEpoch || !authenticated) return;
    if (!Array.isArray(value?.bots)) return;
    telegramBots = { bots: value.bots, pending: Array.isArray(value.pending) ? value.pending : [] };
    $('bot-error').hidden = true;
    if (refreshMetadata) $('bot-create-error').hidden = true;
    renderTelegramBots();
  } catch (error) {
    if (epoch !== authEpoch || !authenticated) return;
    if (error.status === 401) { sessionAuth.expire('expired'); return; }
    const errorID = refreshMetadata && $('bot-create-dialog').open ? 'bot-create-error' : 'bot-error';
    $(errorID).textContent = refreshMetadata
      ? 'Unable to refresh Telegram bot profiles. Try Refresh manager bots again.'
      : 'Unable to refresh Telegram bots. Please try again.';
    $(errorID).hidden = false;
  } finally {
    if (telegramLoadingEpoch === epoch) telegramLoadingEpoch = null;
    if (telegramReadPromise === request) telegramReadPromise = null;
    scheduleTelegramPoll();
  }
}
function telegramError(error) {
  // Never surface a Telegram/server error verbatim: it can contain a token.
  if (error.name === 'NotAllowedError' || error.name === 'AbortError') return 'Verification was cancelled. Your changes have not been submitted.';
  if (error.status === 400) return 'Check the bot token or the new bot’s display name, then try again.';
  if (error.status === 403) return 'Access denied. Sign in again and retry.';
  if (error.status === 404) return 'This bot is no longer available. Refresh the bot list.';
  if (error.code === 'telegram_bot_duplicate') return 'This bot is already connected to the gateway.';
  if (error.code === 'telegram_bot_legacy') return 'The primary bot’s token is managed in the gateway configuration.';
  if (error.code === 'telegram_bot_manager_unavailable') return 'Telegram still reports that the selected bot cannot manage bots after refreshing its profile. Enable Bot Management Mode for that bot in @BotFather, then try again.';
  if (error.code === 'telegram_bot_username_unverified') return 'Unable to check Telegram usernames right now. Please try again.';
  if (error.code === 'telegram_bot_username_unavailable') return 'Could not find a usable username for this display name. Choose a different display name and try again.';
  if (error.code === 'telegram_bot_disabled') return 'Enable this bot before resetting its profile photo.';
  if (error.code === 'telegram_bot_profile_webhook_conflict') return 'This bot is not connected to this gateway’s webhook. Reconnect it here before resetting its profile photo.';
  if (error.code === 'telegram_bot_owner_changed') return 'This managed bot’s owner changed. Reconnect it with its current token before resetting its profile photo.';
  if (error.takeoverRequired) return 'This bot already has a Telegram webhook. Reconnect it with its token to confirm moving it to this gateway.';
  if (error.status === 409) return 'The bot’s state changed. Refresh the bot list before trying again.';
  if (error.status === 502) return 'Telegram could not complete the request. Refresh the bot list before retrying.';
  return 'Unable to complete the request. Refresh the bot list to check whether it completed before retrying.';
}
async function telegramCall(path, opts, current) {
  return sensitiveCall('/telegram-bots' + path, { ...opts, cache: 'no-store' },
    () => authenticated && current());
}
function updateTelegramBusy() {
  $('bot-connect').disabled = telegramBusy;
  $('bot-create').disabled = telegramBusy;
  for (const id of ['bot-token', 'bot-token-submit', 'bot-token-close', 'bot-token-cancel', 'bot-create-name', 'bot-create-close', 'bot-create-cancel', 'bot-create-refresh', 'bot-create-done', 'bot-manager-refresh', 'bot-remove-confirm', 'bot-remove-close', 'bot-remove-cancel']) $(id).disabled = telegramBusy;
  for (const control of $('bot-info').querySelectorAll('button:not(.bot-toggle), input')) control.disabled = telegramBusy || control.dataset.botDisabled === 'true';
  for (const help of $('bot-info').querySelectorAll('.bot-photo-help')) help.textContent = telegramPhotoResetHelp(help.dataset.botDisabled === 'true');
  renderTelegramManagers();
}
async function runTelegramAction(action, errorID = 'bot-error') {
  if (telegramBusy) return;
  const sequence = ++telegramSequence;
  const current = () => sequence === telegramSequence;
  telegramBusy = true;
  $(errorID).hidden = true;
  updateTelegramBusy();
  try { await action(current); } catch (error) {
    if (!current() || !authenticated) return;
    if (error.status === 401) { sessionAuth.expire('expired'); return; }
    $(errorID).textContent = telegramError(error);
    $(errorID).hidden = false;
  } finally {
    if (current()) { telegramBusy = false; updateTelegramBusy(); }
  }
}
function closeBotToken() {
  $('bot-token').value = '';
  telegramTokenTarget = null;
  $('bot-token-status').textContent = '';
  $('bot-token-error').hidden = true;
  if ($('bot-token-dialog').open) $('bot-token-dialog').close();
}
function openBotToken(bot = null) {
  if (telegramBusy || !authenticated) return;
  telegramTokenTarget = bot;
  $('bot-token-heading').textContent = bot ? 'Replace bot token' : 'Connect existing bot';
  $('bot-token-description').textContent = bot
    ? 'Enter the replacement token for ' + (bot.username ? '@' + bot.username : 'this bot') + '. It must belong to the same Telegram bot. Get a new token from @BotFather if the old one was revoked.'
    : 'Get the token from @BotFather, then enter it here. The gateway checks the bot and connects its Telegram webhook.';
  $('bot-token-submit').textContent = bot ? 'Replace token' : 'Connect bot';
  $('bot-token').value = '';
  $('bot-token-status').textContent = '';
  $('bot-token-error').hidden = true;
  $('bot-token-dialog').showModal();
  $('bot-token').focus();
}
async function submitBotToken(event) {
  event.preventDefault();
  if (telegramBusy || !$('bot-token-form').reportValidity()) return;
  const token = $('bot-token').value.trim();
  const target = telegramTokenTarget;
  $('bot-token').value = '';
  if (!token) return;
  await runTelegramAction(async current => {
    $('bot-token-status').textContent = 'Checking and connecting the Telegram bot…';
    const payload = { token, takeover: false };
    if (target) payload.replace_id = target.id;
    try {
      await telegramCall('', { method: 'POST', body: JSON.stringify(payload) }, current);
    } catch (error) {
      if (!current() || !authenticated) return;
      if (!error.takeoverRequired) throw error;
      if (!confirm('This bot is connected to another Telegram webhook. Move it to this gateway? Its previous application will stop receiving new Telegram updates.')) {
        $('bot-token-status').textContent = 'Connection cancelled. The existing webhook was kept.';
        return;
      }
      payload.takeover = true;
      await telegramCall('', { method: 'POST', body: JSON.stringify(payload) }, current);
    }
    if (!current() || !authenticated) return;
    closeBotToken();
    $('bot-status').textContent = target ? 'Bot token replaced.' : 'Telegram bot connected.';
    await loadTelegramBots(authEpoch, true);
  }, 'bot-token-error');
  if ($('bot-token-error').hidden === false) $('bot-token-status').textContent = '';
}
async function toggleTelegramBot(bot) {
  await runTelegramAction(async current => {
    await telegramCall('/' + encodeURIComponent(bot.id), { method: 'PATCH', body: JSON.stringify({ enabled: bot.enabled === false }) }, current);
    if (!current() || !authenticated) return;
    $('bot-status').textContent = bot.enabled === false ? 'Telegram bot enabled.' : 'Telegram bot disabled.';
    await loadTelegramBots(authEpoch, true);
  });
}
function telegramPhotoResetHelp(disabled) {
  if (disabled) return 'Enable this bot to reset its profile photo.';
  if (telegramBusy) return 'Wait for the current bot action to finish before resetting a profile photo.';
  return 'Replaces the current Telegram profile photo with the default Gateway icon.';
}
async function resetTelegramBotProfilePhoto(bot) {
  if (telegramBusy || !authenticated || !bot.id || bot.enabled === false) return;
  if (!confirm('Reset the profile photo for ' + telegramBotLabel(bot) + '? This replaces its current Telegram profile photo with the default Gateway icon.')) return;
  let completed = false;
  await runTelegramAction(async current => {
    $('bot-status').textContent = 'Resetting the profile photo for ' + telegramBotLabel(bot) + '…';
    await telegramCall('/' + encodeURIComponent(bot.id) + '/profile-photo/reset', { method: 'POST', body: JSON.stringify({ confirmed: true }) }, current);
    if (!current() || !authenticated) return;
    completed = true;
    $('bot-status').textContent = 'Profile photo reset for ' + telegramBotLabel(bot) + '.';
    await loadTelegramBots(authEpoch, true);
  });
  if (!completed && authenticated) $('bot-status').textContent = '';
}
async function toggleTelegramBotSessionName(bot, enabled) {
  await runTelegramAction(async current => {
    await telegramCall('/' + encodeURIComponent(bot.id), { method: 'PATCH', body: JSON.stringify({ inherit_session_name: enabled }) }, current);
    if (!current() || !authenticated) return;
    $('bot-status').textContent = enabled
      ? 'Session name preference saved. Telegram applies the name in the background.'
      : 'Original name preference saved. Telegram restores the name in the background.';
    await loadTelegramBots(authEpoch, true);
  });
  if (!authenticated) return;
  const checkbox = [...$('bot-info').querySelectorAll('input[data-bot-name-setting]')].find(input => input.dataset.botNameSetting === String(bot.id));
  if (checkbox) {
    checkbox.checked = telegramBots?.bots.find(item => String(item.id) === String(bot.id))?.inherit_session_name === true;
    checkbox.focus();
  }
}
function telegramBotLabel(bot) {
  return bot.username ? '@' + bot.username : bot.display_name || 'this bot';
}
function closeBotRemoval() {
  telegramRemovalTarget = null;
  $('bot-remove-heading').textContent = 'Remove bot';
  $('bot-remove-target').textContent = '';
  $('bot-remove-error').hidden = true;
  $('bot-remove-status').textContent = '';
  if ($('bot-remove-dialog').open) $('bot-remove-dialog').close();
}
function openBotRemoval(bot) {
  if (telegramBusy || !authenticated || bot.legacy) return;
  telegramRemovalTarget = { ...bot };
  $('bot-remove-heading').textContent = 'Remove ' + telegramBotLabel(bot);
  $('bot-remove-target').textContent = 'To delete ' + telegramBotLabel(bot) + ' permanently on Telegram, open BotFather as its owner, select this bot, and confirm deletion there.';
  $('bot-remove-confirm').hidden = false;
  $('bot-remove-error').hidden = true;
  $('bot-remove-status').textContent = '';
  $('bot-remove-cancel').textContent = 'Cancel';
  $('bot-remove-dialog').showModal();
  $('bot-remove-cancel').focus();
}
function renderRemovedTelegramBots() {
  const root = $('bot-removed');
  root.replaceChildren();
  for (const bot of telegramRemovedBots) {
    const notice = el('div', 'notice bot-removed-notice');
    notice.append(el('p', '', telegramBotLabel(bot) + ' was removed from this gateway. To delete it on Telegram, open BotFather as its owner, select this bot, and confirm deletion.'));
    const link = el('a', 'button-link secondary', 'Delete in BotFather');
    link.href = 'https://t.me/BotFather?start=deletebot';
    link.target = '_blank'; link.rel = 'noopener noreferrer';
    const dismiss = button('Dismiss', 'secondary', () => {
      telegramRemovedBots = telegramRemovedBots.filter(item => item.id !== bot.id);
      renderRemovedTelegramBots();
    });
    const actions = el('div', 'bot-actions');
    actions.append(link, dismiss); notice.append(actions); root.append(notice);
  }
}
async function removeTelegramBot() {
  const bot = telegramRemovalTarget;
  if (!bot || bot.legacy || telegramBusy) return;
  await runTelegramAction(async current => {
    await telegramCall('/' + encodeURIComponent(bot.id), { method: 'DELETE', body: JSON.stringify({ confirmed: true }) }, current);
    if (!current() || !authenticated) return;
    telegramRemovedBots = [...telegramRemovedBots.filter(item => item.id !== bot.id), bot];
    $('bot-status').textContent = telegramBotLabel(bot) + ' removed from this gateway. Worker sessions and project files were kept.';
    $('bot-remove-confirm').hidden = true;
    $('bot-remove-cancel').textContent = 'Done';
    $('bot-remove-status').textContent = 'Removed from this gateway. Deletion on Telegram is a separate step in BotFather below.';
    renderRemovedTelegramBots();
    await loadTelegramBots(authEpoch, true);
  }, 'bot-remove-error');
}
function closeBotCreate() {
  telegramCreationID = '';
  $('bot-create-form').reset();
  $('bot-create-result').hidden = true;
  $('bot-create-form').hidden = false;
  $('bot-create-launch').hidden = true;
  $('bot-create-launch').removeAttribute('href');
  $('bot-create-error').hidden = true;
  $('bot-create-status').textContent = '';
  if ($('bot-create-dialog').open) $('bot-create-dialog').close();
}
function openBotCreate() {
  if (telegramBusy || !authenticated) return;
  closeBotCreate();
  renderTelegramManagers();
  $('bot-create-dialog').showModal();
}
async function submitBotCreate(event) {
  event.preventDefault();
  if (telegramBusy || !$('bot-create-form').reportValidity()) return;
  const payload = { manager_id: $('bot-manager').value, display_name: $('bot-create-name').value.trim() };
  if (!telegramManagers().some(bot => String(bot.id) === payload.manager_id)) return;
  await runTelegramAction(async current => {
    $('bot-create-status').textContent = 'Checking Telegram username…';
    const pending = await telegramCall('/managed', { method: 'POST', body: JSON.stringify(payload) }, current);
    if (!current() || !authenticated) return;
    if (!pending?.id || !telegramCreationURL(pending) || !telegramPendingActive(pending)) throw new Error('Invalid creation response.');
    telegramCreationID = pending.id;
    telegramBots.pending = [...telegramBots.pending.filter(item => item.id !== pending.id), pending];
    $('bot-create-form').hidden = true;
    $('bot-create-result').hidden = false;
    renderTelegramBots();
    scheduleTelegramPoll();
    await loadTelegramBots(authEpoch, true);
    $('bot-create-launch').focus();
  }, 'bot-create-error');
  if ($('bot-create-error').hidden === false) $('bot-create-status').textContent = '';
}
function resetTelegramBots() {
  telegramSequence++;
  telegramBusy = false;
  telegramBots = null;
  telegramLoadingEpoch = null;
  telegramReadPromise = null;
  clearTimeout(telegramPollTimer);
  closeBotToken();
  closeBotCreate();
  closeBotRemoval();
  telegramRemovedBots = [];
  expandedTelegramBots.clear();
  $('bot-removed').replaceChildren();
  $('bot-pending').replaceChildren();
  $('bot-status').textContent = '';
  $('bot-error').hidden = true;
  updateTelegramBusy();
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
function renderSession(session) {
  const stats = session.stats || {};
  const card = el('details', 'session-card');
  card.dataset.sessionId = session.session_id;
  card.open = expandedSessions.has(session.session_id);
  card.addEventListener('toggle', () => {
    if (!card.isConnected) return;
    if (card.open) expandedSessions.add(session.session_id);
    else expandedSessions.delete(session.session_id);
  });
  const summary = el('summary');
  const heading = el('div', 'session-title');
  heading.append(el('h3', '', sessionName(session)));
  const badges = el('div', 'badges');
  badges.append(badge(session.state));
  if (session.pending_approvals > 0) badges.append(el('span', 'badge warning', `${number(session.pending_approvals)} awaiting approval`));
  if (session.queued_commands > 0) badges.append(el('span', 'badge', `${number(session.queued_commands)} queued`));
  heading.append(badges);
  summary.append(heading);
  const content = el('div', 'session-content');
  card.append(summary, content);
  content.append(el('p', 'session-location muted', [session.worker_name, session.runtime_name].filter(Boolean).join(' · ') || 'Worker information unavailable'));
  content.append(el('p', 'session-path', session.cwd || 'Directory unavailable'));
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
  content.append(timing);
  const metrics = el('dl', 'session-metrics');
  fact(metrics, 'Prompts', messageCount(stats.prompt_count, stats));
  fact(metrics, 'Assistant replies', messageCount(stats.assistant_message_count, stats));
  fact(metrics, 'Total tokens', number(stats.total_tokens));
  fact(metrics, 'Context tokens', number(stats.context_tokens));
  content.append(metrics);
  if (stats.history_complete === false) content.append(el('p', 'token-note', 'Partial history · message counts may still be loading.'));
  const lastMessage = el('div', 'last-message');
  lastMessage.append(el('p', 'last-message-label', 'Last message' + (stats.last_message_role ? ' · ' + words(stats.last_message_role) : '')));
  lastMessage.append(el('p', 'last-message-text', stats.last_message || 'Not available'));
  content.append(lastMessage);
  const details = el('details', 'session-details');
  details.open = expandedSessionDetails.has(session.session_id);
  details.addEventListener('toggle', () => {
    if (!details.isConnected) return;
    if (details.open) expandedSessionDetails.add(session.session_id);
    else expandedSessionDetails.delete(session.session_id);
  });
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
  content.append(details);
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
  // Capture synchronously too: a refresh can run before a native toggle event.
  for (const card of $('session-list').querySelectorAll('.session-card')) {
    const key = card.dataset.sessionId;
    if (card.open) expandedSessions.add(key); else expandedSessions.delete(key);
    if (card.querySelector('.session-details').open) expandedSessionDetails.add(key); else expandedSessionDetails.delete(key);
  }
  $('session-list').replaceChildren();
  const pageSessions = filtered.slice(sessionPage * pageSize, (sessionPage + 1) * pageSize);
  for (const session of pageSessions) $('session-list').append(renderSession(session));
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
    if (epoch !== authEpoch) return;
    await loadTelegramBots(epoch);
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
$('bot-connect').addEventListener('click', () => openBotToken());
$('bot-token-form').addEventListener('submit', submitBotToken);
for (const id of ['bot-token-close', 'bot-token-cancel']) $(id).addEventListener('click', closeBotToken);
$('bot-token-dialog').addEventListener('cancel', event => { if (telegramBusy) event.preventDefault(); else closeBotToken(); });
$('bot-create').addEventListener('click', openBotCreate);
$('bot-create-form').addEventListener('submit', submitBotCreate);
for (const id of ['bot-create-close', 'bot-create-cancel', 'bot-create-done']) $(id).addEventListener('click', closeBotCreate);
$('bot-create-refresh').addEventListener('click', () => loadTelegramBots());
$('bot-manager-refresh').addEventListener('click', () => loadTelegramBots(authEpoch, false, true));
$('bot-create-dialog').addEventListener('cancel', event => { if (telegramBusy) event.preventDefault(); else closeBotCreate(); });
for (const id of ['bot-remove-close', 'bot-remove-cancel']) $(id).addEventListener('click', closeBotRemoval);
$('bot-remove-confirm').addEventListener('click', removeTelegramBot);
$('bot-remove-dialog').addEventListener('cancel', event => { if (telegramBusy) event.preventDefault(); else closeBotRemoval(); });
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
for (const section of document.querySelectorAll('#console > .console-section')) {
  const attention = el('span', 'badge warning section-attention', 'Needs attention');
  attention.hidden = true;
  section.querySelector(':scope > summary').append(attention);
  const update = () => {
    attention.hidden = section.open || ![...section.querySelectorAll('.notice.error:not([hidden])')].some(error => error.textContent.trim());
  };
  section.addEventListener('toggle', update);
  new MutationObserver(update).observe(section.querySelector(':scope > .section-content'), { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ['hidden'] });
}
setInterval(() => { if (authenticated && !document.hidden) load(false); }, 15000);
sessionAuth = window.CodexSessionAuth.create({
  mount: $('auth-warning'),
  isLocked: () => !authenticated,
  onBeforeRotate: () => {
    authEpoch++; rotationPending = true;
    $('console').inert = true; $('logout').disabled = true;
    $('worker-enrollment').inert = true;
    $('bot-token-dialog').inert = true; $('bot-create-dialog').inert = true; $('bot-remove-dialog').inert = true;
  },
  onAuthenticated: async () => {
    rotationPending = false; $('console').inert = false; $('logout').disabled = false;
    $('worker-enrollment').inert = false;
    $('bot-token-dialog').inert = false; $('bot-create-dialog').inert = false; $('bot-remove-dialog').inert = false;
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
        $('bot-token-dialog').inert = false; $('bot-create-dialog').inert = false; $('bot-remove-dialog').inert = false;
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

'use strict';
(() => {
  const { node, clean, markdown, code } = window.CodexFormat;
  const $ = id => document.getElementById(id);
  const api = '/tgw/api/v1/webui';
  const state = { sessions: [], selected: null, socket: null, generation: 0, authGeneration: 0, sequence: 0, pending: new Map(), drafts: new Map(), positions: new Map(), items: new Map(), questions: new Map(), models: [], turn: null, connected: false, stopped: false, attempts: 0, timer: null, loading: false, cursor: null, queuedEvents: [], submitting: false, renderTimer: null, authenticated: false, refreshing: false, refreshTimer: null };
  let commandUI = null, notificationUI = null, sessionAuth = null, rawView = false, gatewayCommandAbort = null;
  let authRestore = null, restoringPosition = false, uncertainSend = false;
  let draftRecovery = null, recoveredDrafts = null, draftSaveTimer = null, draftDirty = false;
  const uncertainDrafts = new Set();
  let sessionInventoryAbort = null, authRotationPending = false;
  const sessionUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  const requestedSession = new URL(location.href).searchParams.get('session_id') || '';
  let notificationSessionID = sessionUUID.test(requestedSession) ? requestedSession.toLowerCase() : '';
  const activity = { socket: null, timer: null, generation: 0, revision: -1, sequence: -1, sessions: new Map(), attempts: 0, watchdog: null, ready: false };
  const sessionSettings = new Map(), activitySettingsRevisions = new Map();
  let settingsSerial = 0;
  let loadingOlder = false, suppressHistoryScroll = false;
  let questionRevision = 0, questionSnapshotSequence = 0;
  let questionRefresh = null;
  let turnTimes = new Map(), turnCursor = null;
  const imageDrafts = new Map();
  const sessionDeletions = new Map();
  let deleteSessionTarget = null;
  const maxImageBytes = 10 * 1024 * 1024, maxDraftImageBytes = 40 * 1024 * 1024;
  let imagePickerSession = null;
  let followLatest = true, bottomFrame = null;
  let lastHistoryScrollTop = 0, lastHistoryHeight = 0, lastHistoryViewport = 0;
  let fontSize = 14;
  try { const saved = Number(localStorage.getItem('codex-webui-font-size')); if (Number.isInteger(saved) && saved >= 12 && saved <= 22) fontSize = saved; } catch (_) { /* Storage can be disabled. */ }
  function setFontSize(value, persist = true) {
    const bottom = followLatest && atBottom();
    const scroller = $('transcript');
    const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    const offset = anchor?.getBoundingClientRect().top;
    fontSize = Math.max(12, Math.min(22, value));
    document.documentElement.style.setProperty('--desktop-font-scale', fontSize / 14);
    $('font-size').textContent = fontSize + 'px';
    $('font-decrease').disabled = fontSize === 12;
    $('font-increase').disabled = fontSize === 22;
    if (persist) { try { localStorage.setItem('codex-webui-font-size', String(fontSize)); } catch (_) { /* Optional preference only. */ } }
    resizePrompt(bottom);
    if (!bottom && anchor) scroller.scrollTop += anchor.getBoundingClientRect().top - offset;
  }
  $('font-decrease').addEventListener('click', () => setFontSize(fontSize - 1));
  $('font-increase').addEventListener('click', () => setFontSize(fontSize + 1));
  setFontSize(fontSize, false);
  let rateLimitSnapshot = null;
  let rateLimitRevision = 0;
  let rateLimitEpoch = 0;
  function clearRateLimits(text = '') {
    rateLimitSnapshot = null; rateLimitRevision++; rateLimitEpoch++;
    $('rate-limits').replaceChildren(document.createTextNode(text));
    $('rate-limits').removeAttribute('title');
  }
  function title(session) { return session.name || session.title || session.codex_thread_id || 'Untitled session'; }
  function notificationLocation(id = '') {
    const destination = '/tgw/webui/' + (id && sessionUUID.test(id) ? '?session_id=' + id.toLowerCase() : '');
    history.replaceState(null, '', destination);
    $('auth').querySelector('a').href = '/tgw/admin/?next=' + encodeURIComponent(destination);
  }
  function openNotification(url) {
    let destination;
    try { destination = new URL(url, location.origin); } catch (_) { return false; }
    if (destination.origin !== location.origin || destination.pathname !== '/tgw/webui/') return false;
    const id = destination.searchParams.get('session_id') || '';
    if (id && !sessionUUID.test(id)) return false;
    if (id) {
      notificationSessionID = id.toLowerCase(); notificationLocation(notificationSessionID);
      if (state.authenticated) refreshSessions();
    }
    return true;
  }
  let noticeRecovery = '';
  let steerNoticeTimer = null, steerNoticeTurn = null;
  function notice(text = '', recovery = '') {
    noticeRecovery = text ? recovery : '';
    $('notice').textContent = text; $('notice').hidden = !text;
  }
  function recoverNotice(...scopes) {
    // A recovered transport does not confirm an interrupted send or resolve
    // an unrelated Codex error. Clear only the operation that recovered.
    if (scopes.includes(noticeRecovery)) notice();
  }
  function clearSteerNotice() {
    clearTimeout(steerNoticeTimer); steerNoticeTimer = null; steerNoticeTurn = null;
    $('steer-notice').hidden = true; $('steer-notice').textContent = '';
  }
  function queuedSteer(turnId) {
    if (!state.connected || state.loading || !turnId || state.turn !== turnId) return;
    clearSteerNotice(); steerNoticeTurn = turnId;
    $('steer-notice').textContent = '✓ Message queued'; $('steer-notice').hidden = false;
    steerNoticeTimer = setTimeout(clearSteerNotice, 4000);
    settleBottom();
  }
  function connection(kind, text) { $('connection').dataset.state = kind; $('connection-text').textContent = text; $('reconnect').textContent = 'Reconnect'; $('reconnect').hidden = !state.selected || !['disconnected', 'error'].includes(kind); }
  function showSessions(open) {
    const mobile = matchMedia('(max-width:650px)').matches;
    if (mobile) blurEditable();
    document.body.classList.toggle('sessions-open', open);
    $('sidebar-backdrop').hidden = !open;
    $('show-sessions').setAttribute('aria-expanded', String(open));
    if (open) {
      (mobile ? $('close-sessions') : $('session-search')).focus({ preventScroll: true });
      if (state.authenticated) refreshSessions();
    }
  }
  function blurEditable() {
    const active = document.activeElement;
    if (active?.matches('input, textarea, select, [contenteditable="true"]')) active.blur();
  }
  function openSettings() {
    blurEditable();
    showSessions(false);
    const dialog = $('settings-dialog');
    if (!dialog.open) dialog.showModal();
    $('open-settings').setAttribute('aria-expanded', 'true');
    $('close-settings').focus({ preventScroll: true });
  }
  function sessionActivity(session) {
    const current = { ...session, ...activity.sessions.get(session.session_id) };
    const selected = state.selected?.session_id === session.session_id && state.connected && !state.loading;
    const localQuestions = selected ? [...state.questions.values()].filter(question => !question.answered).length : 0;
    const authoritative = activity.sessions.has(session.session_id);
    const pending = authoritative ? Number(current.pending_questions || 0) : Math.max(Number(current.pending_approvals || 0), localQuestions);
    const running = authoritative ? !!current.active_turn_id || isRunning(current.state) : selected ? !!state.turn : !!current.active_turn_id || isRunning(current.state);
    return { ...current, pending, running, kind: pending ? 'question' : running ? 'running' : 'idle' };
  }
  function updateSessionIndicators() {
    let pending = 0, running = 0;
    const buttons = new Map([...$('session-list').querySelectorAll('.session-button')].map(button => [button.dataset.sessionId, button]));
    for (const session of state.sessions) {
      if (session.archived || session.deleted) continue;
      const current = sessionActivity(session);
      pending += current.pending;
      if (current.running) running++;
      const button = buttons.get(session.session_id);
      if (!button) continue;
      button.dataset.activity = current.kind;
      const offline = current.worker_connectivity && !['connected', 'online'].includes(current.worker_connectivity);
      const status = current.pending ? '? Answer requested' : offline ? '○ Worker ' + current.worker_connectivity : current.running ? '● Working' : '○ ' + (isRunning(current.state) ? 'idle' : current.state || 'Saved');
      const detail = button.querySelector('.detail');
      const workspace = session.cwd || 'Workspace unavailable';
      if (detail.querySelector('.session-status-label')?.textContent !== status || detail.querySelector('.session-workspace')?.textContent !== workspace) {
        const label = node('span', 'session-status-label');
        if (status === '● Working') label.append(node('span', 'session-working-dot activity-dot', '●'), document.createTextNode(' Working'));
        else label.textContent = status;
        detail.replaceChildren(label, document.createTextNode(' · '), node('span', 'session-workspace', workspace));
      }
      detail.classList.toggle('running', current.running && !current.pending && !offline);
      button.setAttribute('aria-label', title(session) + ' · ' + status);
    }
    const menu = $('show-sessions');
    menu.dataset.activity = pending ? 'question' : running ? 'running' : 'idle';
    menu.textContent = pending ? '?' : '☰';
    menu.setAttribute('aria-label', pending ? 'Choose a session · ' + pending + ' pending questions or approvals' : running ? 'Choose a session · ' + running + ' sessions working' : 'Choose a session');
    menu.title = pending ? 'A session needs your answer' : running ? 'A session is working' : 'Choose a session';
  }
  function activityStatus(kind, text) {
    $('activity-status').dataset.state = kind;
    $('activity-status').textContent = text;
  }
  function settingsCheckpoint() { return sessionSettings.get(state.selected?.session_id)?.serial || 0; }
  function applySessionSettings(id, model, effort, source, revision = '') {
    if (!id || typeof model !== 'string' || typeof effort !== 'string') return;
    sessionSettings.set(id, { model, effort, source, revision, serial: ++settingsSerial });
    const session = state.sessions.find(value => value.session_id === id);
    if (session) session.stats = { ...session.stats, model, reasoning_effort: effort };
    if (state.selected?.session_id === id) {
      state.selected.stats = { ...state.selected.stats, model, reasoning_effort: effort };
      renderModels(); updateControls();
    }
  }
  function applyActivitySettings(row) {
    const revision = row.settings_revision;
    if (typeof revision !== 'string' || activitySettingsRevisions.get(row.session_id) === revision) return;
    if (revision && (!/^[1-9]\d*:[1-9]\d*$/.test(revision) || typeof row.model !== 'string' || typeof row.reasoning_effort !== 'string')) return;
    activitySettingsRevisions.set(row.session_id, revision);
    if (revision) applySessionSettings(row.session_id, row.model, row.reasoning_effort, 'activity', revision);
    else if (sessionSettings.get(row.session_id)?.source === 'activity') {
      // A changed runtime generation has no confirmed settings yet. Do not
      // display a previous generation's model as the current configuration.
      applySessionSettings(row.session_id, '', '', 'unconfirmed');
    }
  }
  function closeActivity() {
    activity.generation++; clearTimeout(activity.timer); clearTimeout(activity.watchdog);
    activity.timer = null; activity.watchdog = null; activity.ready = false;
    if (activity.socket) { activity.socket.onclose = null; activity.socket.onmessage = null; activity.socket.close(); activity.socket = null; }
  }
  function connectActivity() {
    if (!state.authenticated || document.hidden || activity.socket) return;
    clearTimeout(activity.timer); activity.timer = null;
    const generation = ++activity.generation;
    const url = new URL(api + '/activity', location.href); url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'; url.searchParams.set('v', '2');
    const socket = new WebSocket(url); activity.socket = socket; activity.sequence = -1; activity.revision = -1; activity.ready = false;
    activityStatus(activity.attempts ? 'reconnecting' : 'connecting', activity.attempts ? 'Reconnecting live updates…' : 'Connecting live updates…');
    const recover = async code => {
      if (activity.generation !== generation || activity.socket !== socket) return;
      closeActivity();
      if (code === 4001 || code === 4401) { expire(); return; }
      if (!state.authenticated || document.hidden) return;
      const ticket = activity.generation;
      activityStatus('reconnecting', 'Reconnecting live updates…');
      // Failed upgrades hide HTTP401 from browser WebSockets. Recheck auth
      // before reconnecting, without interrupting the selected conversation.
      try {
        const response = await fetch('/tgw/api/v1/admin/session', { credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10000) });
        if (ticket !== activity.generation) return;
        if (response.status === 401) { expire(); return; }
      } catch (_) { /* Keep last-known indicators during a network outage. */ }
      if (ticket !== activity.generation || !state.authenticated || document.hidden) return;
      activity.timer = setTimeout(connectActivity, Math.min(30000, 1000 * (2 ** Math.min(5, activity.attempts++))));
    };
    const watch = delay => {
      clearTimeout(activity.watchdog);
      activity.watchdog = setTimeout(() => recover(0), delay);
    };
    watch(20000);
    socket.onmessage = event => {
      if (activity.generation !== generation || !state.authenticated || activity.socket !== socket) return;
      let message; try { message = JSON.parse(event.data); } catch (_) { recover(0); return; }
      if (message.type === 'error') return;
      if (message.version !== 2 || !Number.isSafeInteger(message.sequence) || message.sequence < 0) { recover(0); return; }
      if (message.type === 'heartbeat') {
        if (!activity.ready || message.sequence !== activity.sequence) { recover(0); return; }
        watch(45000); return;
      }
      const previousPending = activity.sessions.get(state.selected?.session_id);
      const validRow = row => row && typeof row.session_id === 'string' && row.session_id.length > 0;
      let inventoryChanged = false;
      if (message.type === 'activity_snapshot') {
        if (!Array.isArray(message.sessions) || !message.sessions.every(validRow) || new Set(message.sessions.map(row => row.session_id)).size !== message.sessions.length) { recover(0); return; }
        if (activity.ready && message.sequence <= activity.sequence) return;
        activity.sessions = new Map(message.sessions.map(row => [row.session_id, row]));
        for (const row of message.sessions) applyActivitySettings(row);
        activity.ready = true;
        const known = new Set(state.sessions.map(row => row.session_id));
        inventoryChanged = known.size !== activity.sessions.size || [...activity.sessions.keys()].some(id => !known.has(id));
      } else if (message.type === 'activity_event') {
        if (!activity.ready) { recover(0); return; }
        if (message.sequence <= activity.sequence) return;
        if (message.sequence !== activity.sequence + 1 || !['turn_started', 'turn_ended', 'question_requested', 'question_resolved', 'session_changed', 'session_removed', 'session_settings_changed'].includes(message.event)) { recover(0); return; }
        if (message.event === 'session_removed') {
          if (typeof message.session_id !== 'string' || message.session !== null) { recover(0); return; }
          activity.sessions.delete(message.session_id);
          sessionSettings.delete(message.session_id); activitySettingsRevisions.delete(message.session_id);
          state.sessions = state.sessions.filter(row => row.session_id !== message.session_id);
          inventoryChanged = true;
        } else {
          if (!validRow(message.session) || message.session.session_id !== message.session_id) { recover(0); return; }
          inventoryChanged = !activity.sessions.has(message.session_id) || message.event === 'session_changed';
          activity.sessions.set(message.session_id, message.session);
          applyActivitySettings(message.session);
        }
      } else { recover(0); return; }
      activity.sequence = message.sequence; activity.revision = message.revision;
      activity.attempts = 0; watch(45000);
      activityStatus('live', '● Live updates');
      updateSessionIndicators();
      const nextPending = activity.sessions.get(state.selected?.session_id);
      if (state.connected && !state.loading && (previousPending?.pending_questions !== nextPending?.pending_questions || previousPending?.pending_revision !== nextPending?.pending_revision)) refreshQuestions();
      if (message.event === 'session_removed') renderSessions();
      if (inventoryChanged) refreshSessions();
    };
    socket.onclose = event => recover(event.code);
    socket.onerror = () => {};
  }
  function timestamp(value) {
    if (value === undefined || value === null) return null;
    const date = new Date(typeof value === 'number' && value < 1e12 ? value * 1000 : value);
    return Number.isNaN(date.getTime()) ? null : date;
  }
  function isRunning(value) { return ['inProgress', 'in_progress', 'running', 'active'].includes(typeof value === 'object' ? value?.type : value); }
  function atBottom() { const scroller = $('transcript'); return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 100; }
  function jump() {
    followLatest = true;
    const scroller = $('transcript');
    scroller.scrollTop = scroller.scrollHeight;
    lastHistoryScrollTop = scroller.scrollTop;
    lastHistoryHeight = scroller.scrollHeight; lastHistoryViewport = scroller.clientHeight;
    $('jump-latest').hidden = true;
  }
  function settleBottom() {
    if (!followLatest || bottomFrame !== null) return;
    const generation = state.generation;
    bottomFrame = requestAnimationFrame(() => {
      bottomFrame = null;
      if (generation === state.generation && followLatest && !loadingOlder && !state.loading && !$('transcript').hidden) jump();
    });
  }
  function resizePrompt(keepBottom = followLatest && atBottom()) {
    const prompt = $('prompt');
    if (!$('composer').hidden) {
      prompt.style.height = 'auto';
      prompt.style.height = Math.min(prompt.scrollHeight, 140) + 'px';
      if (keepBottom) jump();
    }
  }
  function updateControls() {
    if (steerNoticeTurn && (!state.connected || state.turn !== steerNoticeTurn)) clearSteerNotice();
    const available = state.connected && !state.loading && !state.submitting && !commandUI?.pending;
    const image = imageDrafts.get(state.selected?.session_id);
    const gatewayAllowed = !image && commandUI?.gatewayAllowed($('prompt').value);
    const sendAvailable = available || (gatewayAllowed && !state.submitting && !commandUI.pending);
    $('send').disabled = !sendAvailable || (!image && !$('prompt').value.trim()) || !!image?.loading;
    $('send').textContent = state.submitting ? 'Sending…' : state.turn && !gatewayAllowed ? 'Steer ↑' : 'Send ↑';
    $('stop').hidden = !state.turn;
    $('stop').disabled = !available;
    $('attach-image').disabled = !state.selected || state.submitting || !!image;
    $('remove-image').disabled = state.submitting;
    $('activity').hidden = !state.turn;
    $('turn-state').textContent = !state.connected ? 'Disconnected' : state.loading ? 'Loading' : state.turn ? 'Working' : 'Ready';
    $('turn-state').classList.toggle('running', !!state.turn);
    updateSessionIndicators();
    commandUI?.update();
  }
  function saveDraft() {
    if (state.selected) state.drafts.set(state.selected.session_id, $('prompt').value);
    syncRecoveryDrafts();
    resizePrompt();
    updateControls();
  }
  function renderImageDraft() {
    const image = imageDrafts.get(state.selected?.session_id);
    $('image-preview').hidden = !image;
    if (image) {
      $('image-thumbnail').src = image.url;
      $('image-description').textContent = image.file.name + ' · ' + Math.ceil(image.file.size / 1024).toLocaleString() + ' KB' + (image.loading ? ' · Loading…' : '');
    } else {
      $('image-thumbnail').removeAttribute('src'); $('image-description').textContent = '';
    }
    updateControls(); settleBottom();
  }
  function removeImage(sessionId, expected) {
    const image = imageDrafts.get(sessionId);
    if (!image || (expected && image !== expected)) return;
    imageDrafts.delete(sessionId); URL.revokeObjectURL(image.url);
    if (state.selected?.session_id === sessionId) renderImageDraft();
  }
  function attachImage(file, sessionId = state.selected?.session_id) {
    if (!sessionId || !state.authenticated || !file || state.submitting) return;
    if (imageDrafts.has(sessionId)) { notice('One image per message. Remove the attached image before choosing another.'); return; }
    if (!['image/png', 'image/jpeg', 'image/gif'].includes(file.type)) { notice('Choose a PNG, JPEG or GIF image.'); return; }
    if (!file.size || file.size > maxImageBytes) { notice('Images must be no larger than 10 MB.'); return; }
    if ([...imageDrafts.values()].reduce((sum, image) => sum + image.file.size, 0) + file.size > maxDraftImageBytes) { notice('Send or remove an image draft from another session before attaching more images.'); return; }
    const image = { file, url: URL.createObjectURL(file), loading: true };
    imageDrafts.set(sessionId, image); notice();
    const probe = new Image();
    const timer = setTimeout(() => finish('The image could not be opened. Try another PNG, JPEG or GIF.'), 15000);
    function finish(error = '') {
      clearTimeout(timer); probe.onload = null; probe.onerror = null;
      if (imageDrafts.get(sessionId) !== image) return;
      if (error) { removeImage(sessionId, image); if (state.selected?.session_id === sessionId) notice(error); }
      else { image.loading = false; if (state.selected?.session_id === sessionId) renderImageDraft(); }
    }
    probe.onload = () => finish(probe.naturalWidth > 8192 || probe.naturalHeight > 8192 || probe.naturalWidth * probe.naturalHeight > 16000000 ? 'Choose an image up to 16 megapixels and 8,192 pixels per side.' : '');
    probe.onerror = () => finish('The image could not be opened. Try another PNG, JPEG or GIF.');
    probe.src = image.url;
    if (state.selected?.session_id === sessionId) renderImageDraft();
  }
  function imageDataURL(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = reader.onabort = () => reject(new Error('The image could not be read. Your draft has been kept.'));
      reader.readAsDataURL(file);
    });
  }
  function rejectPending(message) {
    for (const request of state.pending.values()) { clearTimeout(request.timer); request.reject(new Error(message)); }
    state.pending.clear();
  }
  function closeSocket() {
    clearSteerNotice();
    loadingOlder = false; suppressHistoryScroll = false;
    if (bottomFrame !== null) { cancelAnimationFrame(bottomFrame); bottomFrame = null; }
    gatewayCommandAbort?.abort();
    clearTimeout(state.timer);
    state.timer = null;
    if (state.socket) { state.socket.onclose = null; state.socket.onmessage = null; state.socket.close(1000, 'Viewer detached'); state.socket = null; }
    rejectPending('Connection closed. No messages have been resent.');
    state.connected = false;
    state.loading = false;
    state.submitting = false;
    clearRateLimits();
  }
  function rpc(method, params = {}, timeout = 30000) {
    if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return Promise.reject(new Error('Reconnect before sending. Your draft has been kept.'));
    const id = 'web-' + (++state.sequence);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { state.pending.delete(id); reject(new Error('The request timed out. Check the conversation before retrying; it may have reached Codex.')); }, timeout);
      state.pending.set(id, { resolve, reject, timer });
      try { state.socket.send(JSON.stringify({ id, method, params })); }
      catch (error) { clearTimeout(timer); state.pending.delete(id); reject(error); }
    });
  }
  async function fetchJSON(path, signal = AbortSignal.timeout(15000)) {
    const authGeneration = state.authGeneration;
    const response = await fetch(path, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { if (authGeneration === state.authGeneration) expire(); throw new Error('Sign in with your passkey to continue.'); }
    if (!response.ok) throw new Error('Gateway request failed (' + response.status + '). Try again shortly.');
    return response.json();
  }
  function captureAuthPosition() {
    if (!state.selected) return;
    const scroller = $('transcript');
    const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    authRestore = { id: state.selected.session_id, top: scroller.scrollTop, bottom: followLatest, anchor: anchor?.dataset.itemId, offset: anchor ? anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top : 0, count: state.items.size };
    uncertainSend ||= state.submitting || uncertainDrafts.has(state.selected.session_id);
  }
  function expire() { if (sessionAuth) sessionAuth.expire('expired'); else lockAuthentication('expired'); }
  function lockAuthentication(reason) {
    captureAuthPosition();
    clearTimeout(draftSaveTimer); draftSaveTimer = null; draftDirty = false;
    if (['logout', 'revoked'].includes(reason)) draftRecovery?.clear();
    draftRecovery?.expireAuth(); recoveredDrafts = null; $('draft-recovery-offer').hidden = true;
    window.dispatchEvent(new CustomEvent('codex-auth-expiring'));
    commandUI?.sessionChanged();
    state.generation++;
    state.authGeneration++;
    resetInventory();
    for (const deletion of sessionDeletions.values()) { clearTimeout(deletion.timer); deletion.controller?.abort(); }
    sessionDeletions.clear(); deleteSessionTarget = null;
    if ($('delete-session-dialog').open) $('delete-session-dialog').close();
    state.stopped = true;
    closeSocket();
    state.authenticated = false;
    if ($('settings-dialog').open) $('settings-dialog').close();
    notificationUI?.setAuthenticated(false);
    closeActivity(); activity.sessions.clear(); activity.attempts = 0; activityStatus('disconnected', 'Sign in for live updates');
    sessionSettings.clear(); activitySettingsRevisions.clear();
    state.selected = null;
    state.sessions = [];
    state.drafts.clear();
    for (const image of imageDrafts.values()) URL.revokeObjectURL(image.url);
    imageDrafts.clear(); renderImageDraft(); imagePickerSession = null;
    state.positions.clear();
    state.items.clear();
    state.questions.clear();
    state.models = [];
    state.turn = null;
    state.cursor = null;
    state.queuedEvents = [];
    $('prompt').value = '';
    $('session-search').value = '';
    $('workspace-path').textContent = ''; $('workspace-path').removeAttribute('title');
    $('usage').textContent = '';
    $('model').textContent = 'Session model'; $('effort').textContent = 'Session effort';
    for (const id of ['delete-session-name', 'delete-session-cwd', 'delete-session-message']) $(id).textContent = '';
    $('image-file').value = '';
    $('messages').replaceChildren();
    $('questions').replaceChildren();
    $('questions').hidden = true;
    $('session-list').replaceChildren();
    $('session-title').textContent = 'Codex';
    $('session-location').textContent = 'Sign in to continue';
    $('sessions-status').textContent = 'Sign in to see your sessions.';
    for (const id of ['transcript', 'composer', 'empty', 'disconnect', 'jump-latest']) $(id).hidden = true;
    $('auth').hidden = false;
    $('auth-title').textContent = authRestore ? 'Unlock your sessions' : 'Sign in to Codex';
    $('auth-description').textContent = authRestore ? 'Your running work continues. Use your passkey to return to the same conversation.' : 'Use your gateway passkey to open your sessions.';
    showSessions(false);
    notice();
    connection('disconnected', 'Sign in required');
    updateControls();
  }
  function renderSessions() {
    const query = $('session-search').value.toLocaleLowerCase().trim();
    const list = $('session-list'); list.replaceChildren();
    const sessions = state.sessions.filter(session => !session.archived && (!query || [title(session), session.cwd, session.worker_name, session.runtime_name].some(value => String(value || '').toLocaleLowerCase().includes(query))));
    const groups = new Map();
    for (const session of sessions) {
      const name = JSON.stringify([session.worker_id, session.runtime_id, session.runtime_name]);
      if (!groups.has(name)) groups.set(name, []);
      groups.get(name).push(session);
    }
    for (const values of groups.values()) {
      const heading = node('h2', 'session-group');
      heading.append(node('span', 'worker-name', values[0].worker_name || values[0].worker_id));
      if (values[0].runtime_name) heading.append(node('span', 'runtime-name', values[0].runtime_name));
      list.append(heading);
      for (const session of values) {
        const row = node('div', 'session-row');
        const button = node('button', 'session-button'); button.type = 'button'; button.dataset.sessionId = session.session_id;
        button.setAttribute('aria-current', String(state.selected?.session_id === session.session_id));
        button.append(node('span', 'name', title(session)));
        const running = isRunning(session.state);
        const offline = session.worker_connectivity && !['connected', 'online'].includes(session.worker_connectivity);
        const status = offline ? '○ Worker ' + session.worker_connectivity : running ? '● Working' : '○ ' + (session.state || 'Saved');
        button.append(node('span', 'detail' + (running && !offline ? ' running' : ''), status + ' · ' + (session.cwd || 'Workspace unavailable')));
        button.addEventListener('click', () => selectSession(session));
        const remove = node('button', 'session-delete'); remove.type = 'button'; remove.dataset.deleteSessionId = session.session_id;
        remove.setAttribute('aria-label', 'Delete session ' + title(session)); remove.title = 'Delete session';
        remove.dataset.pending = String(!!sessionDeletions.get(session.session_id)?.pending);
        const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
        icon.setAttribute('viewBox', '0 0 24 24'); icon.setAttribute('fill', 'none'); icon.setAttribute('stroke', 'currentColor'); icon.setAttribute('stroke-width', '1.6'); icon.setAttribute('aria-hidden', 'true');
        const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', 'M4 7h16M9 7V4h6v3M6 7l1 14h10l1-14M10 10v7M14 10v7'); icon.append(path); remove.append(icon);
        remove.addEventListener('click', () => openSessionDeletion(session));
        row.append(button, remove); list.append(row);
      }
    }
    $('sessions-status').textContent = sessions.length ? sessions.length + (sessions.length === 1 ? ' session' : ' sessions') : query ? 'No matching sessions.' : 'No sessions yet. Create one from Telegram or Codex on your worker.';
    updateSessionIndicators();
  }
  function openSessionDeletion(session) {
    const current = state.sessions.find(value => value.session_id === session.session_id);
    if (!current || !state.authenticated) return;
    blurEditable();
    deleteSessionTarget = { ...current };
    $('delete-session-name').textContent = title(current);
    $('delete-session-cwd').textContent = current.cwd || 'Working directory unavailable';
    renderSessionDeletion();
    $('delete-session-dialog').showModal();
    $('delete-session-cancel').focus({ preventScroll: true });
  }
  function renderSessionDeletion() {
    if (!deleteSessionTarget) return;
    const deletion = sessionDeletions.get(deleteSessionTarget.session_id);
    const message = $('delete-session-message'), confirm = $('delete-session-confirm');
    message.textContent = deletion?.message || '';
    message.hidden = !message.textContent;
    message.dataset.error = String(['failed', 'unknown', 'retry'].includes(deletion?.phase));
    confirm.hidden = !!deletion?.deleted;
    confirm.disabled = deletion?.phase === 'submitting' || !!deletion?.checking;
    confirm.textContent = deletion?.phase === 'submitting' ? 'Deleting…' : deletion?.checking ? 'Checking…' : deletion?.pending || deletion?.phase === 'unknown' ? 'Check status' : deletion ? 'Retry deletion' : 'Delete session';
    $('delete-session-cancel').textContent = deletion ? 'Close' : 'Cancel';
    if (confirm.hidden && document.activeElement === confirm) $('delete-session-cancel').focus({ preventScroll: true });
  }
  function deletionIsCurrent(deletion) {
    return state.authenticated && deletion.authGeneration === state.authGeneration && sessionDeletions.get(deletion.session.session_id) === deletion;
  }
  function updateDeletionView(deletion) {
    if (deleteSessionTarget?.session_id === deletion.session.session_id) renderSessionDeletion();
    const button = [...$('session-list').querySelectorAll('.session-delete')].find(value => value.dataset.deleteSessionId === deletion.session.session_id);
    if (button) button.dataset.pending = String(!!deletion.pending);
  }
  async function deletionRequest(deletion, method) {
    const controller = new AbortController(); deletion.controller = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
    const path = api + '/sessions/delete' + (method === 'GET' ? '?' + new URLSearchParams({ session_id: deletion.session.session_id, request_id: deletion.requestId }) : '');
    try {
      const response = await fetch(path, { method, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(method === 'POST' ? { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ session_id: deletion.session.session_id, request_id: deletion.requestId, confirmed: true }) } : {}) });
      if (!deletionIsCurrent(deletion)) throw new Error('Deletion view expired.');
      if (response.status === 401) { expire(); throw new Error('Sign in to continue.'); }
      let result; try { result = await response.json(); } catch (_) { /* HTTP errors can have a plain response. */ }
      if (!deletionIsCurrent(deletion)) throw new Error('Deletion view expired.');
      if (!response.ok) {
        const error = new Error(typeof result?.message === 'string' ? result.message : response.status === 409 ? 'This session cannot be deleted now. Wait for its running work and pending questions to finish.' : 'Deletion request failed (' + response.status + ').');
        error.status = response.status; throw error;
      }
      if (result?.session_id !== deletion.session.session_id || result.command_id !== deletion.requestId || typeof result.deleted !== 'boolean' || typeof result.pending !== 'boolean' || (result.deleted && result.pending)) throw new Error('Unexpected deletion status.');
      return result;
    } finally {
      clearTimeout(timeout);
      if (deletion.controller === controller) deletion.controller = null;
    }
  }
  function forgetDeletedSession(id) {
    state.drafts.delete(id); state.positions.delete(id);
    const image = imageDrafts.get(id); if (image) { URL.revokeObjectURL(image.url); imageDrafts.delete(id); }
    state.sessions = state.sessions.filter(session => session.session_id !== id);
    activity.sessions.delete(id);
    sessionSettings.delete(id); activitySettingsRevisions.delete(id);
    if (state.selected?.session_id === id) {
      commandUI?.sessionChanged(); state.generation++; state.stopped = true; closeSocket(); state.selected = null;
      state.items.clear(); state.questions.clear(); state.models = []; state.turn = null; state.cursor = null; state.queuedEvents = [];
      $('messages').replaceChildren(); $('questions').replaceChildren(); $('questions').hidden = true; $('prompt').value = ''; renderImageDraft();
      for (const name of ['transcript', 'composer', 'disconnect', 'jump-latest']) $(name).hidden = true;
      $('empty').hidden = false; $('session-title').textContent = 'Codex'; $('session-location').textContent = 'Choose a session to continue';
      $('workspace-path').textContent = ''; $('usage').textContent = '';
      connection('connected', 'Session deleted · choose another session'); updateControls();
    }
    renderSessions(); refreshSessions();
  }
  function applyDeletionResult(deletion, result) {
    deletion.pending = result.pending; deletion.deleted = result.deleted;
    deletion.phase = result.deleted ? 'complete' : result.pending ? 'pending' : result.status === 'outcome_unknown' ? 'unknown' : 'failed';
    deletion.message = result.message || (result.deleted ? 'Session deleted. The working directory and all files were kept.' : result.pending ? 'Deletion requested. Waiting for the worker to confirm.' : 'The session could not be deleted.');
    if (result.deleted) forgetDeletedSession(deletion.session.session_id);
  }
  function scheduleDeletionCheck(deletion) {
    clearTimeout(deletion.timer);
    if (!deletionIsCurrent(deletion) || (!deletion.pending && deletion.phase !== 'unknown')) return;
    if (Date.now() >= deletion.deadline) {
      deletion.message = 'The deletion has not been confirmed yet. You can close this dialog and check its status again. It will not be submitted again automatically.';
      updateDeletionView(deletion); return;
    }
    deletion.timer = setTimeout(() => checkSessionDeletion(deletion), 1500);
  }
  async function checkSessionDeletion(deletion) {
    if (!deletionIsCurrent(deletion) || deletion.checking || deletion.phase === 'submitting') return;
    clearTimeout(deletion.timer); deletion.checking = true; updateDeletionView(deletion);
    try {
      const result = await deletionRequest(deletion, 'GET');
      if (!deletionIsCurrent(deletion)) return;
      applyDeletionResult(deletion, result);
    } catch (error) {
      if (!deletionIsCurrent(deletion)) return;
      if (error.status === 404) {
        deletion.pending = false; deletion.phase = 'retry';
        deletion.message = 'No deletion request is recorded. You can retry the same request, or close this dialog.';
      } else {
        deletion.phase = 'unknown'; deletion.message = 'Could not confirm deletion. The request may still be running. Check its status before retrying; it will not be submitted again automatically.';
      }
    } finally {
      deletion.checking = false;
      if (deletionIsCurrent(deletion)) { updateDeletionView(deletion); scheduleDeletionCheck(deletion); }
    }
  }
  async function confirmSessionDeletion() {
    if (!deleteSessionTarget || !state.authenticated) return;
    let deletion = sessionDeletions.get(deleteSessionTarget.session_id);
    if (deletion?.checking || deletion?.phase === 'submitting' || deletion?.deleted) return;
    if (deletion?.pending || deletion?.phase === 'unknown') { deletion.deadline = Date.now() + 120000; await checkSessionDeletion(deletion); return; }
    // Only this explicit confirmation submits a destructive request. If its
    // response was lost, subsequent requests read status; they never replay it.
    deletion = { session: { ...deleteSessionTarget }, requestId: deletion?.phase === 'retry' ? deletion.requestId : crypto.randomUUID(), authGeneration: state.authGeneration,
      phase: 'submitting', pending: true, deleted: false, deadline: Date.now() + 120000, message: 'Requesting deletion. The working directory and all files will be kept.' };
    sessionDeletions.set(deletion.session.session_id, deletion); updateDeletionView(deletion);
    try {
      const result = await deletionRequest(deletion, 'POST');
      if (!deletionIsCurrent(deletion)) return;
      applyDeletionResult(deletion, result);
    } catch (error) {
      if (!deletionIsCurrent(deletion)) return;
      const rejected = [400, 403, 404, 409].includes(error.status);
      deletion.pending = !rejected; deletion.phase = rejected ? 'failed' : 'unknown';
      deletion.message = rejected ? error.message : 'The connection was interrupted. The deletion may have been accepted. Checking its status; it will not be submitted again automatically.';
    } finally {
      if (deletionIsCurrent(deletion)) { updateDeletionView(deletion); scheduleDeletionCheck(deletion); }
    }
  }
  function resetInventory() {
    sessionInventoryAbort?.abort(); sessionInventoryAbort = null;
    state.refreshing = false; state.refreshAgain = false;
  }
  async function refreshSessions() {
    if (!state.authenticated) return;
    if (state.refreshing) { state.refreshAgain = true; return; }
    state.refreshAgain = false; state.refreshing = true;
    const authGeneration = state.authGeneration;
    const controller = new AbortController(); sessionInventoryAbort = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    $('refresh-sessions').disabled = true;
    try {
      const data = await fetchJSON(api + '/sessions', controller.signal);
      if (authGeneration !== state.authGeneration || !state.authenticated) return;
      state.sessions = (data.sessions || []).filter(session => !sessionDeletions.get(session.session_id)?.deleted);
      for (const session of state.sessions) {
        const settings = sessionSettings.get(session.session_id);
        if (settings) session.stats = { ...session.stats, model: settings.model, reasoning_effort: settings.effort };
      }
      state.authenticated = true;
      notificationUI?.setAuthenticated(true);
      $('auth').hidden = true;
      renderSessions();
      connectActivity();
      commandUI?.update();
      if (!state.selected) { $('empty').hidden = false; connection('connected', 'Gateway connected · choose a session'); }
      if (authRestore && !state.selected) {
        const target = state.sessions.find(session => session.session_id === authRestore.id && !session.archived && !session.deleted);
        if (target) { notificationSessionID = ''; selectSession(target); }
        else { authRestore = null; notice('Your previous session is no longer available. Choose another session.'); }
      }
      if (notificationSessionID) {
        const requested = notificationSessionID; notificationSessionID = '';
        const target = state.sessions.find(session => session.session_id === requested && !session.archived && !session.deleted);
        if (target) selectSession(target);
        else { notificationLocation(); notice('This notification’s session is no longer available. Choose another session.'); }
      }
    } catch (error) { if (authGeneration !== state.authGeneration) return; if (state.authenticated) $('sessions-status').textContent = error.message; else if ($('auth').hidden) connection('error', error.message); }
    finally {
      clearTimeout(timeout);
      if (sessionInventoryAbort === controller) sessionInventoryAbort = null;
      if (authGeneration !== state.authGeneration) return;
      state.refreshing = false; $('refresh-sessions').disabled = false;
      if (state.refreshAgain && state.authenticated) { state.refreshAgain = false; queueMicrotask(refreshSessions); }
    }
  }
  function selectSession(session) {
    showSessions(false);
    if (sessionUUID.test(session.session_id)) notificationLocation(session.session_id);
    if (state.selected?.session_id === session.session_id && state.connected) return;
    commandUI?.sessionChanged();
    if (state.selected) { saveDraft(); state.positions.set(state.selected.session_id, { top: $('transcript').scrollTop, bottom: atBottom() }); }
    state.generation++;
    closeSocket();
    state.selected = session;
    state.stopped = false;
    state.attempts = 0;
    state.items.clear();
    state.questions.clear();
    state.turn = null;
    state.cursor = null;
    state.models = [];
    state.queuedEvents = [];
    state.submitting = false;
    followLatest = true;
    loadingOlder = false;
    $('messages').replaceChildren();
    $('questions').replaceChildren(); $('questions').hidden = true;
    $('older').hidden = true;
    $('older').disabled = false;
    $('jump-latest').hidden = true;
    $('prompt').value = state.drafts.get(session.session_id) || '';
    renderImageDraft();
    $('session-title').textContent = title(session);
    $('session-location').textContent = [session.worker_name, session.runtime_name].filter(Boolean).join(' / ');
    $('workspace-path').textContent = session.cwd || '';
    $('workspace-path').title = session.cwd || '';
    $('usage').textContent = '';
    $('empty').hidden = true;
    $('transcript').hidden = false;
    $('composer').hidden = false;
    resizePrompt();
    $('disconnect').hidden = false;
    renderModels();
    renderSessions();
    notice();
    connect(state.generation);
  }
  function connect(generation) {
    if (generation !== state.generation || state.stopped || !state.selected) return;
    // Each socket attempt owns its callbacks, including automatic retries. A late
    // response from the previous transport cannot change the new session view.
    generation = ++state.generation;
    state.queuedEvents = [];
    state.loading = true;
    disableQuestions();
    connection(state.attempts ? 'reconnecting' : 'connecting', state.attempts ? 'Reconnecting… Your work continues on the worker.' : 'Connecting to ' + title(state.selected) + '…');
    updateControls();
    const url = new URL(api + '/connect', location.href); url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'; url.searchParams.set('session_id', state.selected.session_id);
    const socket = new WebSocket(url); state.socket = socket;
    const handshakeTimer = setTimeout(() => { if (state.socket === socket && !state.connected) socket.close(); }, 20000);
    socket.onmessage = event => {
      if (generation !== state.generation || state.socket !== socket) return;
      let message;
      try { message = JSON.parse(event.data); } catch (_) { notice('Received an unreadable update. Reconnect to refresh this conversation.', 'connection'); return; }
      if (message.type === 'ready') {
        clearTimeout(handshakeTimer);
        state.connected = true;
        state.attempts = 0;
        connection('loading', 'Connected · Restoring session…');
        updateControls();
        bootstrap(generation, socket).catch(error => { if (generation === state.generation && state.socket === socket) { closeSocket(); disableQuestions(); notice(error.message, 'connection'); connection('error', 'Could not load this session. Reconnect to try again.'); updateControls(); } });
        return;
      }
      if (message.type === 'error') { notice(message.message || 'Worker connection failed.', 'connection'); return; }
      if (message.id !== undefined && !message.method) {
        const request = state.pending.get(String(message.id));
        if (request) { clearTimeout(request.timer); state.pending.delete(String(message.id)); message.error ? request.reject(new Error(message.error.message || 'Codex rejected this request.')) : request.resolve(message.result); }
        else if (message.error && state.questions.has(String(message.id))) {
          state.questions.get(String(message.id)).answered = false;
          renderQuestions(); notice(message.error.message || 'The answer was rejected. Check whether this question is still pending.');
        }
        return;
      }
      if (state.loading) state.queuedEvents.push(message);
      else handleEvent(message);
    };
    socket.onclose = async event => {
      clearTimeout(handshakeTimer);
      if (generation !== state.generation || state.socket !== socket) return;
      state.connected = false; state.loading = false;
      clearRateLimits();
      rejectPending('Connection interrupted. No input was resent. Check the conversation before retrying.');
      updateControls();
      disableQuestions();
      if (event.code === 4001 || event.code === 4401) { expire(); return; }
      if (state.stopped) return;
      connection('reconnecting', 'Connection lost. Your draft is kept; running work continues.');
      try { await fetchJSON('/tgw/api/v1/admin/session'); } catch (_) { if (!state.authenticated) return; }
      if (generation !== state.generation || state.stopped) return;
      if (event.code === 1008) { connection('error', 'Connection refused. Refresh sessions or sign in again.'); return; }
      const delay = Math.min(30000, 1000 * (2 ** Math.min(5, state.attempts++))) + Math.floor(Math.random() * 300);
      state.timer = setTimeout(() => connect(generation), delay);
    };
    socket.onerror = () => { /* close is authoritative; never retry a prompt from this handler. */ };
  }
  async function restoreAuthPosition(generation) {
    if (!authRestore || authRestore.id !== state.selected?.session_id) return;
    const position = authRestore; authRestore = null;
    restoringPosition = true;
    try {
      if (position.bottom) { jump(); settleBottom(); }
      else {
        followLatest = false;
        const findAnchor = () => [...$('messages').children].find(item => item.dataset.itemId === position.anchor);
        // Keep only an item ID and geometry while locked. Load the same pages
        // again after authentication; no transcript is retained in storage.
        let pages = Math.ceil(position.count / 20);
        while (!findAnchor() && state.cursor && pages-- > 0 && generation === state.generation) {
          const cursor = state.cursor; await loadOlder(); if (cursor === state.cursor) break;
        }
        if (generation !== state.generation) return;
        const anchor = findAnchor(), scroller = $('transcript');
        scroller.scrollTop = anchor ? scroller.scrollTop + anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top - position.offset : position.top;
        $('jump-latest').hidden = atBottom();
      }
      if (uncertainSend) { notice('A previous send may have reached Codex. Check the conversation before sending it again. Nothing was resent.'); uncertainSend = false; }
      window.dispatchEvent(new CustomEvent('codex-auth-restored', { detail: { sessionId: state.selected.session_id } }));
    } finally { restoringPosition = false; }
  }
  function sessionConnectionReady() {
    if (!state.connected || state.loading || !state.selected) return;
    const questions = state.questionsStatus === 'loading' ? ' · Checking questions…' : state.questionsStatus === 'error' ? ' · Questions unavailable; reconnect to retry' : '';
    connection('connected', 'Connected · ' + title(state.selected) + questions);
  }
  async function bootstrap(generation, socket) {
    const threadId = state.selected.codex_thread_id;
    const current = () => generation === state.generation && state.socket === socket && state.connected;
    const settingsBeforeResume = settingsCheckpoint();
    const resume = await rpc('thread/resume', { threadId });
    if (!current()) return;
    const thread = resume?.thread || {};
    if (resume?.model && settingsCheckpoint() === settingsBeforeResume && sessionSettings.get(state.selected.session_id)?.source !== 'activity') {
      applySessionSettings(state.selected.session_id, resume.model, resume.reasoningEffort || '', 'resume');
    }
    connection('loading', 'Connected · Loading recent messages…');
    state.questions.clear();
    state.turn = null;
    turnTimes = new Map(); turnCursor = null;
    state.questionsStatus = 'loading';
    const [, metadata] = await Promise.all([
      rpc('thread/items/list', { threadId, limit: 20, sortDirection: 'desc' }).then(page => {
        if (!current()) return;
        const preserve = state.items.size > 0;
        const previous = { top: $('transcript').scrollTop, bottom: followLatest };
        state.items.clear();
        state.cursor = page.nextCursor || null;
        // The authoritative pending-question response owns restored questions;
        // historical async questions may already have been dismissed.
        for (const entry of [...(page.data || [])].reverse()) ingestHistoryEntry(entry, false);
        renderMessages(false);
        $('older').hidden = !state.cursor; $('older').disabled = true;
        connection('loading', 'Connected · Checking active turn…');
        if (authRestore?.id === state.selected?.session_id && !authRestore.bottom) { followLatest = false; }
        else if (!preserve || previous.bottom) { jump(); settleBottom(); }
        else { followLatest = false; $('transcript').scrollTop = previous.top; }
      }),
      rpc('thread/turns/list', { threadId, limit: 20, sortDirection: 'desc', itemsView: 'notLoaded' }),
    ]);
    if (!current()) return;
    turnCursor = metadata.nextCursor || null;
    rememberTurns(metadata.data || []);
    for (const item of state.items.values()) {
      const turn = turnTimes.get(item._turn);
      if (turn) { item._time ??= turn.startedAt; item._complete = !isRunning(turn.status); }
    }
    for (const turn of metadata.data || []) if (isRunning(turn.status)) state.turn = turn.id;
    // Resume can know a new turn before the lightweight state page catches up.
    for (const turn of thread.turns || []) if (isRunning(turn.status)) state.turn = turn.id;
    state.loading = false;
    recoverNotice('connection', 'history');
    for (const message of state.queuedEvents.splice(0)) handleEvent(message, true);

    renderMessages(false);
    renderQuestions();
    $('older').hidden = !state.cursor;
    $('older').disabled = false;
    sessionConnectionReady();
    updateUsage(state.selected.stats);
    updateControls();
    await restoreAuthPosition(generation);
    if (!current()) return;
    settleBottom();
    // Start this optional worker lookup only after the essential native reads
    // finish: older workers share its input lane with history fallback reads.
    // New live requests received after dispatch take precedence over a snapshot.
    const revision = questionRevision;
    rpc('gateway/questions', {}).then(snapshot => {
      if (!current()) return;
      state.questionsStatus = 'ready';
      if (questionRevision === revision) { applyQuestionSnapshot(snapshot); renderQuestions(); }
      else refreshQuestions();
      sessionConnectionReady();
    }).catch(() => {
      if (!current()) return;
      state.questionsStatus = 'error';
      sessionConnectionReady();
    });
    const limitRevision = rateLimitRevision;
    const limitEpoch = rateLimitEpoch;
    if (!rateLimitSnapshot) $('rate-limits').textContent = 'Loading limits…';
    rpc('account/rateLimits/read', {}).then(result => {
      if (!current() || limitEpoch !== rateLimitEpoch) return;
      const snapshot = result?.rateLimitsByLimitId?.codex || result?.rateLimits;
      updateRateLimits(!snapshot?.limitId || snapshot.limitId === 'codex' ? snapshot : null, limitRevision !== rateLimitRevision);
    }).catch(() => {
      if (current() && limitRevision === rateLimitRevision) clearRateLimits('Limits unavailable');
    });
    rpc('model/list', {}).then(result => {
      if (!current()) return;
      state.models = result?.data || result?.models || [];
      renderModels(); updateControls();
    }).catch(() => { /* Session defaults remain usable if this runtime has no model listing. */ });
  }
  function rememberTurns(turns) {
    for (const turn of turns) turnTimes.set(turn.id, { id: turn.id, startedAt: turn.startedAt, status: turn.status });
  }
  function ingestHistoryEntry(entry, trackQuestions = true) {
    if (!entry.item || !entry.turnId) return;
    const turn = { ...turnTimes.get(entry.turnId), id: entry.turnId };
    if (entry.turnStartedAt !== undefined) turn.startedAt = entry.turnStartedAt;
    if (entry.turnStatus) turn.status = entry.turnStatus;
    ingestItem(entry.item, turn, false, trackQuestions);
  }
  function ingestTurn(turn, prepend = false) {
    if (isRunning(turn.status)) state.turn = turn.id;
    for (const item of turn.items || []) ingestItem(item, turn, prepend);
  }
  function ingestItem(item, turn = {}, prepend = false, trackQuestions = true) {
    const key = item.id || 'item-' + (++state.sequence);
    const previous = state.items.get(key);
    const value = { ...previous, ...item, _turn: turn.id || previous?._turn, _time: item.createdAt ?? turn.startedAt ?? previous?._time, _complete: previous?._complete || !isRunning(turn.status) };
    if (prepend && !previous) state.items = new Map([[key, value], ...state.items]); else state.items.set(key, value);
    if (trackQuestions) trackAsyncQuestions(value);
  }
  function promptText(text) {
    let value = text.trimStart();
    // Codex history can prefix actual user input with recognized context wrappers.
    // Context-only items must not dismiss unanswered questions.
    for (let i = 0; i < 32; i++) {
      const wrapper = value.match(/^<(user_instructions|environment_context|skill|user_shell_command|turn_aborted|subagent_notification|recommended_plugins|goal_context|external_[A-Za-z0-9_]+)(?:\s[^>]*)?>/i);
      const end = wrapper ? '</' + wrapper[1] + '>' : value.toLowerCase().startsWith('# agents.md instructions') ? '</instructions>' : null;
      if (!end) break;
      const index = value.toLowerCase().indexOf(end.toLowerCase());
      if (index < 0) return '';
      value = value.slice(index + end.length).trimStart();
    }
    return value;
  }
  function trackAsyncQuestions(item) {
    if (item.type === 'agentMessage' && item.delivery === 'async' && item.questions?.length) {
      const id = 'async:' + item.id;
      if (!state.questions.has(id) && ![...state.questions.values()].some(request => request.params.itemId === item.id)) {
        questionRevision++;
        state.questions.set(id, { id, synthetic: true, answered: false, method: 'item/tool/requestUserInput', params: { turnId: item._turn, itemId: item.id, questions: item.questions.map((question, index) => ({ id: String(index), question: question.title, options: (question.options || []).map(option => typeof option === 'string' ? { label: option } : option) })) } });
      }
    } else if (item.type === 'userMessage') {
      const input = promptText(itemText(item));
      if (!input) return;
      const explicit = input.startsWith('> ') && input.includes('\n\n');
      questionRevision++;
      for (const [id, request] of state.questions) {
        if (!request.synthetic) continue;
        if (!explicit) { state.questions.delete(id); continue; }
        request.params.questions = request.params.questions.filter(question => {
          const prefix = '> ' + question.question.replaceAll('\n', '\n> ') + '\n\n';
          const index = input.indexOf(prefix);
          return index < 0 || (index > 0 && !input.slice(0, index).endsWith('\n\n')) || !input.slice(index + prefix.length).trim();
        });
        if (!request.params.questions.length) state.questions.delete(id);
      }
    }
  }
  function itemText(item) {
    if (item.type === 'reasoning') return Array.isArray(item.summary) ? item.summary.map(part => typeof part === 'string' ? part : typeof part?.text === 'string' ? part.text : '').filter(Boolean).join('\n') : '';
    if (typeof item.text === 'string') return item.text;
    if (Array.isArray(item.content)) return item.content.map(part => part.text || (part.type?.toLowerCase().includes('image') ? '[Image attachment]' : '')).filter(Boolean).join('\n');
    if (Array.isArray(item.summary)) return item.summary.map(part => typeof part === 'string' ? part : part.text || '').join('\n');
    return '';
  }
  function renderItem(item) {
    const type = item.type;
    const isUser = type === 'userMessage';
    const isAssistant = type === 'agentMessage';
    const isReasoning = type === 'reasoning';
    const reasoningLabel = item._turn && item._turn === state.turn && !item._complete ? 'Reasoning' : 'Reasoning complete';
    const isTool = !isUser && !isAssistant;
    const article = node('article', 'message ' + (isUser ? 'user' : isTool ? 'tool' : item.phase === 'commentary' ? 'commentary' : 'assistant'));
    if (isReasoning) article.classList.add('reasoning');
    article.dataset.itemId = item.id || '';
    const header = node('div', 'message-header'); header.append(node('span', 'role', isUser ? '› You' : isTool ? '• ' + (isReasoning ? reasoningLabel : 'Tool') : '• Codex'));
    const date = timestamp(item._time);
    if (date) { const time = node('time', '', date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })); time.dateTime = date.toISOString(); time.title = date.toLocaleString(); header.append(time); }
    article.append(header);
    if (isUser) article.append(node('div', 'plain', clean(itemText(item))));
    else if (isAssistant) article.append(rawView ? node('div', 'command-raw', clean(itemText(item))) : markdown(itemText(item)));
    else if (isReasoning) {
      // The native public summary is optional. Never substitute private
      // reasoning content, or create an expander with no summary to display.
      const summaryText = clean(itemText(item)).trim();
      if (summaryText) {
        const details = node('details'); const summary = node('summary');
        summary.append(node('span', 'tool-title', reasoningLabel));
        details.append(summary, markdown(summaryText)); article.append(details);
      }
      if (item.displayNotice) article.append(node('p', 'muted small', clean(item.displayNotice)));
    } else if (type === 'subAgentActivity') {
      // Native SubAgentActivity carries a lifecycle kind and target identity,
      // not tool output. Codex's TUI presents the same action as a plain line.
      const actions = { started: 'Started', interacted: 'Interacted with', interrupted: 'Interrupted', completed: 'Completed' };
      const action = Object.hasOwn(actions, item.kind) ? actions[item.kind] : 'Agent activity';
      const target = [item.agentPath, item.agentThreadId].find(value => typeof value === 'string' && clean(value).trim());
      const line = node('p', 'tool-title subagent-activity');
      line.append(document.createTextNode(action));
      if (target) line.append(document.createTextNode(' '), node('code', '', clean(target)));
      article.append(line);
      if (item.displayNotice) article.append(node('p', 'muted small', clean(item.displayNotice)));
    }
    else {
      const details = node('details');
      const summary = node('summary');
      let label = type || 'Activity';
      if (type === 'commandExecution') label = (item.status === 'completed' ? 'Ran ' : 'Running ') + (item.command || 'command');
      else if (type === 'fileChange') label = 'File changes · ' + ((item.changes || []).map(change => change.path).join(', ') || item.status || 'pending');
      else if (type === 'mcpToolCall') label = [item.server, item.tool].filter(Boolean).join(' / ');
      else if (type === 'webSearch') label = 'Search · ' + (item.query || item.action?.query || 'Web');
      else if (type === 'plan') label = 'Plan';
      summary.append(node('span', 'tool-title', clean(label).slice(0, 600))); details.append(summary);
      if (item.displayNotice) {
        details.append(node('p', 'muted small', clean(item.displayNotice)));
      } else if (type === 'fileChange') {
        for (const change of item.changes || []) { details.append(node('p', '', change.path || ''), code(change.diff || '', 'diff')); }
      } else if (type === 'plan') details.append(markdown(itemText(item)));
      else {
        const output = item.aggregatedOutput || item.output || item.result?.content?.map?.(part => part.text || '').join('\n') || itemText(item);
        if (output) {
          const text = typeof output === 'string' ? output : JSON.stringify(output, null, 2);
          if (text.length > 64000 || item._truncated) details.append(node('p', 'muted small', 'Showing the last 64,000 characters of tool output.'));
          details.append(code(text.slice(-64000)));
        }
        if (item.exitCode !== undefined && item.exitCode !== null) details.append(node('p', 'muted small', 'Exit code: ' + item.exitCode));
      }
      article.dataset.failed = String(item.status === 'failed' || (typeof item.exitCode === 'number' && item.exitCode !== 0));
      article.append(details);
    }
    return article;
  }
  function renderMessages(follow = true) {
    const bottom = followLatest;
    const top = $('transcript').scrollTop;
    const expanded = new Set([...$('messages').querySelectorAll('article:has(details[open])')].map(item => item.dataset.itemId));
    const fragment = document.createDocumentFragment();
    for (const item of state.items.values()) {
      const article = renderItem(item);
      if (expanded.has(article.dataset.itemId)) article.querySelector('details')?.setAttribute('open', '');
      fragment.append(article);
    }
    $('messages').replaceChildren(fragment);
    if (follow && bottom) jump();
    else { $('transcript').scrollTop = top; if (follow) $('jump-latest').hidden = false; }
  }
  function scheduleRender() {
    if (state.renderTimer) return;
    state.renderTimer = setTimeout(() => { state.renderTimer = null; if (state.selected) renderMessages(); }, 80);
  }
  function updateUsage(stats) {
    if (!stats) return;
    const context = stats.context_tokens ?? stats.last?.totalTokens;
    const maximum = stats.context_window ?? stats.modelContextWindow;
    $('usage').textContent = typeof context === 'number' && maximum > 0 ? Math.round(context / maximum * 100) + '% context' : '';
  }
  function updateRateLimits(snapshot, fillMissing = false) {
    // Codex's statusline uses the default bucket. Other model-specific buckets
    // cannot be reliably mapped to the selected model from this protocol.
    if (snapshot?.limitId && snapshot.limitId !== 'codex') {
      return;
    }
    if (snapshot) {
      rateLimitSnapshot = { ...rateLimitSnapshot };
      for (const [key, value] of Object.entries(snapshot)) {
        if (value !== null && value !== undefined && (!fillMissing || rateLimitSnapshot[key] === undefined)) rateLimitSnapshot[key] = value;
      }
    }
    rateLimitRevision++;
    const limits = $('rate-limits');
    limits.replaceChildren();
    const descriptions = [];
    for (const [key, fallback] of [['primary', 'Usage'], ['secondary', 'Secondary usage']]) {
      const window = rateLimitSnapshot?.[key];
      if (!window || !Number.isFinite(window.usedPercent)) continue;
      const duration = window.windowDurationMins;
      const label = [[300, '5h'], [1440, 'Daily'], [10080, 'Weekly'], [43200, 'Monthly'], [525600, 'Annual']].find(([minutes]) => Number.isFinite(duration) && Math.abs(duration - minutes) <= minutes * .05)?.[1] || fallback;
      const left = Math.round(Math.max(0, Math.min(100, 100 - window.usedPercent)));
      const span = node('span', 'rate-limit' + (left <= 10 ? ' exhausted' : left <= 25 ? ' low' : ''), label + ' ' + left + '% left');
      const reset = Number.isFinite(window.resetsAt) ? timestamp(window.resetsAt) : null;
      const description = label + ': ' + left + '% remaining' + (reset ? ' · resets ' + reset.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }) : '');
      span.title = description;
      span.setAttribute('aria-label', description);
      descriptions.push(description);
      limits.append(span);
    }
    if (!limits.childNodes.length) limits.textContent = 'Limits unavailable';
    limits.title = descriptions.join('\n') || 'This runtime has not reported account usage limits.';
  }
  function handleEvent(message, deferRender = false) {
    const { method, params: p = {} } = message;
    if (!method) return; // Transport heartbeats must not repaint an idle transcript.
    if (p.threadId && p.threadId !== state.selected?.codex_thread_id) return;
    if (p.turnId && p.delta && ['item/agentMessage/delta', 'item/reasoning/summaryTextDelta'].includes(method)) recoverNotice('retry:' + p.turnId);
    if (method === 'thread/settings/updated') {
      // Older workers can already relay this native notification to viewers.
      // Once the versioned activity feed has confirmed settings, its ordered
      // revisions are authoritative across all connections and sessions.
      const settings = p.threadSettings;
      if (p.threadId === state.selected?.codex_thread_id && settings && typeof settings.model === 'string' && (typeof settings.effort === 'string' || settings.effort === null) && sessionSettings.get(state.selected.session_id)?.source !== 'activity') {
        applySessionSettings(state.selected.session_id, settings.model, settings.effort || '', 'native');
      }
      return;
    }
    if (method === 'account/rateLimits/updated') { updateRateLimits(p.rateLimits); return; }
    if (message.id !== undefined && method) {
      questionRevision++;
      for (const [key, request] of state.questions) if (request.source === 'worker' && p.itemId && p.itemId === request.params.itemId && !request.synthetic) state.questions.delete(key);
      state.questions.set(String(message.id), { ...message, answered: false }); if (!deferRender) renderQuestions(); return;
    }
    if (method === 'turn/started') { state.turn = p.turn?.id || p.turnId; ingestTurn(p.turn || {}); }
    else if (method === 'turn/completed') {
      questionRevision++;
      if (!state.turn || state.turn === (p.turn?.id || p.turnId)) state.turn = null;

      if (p.turn?.error) notice(p.turn.error.message || 'The turn failed.');
      else if (['completed', 'interrupted'].includes(p.turn?.status)) recoverNotice('retry:' + (p.turn?.id || p.turnId));
      for (const [key, question] of state.questions) if (!question.synthetic && question.params?.turnId === (p.turn?.id || p.turnId)) { state.questions.delete(key); }
      renderQuestions();
    } else if (method === 'thread/status/changed') {
      if (p.status?.type === 'idle') { state.turn = null; }
    } else if (method === 'item/started' || method === 'item/completed') {
      if (p.item) { ingestItem({ ...p.item, _complete: method === 'item/completed' }, { id: p.turnId, startedAt: state.items.get(p.item.id)?._time || Date.now(), status: method === 'item/started' ? 'inProgress' : 'completed' }); renderQuestions(); }
    } else if (method === 'item/agentMessage/delta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'agentMessage', text: '', _turn: p.turnId };
      item.text = (item.text || '') + (p.delta || ''); state.items.set(p.itemId, item);
    } else if (method === 'item/commandExecution/outputDelta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'commandExecution', _turn: p.turnId };
      const output = (item.aggregatedOutput || '') + (p.delta || '');
      item._truncated = item._truncated || output.length > 64000;
      item.aggregatedOutput = output.slice(-64000); state.items.set(p.itemId, item);
    } else if (method === 'item/reasoning/summaryTextDelta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'reasoning', summary: [], _turn: p.turnId };
      const index = Number.isInteger(p.summaryIndex) && p.summaryIndex >= 0 && p.summaryIndex < 100 ? p.summaryIndex : 0;
      const summary = [...(item.summary || [])]; summary[index] = (summary[index] || '') + (p.delta || '');
      item.summary = summary; state.items.set(p.itemId, item);
    } else if (method === 'turn/diff/updated') {
      state.items.set('diff-' + p.turnId, { id: 'diff-' + p.turnId, type: 'fileChange', changes: [{ path: 'Turn diff', diff: p.diff || '' }] });
    } else if (method === 'turn/plan/updated') {
      state.items.set('plan-' + p.turnId, { id: 'plan-' + p.turnId, type: 'plan', text: [p.explanation, ...(p.plan || []).map(step => (step.status === 'completed' ? '✓ ' : step.status === 'inProgress' ? '› ' : '○ ') + step.step)].filter(Boolean).join('\n') });
    } else if (method === 'thread/tokenUsage/updated') { updateUsage(p.tokenUsage || p); return; }
    else if (method === 'serverRequest/resolved') { questionRevision++; state.questions.delete(String(p.requestId)); renderQuestions(); refreshQuestions(); return; }
    else if (method === 'error') {
      notice(p.error?.message || p.message || 'Codex reported an error.', p.willRetry === true && p.turnId && p.turnId === state.turn ? 'retry:' + p.turnId : '');
      return;
    }
    else return;
    updateControls();
    if (!deferRender) scheduleRender();
  }
  function disableQuestions() { for (const control of $('questions').querySelectorAll('button, input, textarea')) control.disabled = true; }
  function applyQuestionSnapshot(snapshot) {
    const pending = Array.isArray(snapshot?.questions) ? snapshot.questions : [];
    const retained = new Set(pending.map(request => 'worker:' + request.approvalId));
    const pendingItems = new Set(pending.filter(request => request.async && request.params?.itemId).map(request => request.params.itemId));
    for (const [id, request] of state.questions) {
      if (request.source === 'worker' && !retained.has(id)) state.questions.delete(id);
      else if (request.synthetic && request.source !== 'worker' && !pendingItems.has(request.params.itemId)) state.questions.delete(id);
    }
    for (const request of pending) {
      if (!request.approvalId || !request.params || !request.method) continue;
      const id = 'worker:' + request.approvalId;
      const duplicate = [...state.questions.entries()].find(([key, current]) => key !== id && request.params.itemId && current.params.itemId === request.params.itemId);
      // Live native requests are answered on this browser's connection. A
      // restored request is answered through its owning worker connection.
      if (duplicate && !duplicate[1].synthetic) continue;
      if (duplicate) state.questions.delete(duplicate[0]);
      const previous = state.questions.get(id);
      // Keep an in-flight answer attached to this exact request object. A
      // refresh must not strand a failed answer in a permanently disabled card.
      const current = previous || {};
      Object.assign(current, { id, source: 'worker', approvalId: request.approvalId, synthetic: !!request.async, method: request.method, params: request.params, answered: previous?.answered || false });
      state.questions.set(id, current);
    }
    questionRevision++;
  }
  async function refreshQuestions() {
    if (!state.connected || state.loading) return;
    const generation = state.generation, revision = questionRevision, sequence = ++questionSnapshotSequence;
    if (questionRefresh?.generation === generation) { questionRefresh.dirty = true; return; }
    const flight = { generation, dirty: false }; questionRefresh = flight;
    try {
      const snapshot = await rpc('gateway/questions', {});
      if (generation !== state.generation || sequence !== questionSnapshotSequence || revision !== questionRevision) return;
      applyQuestionSnapshot(snapshot); renderQuestions();
      state.questionsStatus = 'ready'; sessionConnectionReady();
    } catch (_) { /* Existing live requests remain usable after a missed refresh. */ }
    finally {
      if (questionRefresh === flight) {
        questionRefresh = null;
        if (flight.dirty && generation === state.generation) refreshQuestions();
      }
    }
  }
  async function sendAnswer(request, result) {
    if (!state.connected || state.loading || state.socket?.readyState !== WebSocket.OPEN || request.answered || state.questions.get(String(request.id)) !== request) return;
    const generation = state.generation;
    request.formDraft = new Map([...$('questions').querySelectorAll('textarea')].map(field => [field.name, field.value]));
    request.formChecked = new Set([...$('questions').querySelectorAll('input:checked')].map(field => field.name + '\0' + field.value));
    try {
      questionRevision++;
      request.answered = true;
      renderQuestions();
      if (request.synthetic) {
        const text = request.params.questions.map(question => '> ' + question.question.replaceAll('\n', '\n> ') + '\n\n' + (result.answers[question.id]?.answers || []).join('\n')).join('\n\n');
        const params = { threadId: state.selected.codex_thread_id, input: [{ type: 'text', text }] };
        const reply = state.turn ? await rpc('turn/steer', { ...params, expectedTurnId: state.turn }) : await rpc('turn/start', params);
        if (generation !== state.generation) return;
        if (reply?.turn?.id) state.turn = reply.turn.id;
        state.questions.delete(String(request.id)); renderQuestions(); updateControls();
      } else if (request.source === 'worker') {
        await rpc('gateway/answer', { approvalId: request.approvalId, result });
        if (generation !== state.generation) return;
        state.questions.delete(String(request.id)); renderQuestions(); updateControls();
      } else state.socket.send(JSON.stringify({ id: request.id, result }));
    } catch (error) {
      if (generation !== state.generation) return;
      request.answered = false; renderQuestions(); notice(error.message || 'The answer could not be sent. Reconnect and check whether the question is still pending.');
    }
  }
  function renderQuestions() {
    // Preserve typed answers across unrelated events; fields are rebuilt only when request membership changes.
    const active = $('questions').contains(document.activeElement) ? document.activeElement : null;
    const focused = active?.name ? { name: active.name, value: active.value, start: active.selectionStart, end: active.selectionEnd } : null;
    const drafts = new Map([...$('questions').querySelectorAll('textarea')].map(field => [field.name, field.value]));
    const checked = new Set([...$('questions').querySelectorAll('input:checked')].map(field => field.name + '\0' + field.value));
    $('questions').replaceChildren();
    for (const request of state.questions.values()) {
      const p = request.params || {};
      const card = node('form', 'question-card' + (request.answered ? ' answered' : ''));
      card.dataset.requestId = String(request.id);
      card.append(node('h2', '', request.method.includes('requestUserInput') ? 'Input requested · ' + title(state.selected) : 'Approval requested · ' + title(state.selected)));
      if (request.answered) { card.append(node('p', '', 'Response sent. Waiting for Codex to resolve this request.')); $('questions').append(card); continue; }
      if (request.method.includes('requestUserInput')) {
        const fields = [];
        for (const question of p.questions || []) {
          const group = node('fieldset'); const name = String(request.id) + ':' + question.id;
          group.append(node('legend', '', clean(question.question || question.header || 'Your answer')));
          for (const option of question.options || []) {
            const label = node('label'); const input = node('input'); input.type = 'radio'; input.name = name; input.value = option.label;
            const hasCurrentChoice = [...checked].some(value => value.startsWith(name + '\0'));
            input.checked = hasCurrentChoice ? checked.has(name + '\0' + option.label) : !!request.formChecked?.has(name + '\0' + option.label);
            const words = node('span', '', option.label + (option.description ? ' — ' + option.description : ''));
            label.append(input, words); group.append(label);
          }
          const text = node('textarea'); text.name = name; text.rows = 2; text.placeholder = question.options?.length ? 'Or type your answer…' : 'Type your answer…'; text.setAttribute('aria-label', question.question || question.header || 'Your answer'); text.value = drafts.get(name) ?? request.formDraft?.get(name) ?? '';
          group.append(text); card.append(group); fields.push({ question, group, text });
        }
        const actions = node('div', 'actions'); const send = node('button', 'primary', 'Send answer'); send.type = 'submit'; actions.append(send); card.append(actions);
        card.addEventListener('submit', event => {
          event.preventDefault(); const answers = {};
          for (const { question, group, text } of fields) {
            const answer = text.value.trim() || group.querySelector('input:checked')?.value;
            if (!answer) { text.focus(); notice('Answer each question before sending.'); return; }
            answers[question.id] = { answers: [answer] };
          }
          notice(); sendAnswer(request, { answers });
        });
      } else if (['item/commandExecution/requestApproval', 'item/fileChange/requestApproval'].includes(request.method)) {
        card.append(node('p', '', clean(p.reason || 'Codex needs your permission to continue.')));
        if (p.command) card.append(code(p.command));
        if (p.cwd) card.append(node('p', 'muted small', p.cwd));
        const actions = node('div', 'actions');
        const decisions = Array.isArray(p.availableDecisions) ? p.availableDecisions.filter(value => typeof value === 'string') : ['accept', 'decline'];
        const labels = { accept: 'Allow once', acceptForSession: 'Allow for session', decline: 'Decline', cancel: 'Cancel turn' };
        for (const decision of decisions) {
          if (!labels[decision]) continue;
          const button = node('button', decision === 'accept' ? 'primary' : 'quiet', labels[decision]); button.type = 'button'; button.addEventListener('click', () => sendAnswer(request, { decision })); actions.append(button);
        }
        card.append(actions);
      } else if (request.method === 'item/permissions/requestApproval') {
        card.append(node('p', '', clean(p.reason || 'Codex requests additional permissions for this turn.')));
        card.append(code(JSON.stringify(p.permissions || {}, null, 2), 'json'));
        const actions = node('div', 'actions');
        for (const [label, permissions] of [['Allow for this turn', p.permissions || {}], ['Decline', {}]]) {
          const button = node('button', label.startsWith('Allow') ? 'primary' : 'quiet', label); button.type = 'button';
          button.addEventListener('click', () => sendAnswer(request, { permissions, scope: 'turn' })); actions.append(button);
        }
        card.append(actions);
      } else {
        card.append(node('p', '', 'This request needs a control this web interface does not yet support. Open the session in Codex or Telegram to respond.'));
      }
      $('questions').append(card);
    }
    $('questions').hidden = !state.questions.size;
    if (!state.connected || state.loading) disableQuestions();
    if (focused) {
      const replacement = [...$('questions').querySelectorAll('input,textarea')].find(field => field.name === focused.name && (field.tagName === 'TEXTAREA' || field.value === focused.value));
      if (replacement && !replacement.disabled) { replacement.focus({ preventScroll: true }); if (focused.start !== null && replacement.setSelectionRange) replacement.setSelectionRange(focused.start, focused.end); }
    }
    updateSessionIndicators();
  }
  function currentModel() { return sessionSettings.get(state.selected?.session_id)?.model ?? state.selected?.stats?.model ?? ''; }
  function currentModelInfo() { return state.models.find(model => model.model === currentModel() || model.id === currentModel()); }
  function currentEffort() { const settings = sessionSettings.get(state.selected?.session_id); return settings ? settings.effort : state.selected?.stats?.reasoning_effort || currentModelInfo()?.defaultReasoningEffort || ''; }
  function reasoningOptions() {
    return (currentModelInfo()?.supportedReasoningEfforts || []).map(option => {
      const value = typeof option === 'string' ? option : option.reasoningEffort;
      return { value, label: value, description: typeof option === 'object' ? option.description : '' };
    }).filter(option => option.value);
  }
  function renderModels() { $('model').textContent = currentModel() || 'Session model'; renderEfforts(); }
  function renderEfforts() { const settings = sessionSettings.get(state.selected?.session_id); $('effort').textContent = currentEffort() || (settings && settings.source !== 'unconfirmed' ? 'Default' : 'Session effort'); settleBottom(); }
  async function send(event) {
    event.preventDefault();
    const text = $('prompt').value.trim();
    const image = imageDrafts.get(state.selected?.session_id);
    if (image && text.startsWith('/')) { notice('Send the image with a message, or remove it before using a / command.'); return; }
    if (text && commandUI?.handle(text)) return;
    if ((!text && !image) || image?.loading || !state.connected || state.loading || state.submitting) return;
    if (new TextEncoder().encode(text).length > 256 * 1024) { notice('This prompt is too long. Shorten it before sending.'); return; }
    const generation = state.generation;
    const sessionId = state.selected.session_id;
    const original = $('prompt').value, steerTurn = state.turn;
    state.submitting = true; updateControls(); notice();
    try {
      const input = text ? [{ type: 'text', text }] : [];
      if (image) input.push({ type: 'image', url: await imageDataURL(image.file) });
      if (generation !== state.generation || !state.connected) return;
      // Forget the old recovery key synchronously before dispatch. Even if
      // this send loses its acknowledgement, its text cannot be recovered
      // later as an apparently unsent draft. Other drafts get a fresh copy.
      uncertainDrafts.add(sessionId);
      if (draftRecovery?.enabled()) { draftRecovery.clear(); syncRecoveryDrafts(true); }
      const params = { threadId: state.selected.codex_thread_id, input };
      let result;
      if (steerTurn) result = await rpc('turn/steer', { ...params, expectedTurnId: steerTurn });
      else result = await rpc('turn/start', params);
      if (generation !== state.generation) return;
      if ($('prompt').value === original) { $('prompt').value = ''; state.drafts.delete(sessionId); uncertainDrafts.delete(sessionId); syncRecoveryDrafts(); resizePrompt(); }
      if (image) removeImage(sessionId, image);
      if (result?.turn?.id) { state.turn = result.turn.id; ingestTurn(result.turn); }
      if (steerTurn) queuedSteer(steerTurn);
      jump();
    } catch (error) { if (generation === state.generation) notice(error.message); }
    finally { if (generation === state.generation) { state.submitting = false; updateControls(); } }
  }
  async function loadOlder() {
    if (!state.cursor || !state.connected || state.loading || loadingOlder) return;
    const generation = state.generation;
    const scroller = $('transcript');
    followLatest = false;
    loadingOlder = true;
    $('older').disabled = true;
    try {
      const threadId = state.selected.codex_thread_id;
      const result = await rpc('thread/items/list', { threadId, cursor: state.cursor, limit: 20, sortDirection: 'desc' });
      if (generation !== state.generation) return;
      if (turnCursor && (result.data || []).some(entry => entry.turnStartedAt === undefined && !turnTimes.has(entry.turnId))) {
        const metadata = await rpc('thread/turns/list', { threadId, cursor: turnCursor, limit: 20, sortDirection: 'desc', itemsView: 'notLoaded' });
        if (generation !== state.generation) return;
        rememberTurns(metadata.data || []); turnCursor = metadata.nextCursor || null;
      }
      // The user can keep reading while the page is in flight. Anchor at the
      // current position, not where the request originally started.
      const previousHeight = scroller.scrollHeight, top = scroller.scrollTop;
      const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
      const anchorID = anchor?.dataset.itemId, anchorOffset = anchor?.getBoundingClientRect().top;
      const previous = state.items; state.items = new Map();
      for (const entry of [...(result.data || [])].reverse()) ingestHistoryEntry(entry, false);
      for (const [key, item] of previous) state.items.set(key, item);
      state.cursor = result.nextCursor || null;
      suppressHistoryScroll = true;
      renderMessages(false); $('older').hidden = !state.cursor;
      recoverNotice('history');
      if ($('reconnect').textContent === 'Reload recent messages') $('reconnect').hidden = true;
      const restored = [...$('messages').children].find(item => item.dataset.itemId === anchorID);
      scroller.scrollTop = restored && anchorOffset !== undefined ? top + restored.getBoundingClientRect().top - anchorOffset : top + scroller.scrollHeight - previousHeight;
      requestAnimationFrame(() => requestAnimationFrame(() => { suppressHistoryScroll = false; }));
    } catch (error) {
      if (generation === state.generation) {
        notice(error.message, 'history');
        $('reconnect').textContent = 'Reload recent messages'; $('reconnect').hidden = false;
      }
    } finally { if (generation === state.generation) { loadingOlder = false; $('older').disabled = false; } }
  }
  function disconnect() {
    saveDraft(); commandUI?.sessionChanged(); state.stopped = true; state.generation++;
    closeSocket(); disableQuestions(); connection('disconnected', 'Disconnected. Running work continues; your draft is kept.'); updateControls();
  }
  async function gatewayCommand(command, options = {}) {
    const readonly = command === 'tgstatus' || command === 'tginstances';
    const generation = state.generation;
    const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
    const controller = new AbortController(); gatewayCommandAbort = controller;
    const timeout = setTimeout(() => controller.abort(), 30000);
    let response;
    try {
      const parameters = { command, ...(options.includeSession !== false && state.selected ? { session_id: state.selected.session_id } : {}) };
      const url = api + '/commands' + (readonly ? '?' + new URLSearchParams(parameters) : '');
      response = await fetch(url, { method: readonly ? 'GET' : 'POST', credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(readonly ? {} : { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(parameters) }) });
      if (response.status === 401) { if (generation === state.generation) expire(); throw new Error('Sign in to continue.'); }
      if (!response.ok) throw new Error('Gateway command failed (' + response.status + '). Refresh the page and retry if necessary.');
      return await response.json();
    } catch (error) {
      if (response && !response.ok) throw error;
      if (readonly) throw new Error('Could not read the latest gateway status. Check your connection and try again.');
      throw new Error('The connection was interrupted or timed out. The command may have been accepted; check /tgstatus before retrying. It will not be resent automatically.');
    } finally { clearTimeout(timeout); if (gatewayCommandAbort === controller) gatewayCommandAbort = null; }
  }
  async function applyCommandResult(name, args, result, settingsBeforeCommand) {
    if (result.turn_id) { state.turn = result.turn_id; updateControls(); }
    if (result.session && ['new', 'fork'].includes(name)) {
      const generation = state.generation;
      // The worker's durable inventory event can arrive just after its command
      // reply. Wait for the gateway to authorize the new ID before attaching.
      for (let attempt = 0; attempt < 20; attempt++) {
        await refreshSessions();
        if (generation !== state.generation) return;
        const created = state.sessions.find(session => session.session_id === result.session.session_id);
        if (created) { selectSession(created); return; }
        await new Promise(resolve => setTimeout(resolve, 250));
      }
      notice('Session created. Refresh the session list to connect when its worker inventory arrives.');
    } else if (['archive', 'delete'].includes(name)) {
      disconnect(); state.selected = null; state.items.clear(); state.questions.clear();
      $('messages').replaceChildren(); $('questions').replaceChildren(); $('questions').hidden = true;
      for (const id of ['transcript', 'composer', 'disconnect', 'jump-latest']) $(id).hidden = true;
      $('empty').hidden = false; $('session-title').textContent = 'Codex'; $('session-location').textContent = 'Choose a session to continue';
      notice(result.text || 'Conversation removed from the session list.'); await refreshSessions();
    } else if (state.selected && name === 'rename') {
      state.selected.name = args;
      state.sessions = state.sessions.map(session => session.session_id === state.selected.session_id ? { ...session, name: args } : session);
      $('session-title').textContent = args; renderSessions();
      connection('connected', 'Connected · ' + args);
    } else if (state.selected && name === 'model' && args && !args.startsWith('--') && !result.model_menu) {
      const [model, effort] = args.split(/\s+/);
      if (sessionSettings.get(state.selected.session_id)?.source !== 'activity' && (settingsBeforeCommand === undefined || settingsBeforeCommand === settingsCheckpoint())) {
        const canonical = state.models.find(value => value.model === model || value.id === model)?.model || model;
        applySessionSettings(state.selected.session_id, canonical, effort || currentEffort(), 'command');
      }
    } else if (state.selected && name === 'reasoning' && args) {
      if (sessionSettings.get(state.selected.session_id)?.source !== 'activity' && (settingsBeforeCommand === undefined || settingsBeforeCommand === settingsCheckpoint())) applySessionSettings(state.selected.session_id, currentModel(), args, 'command');
    }
  }
  async function commandHistory(count) {
    const generation = state.generation, threadId = state.selected.codex_thread_id;
    let cursor = null; const found = [], seen = new Set();
    for (let page = 0; page < 20 && found.length < count; page++) {
      const result = await rpc('thread/turns/list', { threadId, limit: 20, sortDirection: 'desc', itemsView: 'full', ...(cursor ? { cursor } : {}) });
      if (generation !== state.generation) throw new Error('The selected session changed.');
      for (const turn of result.data || []) {
        for (const item of [...(turn.items || [])].reverse()) {
          if (!['userMessage', 'agentMessage'].includes(item.type) || item.phase === 'commentary') continue;
          const key = turn.id + ':' + item.id;
          if (seen.has(key)) continue; seen.add(key);
          const value = item.type === 'userMessage' ? promptText(itemText(item)) : itemText(item);
          if (!value.trim()) continue;
          const date = timestamp(item.createdAt ?? turn.startedAt);
          found.push({ role: item.type === 'userMessage' ? 'You' : 'Codex', text: value, time: date ? (item.createdAt ? '' : 'Turn time: ') + date.toLocaleString() : 'Date/time unavailable' });
          if (found.length === count) break;
        }
        if (found.length === count) break;
      }
      if (!result.nextCursor || result.nextCursor === cursor) break;
      cursor = result.nextCursor;
    }
    return found.reverse();
  }
  commandUI = window.CodexCommandUI.create({
    state, rpc, title, notice, queuedSteer, saveDraft, updateControls, showSessions, renderSessions, renderQuestions, selectSession, refreshSessions,
    currentModel, currentEffort, reasoningOptions, settingsCheckpoint,
    cancelGatewayCommand: () => gatewayCommandAbort?.abort(),
    composerHidden: () => $('composer').hidden, gatewayCommand, applyResult: applyCommandResult, disconnect,
    readHistory: commandHistory, setRaw: value => { rawView = value; renderMessages(false); },
    lastResponse: () => itemText([...state.items.values()].reverse().find(item => item.type === 'agentMessage' && item.phase !== 'commentary') || {}),
    exportText: () => '# ' + title(state.selected) + '\n\n' + [...state.items.values()].filter(item => ['userMessage', 'agentMessage'].includes(item.type)).map(item => '## ' + (item.type === 'userMessage' ? 'You' : 'Codex') + (timestamp(item._time) ? ' · ' + timestamp(item._time).toISOString() : '') + '\n\n' + clean(item.type === 'userMessage' ? promptText(itemText(item)) : itemText(item))).join('\n\n'),
  });
  $('show-sessions').addEventListener('click', () => showSessions(true));
  $('empty-sessions').addEventListener('click', () => showSessions(true));
  $('close-sessions').addEventListener('click', () => showSessions(false));
  $('sidebar-backdrop').addEventListener('click', () => showSessions(false));
  $('open-settings').addEventListener('click', openSettings);
  $('close-settings').addEventListener('click', () => $('settings-dialog').close());
  $('settings-dialog').addEventListener('close', () => {
    $('open-settings').setAttribute('aria-expanded', 'false');
    const target = !state.authenticated ? $('auth-login') : matchMedia('(max-width:650px)').matches ? $('show-sessions') : $('open-settings');
    if (target.getClientRects().length) target.focus({ preventScroll: true });
  });
  $('settings-dialog').addEventListener('click', event => {
    const dialog = $('settings-dialog');
    if (event.target !== dialog) return;
    const bounds = dialog.getBoundingClientRect();
    if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
  });
  $('session-search').addEventListener('input', renderSessions);
  $('refresh-sessions').addEventListener('click', refreshSessions);
  $('delete-session-confirm').addEventListener('click', confirmSessionDeletion);
  $('delete-session-cancel').addEventListener('click', () => $('delete-session-dialog').close());
  $('delete-session-dialog').addEventListener('close', () => {
    const id = deleteSessionTarget?.session_id; deleteSessionTarget = null;
    const button = [...$('session-list').querySelectorAll('.session-delete')].find(value => value.dataset.deleteSessionId === id);
    if (button && button.getClientRects().length) button.focus({ preventScroll: true });
    else if (matchMedia('(max-width:650px)').matches && $('show-sessions').getClientRects().length) $('show-sessions').focus({ preventScroll: true });
  });
  $('prompt').setAttribute('aria-controls', 'command-suggestions');
  $('prompt').setAttribute('aria-autocomplete', 'list');
  $('prompt').addEventListener('input', () => { uncertainDrafts.delete(state.selected?.session_id); saveDraft(); commandUI.changed(); });
  $('prompt').addEventListener('keydown', event => { if (commandUI.keydown(event)) return; if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && !matchMedia('(pointer: coarse)').matches) { event.preventDefault(); if (!$('send').disabled) $('composer').requestSubmit(); } });
  $('composer').addEventListener('submit', send);
  $('attach-image').addEventListener('click', () => { imagePickerSession = state.selected?.session_id; $('image-file').click(); });
  $('image-file').addEventListener('change', () => {
    const file = $('image-file').files[0]; $('image-file').value = '';
    if (imagePickerSession === state.selected?.session_id) attachImage(file, imagePickerSession);
    imagePickerSession = null;
  });
  $('remove-image').addEventListener('click', () => { if (!state.submitting) removeImage(state.selected?.session_id); });
  $('prompt').addEventListener('paste', event => {
    const files = [...(event.clipboardData?.items || [])].filter(item => item.kind === 'file').map(item => item.getAsFile()).filter(Boolean);
    if (!files.length) return;
    event.preventDefault();
    if (files.length !== 1) { notice('Paste one image per message.'); return; }
    attachImage(files[0]);
  });
  $('composer').addEventListener('dragover', event => { if ([...(event.dataTransfer?.types || [])].includes('Files')) event.preventDefault(); });
  $('composer').addEventListener('drop', event => {
    if (!event.dataTransfer?.files.length) return;
    event.preventDefault();
    if (event.dataTransfer.files.length !== 1) { notice('Attach one image per message.'); return; }
    attachImage(event.dataTransfer.files[0]);
  });
  $('stop').addEventListener('click', () => { const generation = state.generation; if (state.turn) rpc('turn/interrupt', { threadId: state.selected.codex_thread_id, turnId: state.turn }).catch(error => { if (generation === state.generation) notice(error.message); }); });
  $('older').addEventListener('click', loadOlder);
  $('jump-latest').addEventListener('click', jump);
  $('transcript').addEventListener('scroll', () => {
    const scroller = $('transcript'), top = scroller.scrollTop;
    const movingUp = top < lastHistoryScrollTop;
    const sameLayout = scroller.scrollHeight === lastHistoryHeight && scroller.clientHeight === lastHistoryViewport;
    lastHistoryScrollTop = top;
    lastHistoryHeight = scroller.scrollHeight; lastHistoryViewport = scroller.clientHeight;
    if (!suppressHistoryScroll) {
      if (scroller.scrollHeight - top - scroller.clientHeight <= 3) followLatest = true;
      // Focusing/removing a command choice can scroll the conversation while
      // its panel changes size. Explicit wheel/touch/key input still opts out
      // immediately; do not mistake menu focus scrolls for reading history.
      else if (movingUp && sameLayout && $('command-panel').hidden) followLatest = false;
    }
    if (atBottom()) $('jump-latest').hidden = true;
    if (movingUp && top <= 64 && !suppressHistoryScroll && !restoringPosition) loadOlder();
  }, { passive: true });
  $('transcript').addEventListener('wheel', event => { if (event.deltaY < 0) followLatest = false; }, { passive: true });
  $('transcript').addEventListener('keydown', event => { if (['ArrowUp', 'PageUp', 'Home'].includes(event.key)) followLatest = false; });
  let historyTouchY = null;
  $('transcript').addEventListener('touchstart', event => { historyTouchY = event.touches[0]?.clientY ?? null; }, { passive: true });
  $('transcript').addEventListener('touchmove', event => { if (historyTouchY !== null && event.touches[0]?.clientY > historyTouchY + 3) followLatest = false; }, { passive: true });
  $('disconnect').addEventListener('click', disconnect);
  function reconnect() { if (!state.selected) { refreshSessions(); return; } state.generation++; closeSocket(); state.stopped = false; state.attempts = 0; connect(state.generation); }
  $('reconnect').addEventListener('click', reconnect);
  document.addEventListener('visibilitychange', () => { if (document.hidden) { flushRecoveryDrafts(); closeActivity(); } });
  window.addEventListener('pagehide', closeActivity);
  navigator.serviceWorker?.addEventListener('message', event => {
    if (event.data?.type !== 'codex-notification-open') return;
    let source;
    try { source = new URL(event.source?.scriptURL); } catch (_) { return; }
    if (source.origin !== location.origin || source.pathname !== '/tgw/webui/sw.js') return;
    if (openNotification(event.data.url)) event.ports?.[0]?.postMessage({ type: 'notification-handled' });
  });
  document.addEventListener('keydown', event => { if (event.key === 'Escape' && !event.defaultPrevented && !document.querySelector('dialog[open]') && document.body.classList.contains('sessions-open')) { showSessions(false); $('show-sessions').focus({ preventScroll: true }); } });
  // Safari permits manual pinch despite viewport limits; keep one-finger
  // scrolling and text editing native, and cancel only zoom gestures.
  for (const type of ['gesturestart', 'gesturechange']) {
    document.addEventListener(type, event => event.preventDefault(), { passive: false });
  }
  // Use document coordinates for the absolute shell. Safari may move its
  // layout viewport as well as its visual viewport when focusing an input;
  // offsetTop alone misses that movement and fixed elements can be clipped.
  let viewportFrame = null;
  let viewportSettleUntil = 0;
  let viewportGeometry = null;
  // Font scaling and questions change footer height. Keep this control above
  // the actual footer rather than positioning it over the Send button.
  const footerObserver = new ResizeObserver(() => {
    if ($('transcript').hidden) return;
    const height = $('workspace').getBoundingClientRect().bottom - $('transcript').getBoundingClientRect().bottom;
    $('workspace').style.setProperty('--conversation-footer-height', Math.max(0, height) + 'px');
    settleBottom();
  });
  for (const id of ['transcript', 'messages', 'composer', 'questions', 'notice', 'command-panel']) footerObserver.observe($(id));
  function syncViewport() {
    viewportFrame = null;
    const viewport = window.visualViewport;
    const geometry = {
      top: Math.max(0, viewport?.pageTop ?? (window.scrollY + (viewport?.offsetTop || 0))),
      left: Math.max(0, viewport?.pageLeft ?? (window.scrollX + (viewport?.offsetLeft || 0))),
      width: viewport?.width || window.innerWidth,
      height: viewport?.height || window.innerHeight,
    };
    if (!viewportGeometry || Object.keys(geometry).some(key => geometry[key] !== viewportGeometry[key])) {
      const bottom = followLatest;
      const widthChanged = !viewportGeometry || geometry.width !== viewportGeometry.width;
      const heightChanged = !viewportGeometry || geometry.height !== viewportGeometry.height;
      const style = document.documentElement.style;
      for (const [key, value] of Object.entries(geometry)) {
        if (!viewportGeometry || value !== viewportGeometry[key]) style.setProperty('--viewport-' + key, value + 'px');
      }
      viewportGeometry = geometry;
      // Changing the textarea height during every keyboard-pan frame can
      // trigger another native caret scroll. Only remeasure when it can wrap.
      if (widthChanged) resizePrompt(bottom);
      else if (heightChanged && bottom) jump();
    }
    // WebKit can report old viewport metrics in the focus/resize event and
    // publish the final values later, without another event. Recheck briefly,
    // then stop completely; idle sessions must not run a polling loop.
    if (!document.hidden && performance.now() < viewportSettleUntil) scheduleViewport();
  }
  function scheduleViewport() {
    if (viewportFrame === null) viewportFrame = requestAnimationFrame(syncViewport);
  }
  function settleViewport(duration = 300) {
    viewportSettleUntil = Math.max(viewportSettleUntil, performance.now() + duration);
    scheduleViewport();
  }
  for (const type of ['resize', 'scroll']) {
    window.addEventListener(type, () => settleViewport(), { passive: true });
    window.visualViewport?.addEventListener(type, () => settleViewport(), { passive: true });
  }
  window.addEventListener('orientationchange', () => settleViewport(1000));
  document.addEventListener('focusin', () => settleViewport(1000));
  document.addEventListener('focusout', () => settleViewport(1000));
  document.addEventListener('visibilitychange', () => { if (!document.hidden) settleViewport(1000); });
  syncViewport();
  notificationUI = window.CodexNotifications.create({ expired: expire });
  if (notificationSessionID) notificationLocation(notificationSessionID);
  // Activity is pushed independently of the selected session. No history or
  // session-list polling runs while idle, and no transcript is persisted here.
  sessionAuth = window.CodexSessionAuth.create({
    mount: $('workspace'), isLocked: () => !state.authenticated && !$('auth').hidden,
    onExpired: lockAuthentication,
    onRenewing: busy => {
      $('auth-login').disabled = busy; $('auth-login').textContent = busy ? 'Waiting for passkey…' : 'Continue with passkey';
      if (!busy && authRotationPending) {
        authRotationPending = false;
        // A failed finish can leave either the old or new cookie in place.
        // Let the server confirm it before restoring either transport.
        sessionAuth.verify('renewal-recovery').then(value => {
          if (!value || !state.authenticated) return;
          connectActivity();
          if (state.selected && !state.connected && !state.loading && !state.stopped) reconnect();
        }).catch(error => notice(error.message, 'connection'));
      }
    },
    onBeforeRotate: () => {
      if (!state.authenticated) return;
      captureAuthPosition(); saveDraft();
      authRotationPending = true;
      state.authGeneration++; state.generation++; resetInventory();
      closeSocket(); closeActivity(); disableQuestions();
      connection('reconnecting', 'Renewing sign-in… Your running work continues.'); updateControls();
    },
    onStatus: text => { $('auth-status').textContent = text; },
    onWarning: warning => { if (warning) flushRecoveryDrafts(); },
    onAuthenticated: async () => {
      authRotationPending = false;
      const previouslyAuthenticated = state.authenticated;
      const authEpoch = state.authGeneration + 1;
      draftRecovery?.identityChanged();
      captureAuthPosition();
      state.authGeneration++; state.generation++;
      resetInventory();
      closeSocket(); closeActivity();
      state.authenticated = true; state.stopped = false;
      $('auth').hidden = true;
      // selectSession performs a fresh native bootstrap with the new login.
      // Preserve a still-authorized draft during early renewal only.
      if (state.selected) saveDraft();
      state.selected = null;
      await refreshSessions();
      if (authEpoch !== state.authGeneration || !state.authenticated) return;
      if (previouslyAuthenticated) syncRecoveryDrafts();
      else if (draftRecovery?.enabled()) {
        const recovered = await draftRecovery.recover();
        if (authEpoch !== state.authGeneration || !state.authenticated) return;
        recoveredDrafts = recovered; $('draft-recovery-offer').hidden = !recoveredDrafts;
      }
    },
    onVerified: (_session, { reason }) => {
      if (reason !== 'foreground' || !state.authenticated) return;
      refreshSessions(); connectActivity();
      if (state.selected && !state.connected && !state.loading && !state.stopped && (!state.socket || state.socket.readyState === 3)) reconnect();
    },
  });
  async function draftRequest(path, options = {}) {
    const authGeneration = state.authGeneration;
    const response = await fetch(path, { ...options, credentials: 'same-origin', cache: 'no-store', headers: { ...(options.headers || {}), 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') } });
    if (!response.ok) {
      if (response.status === 401 && authGeneration === state.authGeneration) expire();
      const error = new Error('Encrypted draft request failed (' + response.status + ').'); error.status = response.status; throw error;
    }
    return response.status === 204 ? null : response.json();
  }
  function flushRecoveryDrafts() {
    clearTimeout(draftSaveTimer); draftSaveTimer = null;
    if (!draftDirty || !draftRecovery?.enabled() || !state.authenticated || recoveredDrafts) return;
    draftDirty = false;
    draftRecovery.update(Object.fromEntries([...state.drafts].filter(([id]) => !uncertainDrafts.has(id))));
  }
  function syncRecoveryDrafts(immediate = false) {
    if (!draftRecovery?.enabled() || !state.authenticated || recoveredDrafts) return;
    draftDirty = true; clearTimeout(draftSaveTimer);
    if (immediate) flushRecoveryDrafts();
    else draftSaveTimer = setTimeout(flushRecoveryDrafts, 500);
  }
  draftRecovery = window.CodexDraftRecovery.create({ request: draftRequest, getIdentity: () => sessionAuth.snapshot()?.owner_id, onStatus: text => { $('draft-recovery-status').textContent = text; } });
  $('draft-recovery-enabled').checked = draftRecovery.enabled();
  $('draft-recovery-enabled').addEventListener('change', async event => {
    await draftRecovery.setEnabled(event.target.checked);
    if (!event.target.checked) { clearTimeout(draftSaveTimer); draftSaveTimer = null; draftDirty = false; recoveredDrafts = null; $('draft-recovery-offer').hidden = true; }
    else syncRecoveryDrafts(true);
  });
  $('restore-drafts').addEventListener('click', () => {
    if (!state.authenticated || !recoveredDrafts) return;
    const saved = recoveredDrafts; recoveredDrafts = null; $('draft-recovery-offer').hidden = true;
    const allowed = new Set(state.sessions.map(session => session.session_id));
    for (const [id, text] of Object.entries(saved)) if (allowed.has(id) && !uncertainDrafts.has(id) && !state.drafts.get(id)) state.drafts.set(id, text);
    if (state.selected && !$('prompt').value) { $('prompt').value = state.drafts.get(state.selected.session_id) || ''; resizePrompt(); updateControls(); }
    draftRecovery.consume();
  });
  $('discard-drafts').addEventListener('click', () => { recoveredDrafts = null; $('draft-recovery-offer').hidden = true; draftRecovery.clear(); });
  $('auth-login').addEventListener('click', () => sessionAuth.login().catch(() => {}));
  sessionAuth.verify('initial').catch(error => { lockAuthentication('unavailable'); $('auth-status').textContent = error.message; });
})();

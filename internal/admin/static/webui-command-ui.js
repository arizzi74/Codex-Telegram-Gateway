'use strict';
(() => {
  const { node, clean, markdown } = window.CodexFormat;
  const catalog = window.CodexWebCommands;
  const $ = id => document.getElementById(id);
  window.CodexCommandUI = { create(ctx) {
    const { state } = ctx;
    let matches = [], index = 0, inline = false, mode = '', epoch = 0, pending = false, pendingToken = 0;
    let choiceRows = [], choiceIndex = 0, choiceFocus = false, menuBack = null;
    let updateMonitor = null;
    let raw = false;
    const panel = $('command-panel'), content = $('command-content'), list = $('command-suggestions');
    const prompt = $('prompt'), search = $('command-search');
    function show(title, nextMode = 'result') {
      stopUpdateMonitor();
      epoch++; mode = nextMode;
      choiceRows = []; choiceIndex = 0; choiceFocus = false; menuBack = null;
      panel.hidden = false; $('command-title').textContent = title;
      $('show-commands').setAttribute('aria-expanded', 'true');
      content.replaceChildren(); list.replaceChildren();
      content.removeAttribute('role'); content.removeAttribute('aria-label');
      prompt.removeAttribute('aria-activedescendant'); search.removeAttribute('aria-activedescendant');
      search.hidden = nextMode !== 'list' || inline;
      list.hidden = nextMode !== 'list';
      content.hidden = nextMode === 'list';
      prompt.setAttribute('aria-expanded', String(nextMode === 'list' && inline));
      return epoch;
    }
    function close(focus = false) {
      if (focus && pending) return;
      stopUpdateMonitor();
      epoch++; mode = ''; panel.hidden = true;
      choiceRows = []; choiceFocus = false; menuBack = null;
      content.replaceChildren(); list.replaceChildren();
      prompt.removeAttribute('aria-activedescendant'); search.removeAttribute('aria-activedescendant');
      prompt.setAttribute('aria-expanded', 'false'); $('show-commands').setAttribute('aria-expanded', 'false');
      if (focus && !ctx.composerHidden()) prompt.focus({ preventScroll: true });
    }
    function text(value, error = false) {
      const item = node('p', 'command-text' + (error ? ' command-result-error' : ''), clean(value)); content.append(item); return item;
    }
    function stopUpdateMonitor() {
      const previous = updateMonitor; updateMonitor = null;
      if (!previous) return;
      clearTimeout(previous.timer);
      if (previous.reading) ctx.cancelGatewayCommand();
    }
    function monitorWorkerUpdates(result, output) {
      const targets = new Map((result.workers || []).filter(worker => worker.worker_id).map(worker => [worker.worker_id, worker.request_id || '']));
      if (!targets.size) return;
      const hint = text('Checking worker update progress automatically…'); hint.setAttribute('role', 'status');
      const monitor = { generation: state.generation, ticket: epoch, started: Date.now(), timer: null, reading: false, resume: null };
      updateMonitor = monitor;
      const current = () => updateMonitor === monitor && monitor.generation === state.generation && monitor.ticket === epoch && state.authenticated && !panel.hidden;
      const schedule = () => {
        if (!current() || document.hidden) return;
        const elapsed = Date.now() - monitor.started;
        monitor.timer = setTimeout(check, elapsed < 30000 ? 2000 : elapsed < 120000 ? 5000 : 15000);
      };
      monitor.resume = () => { if (!monitor.reading && current()) { clearTimeout(monitor.timer); monitor.timer = setTimeout(check, 0); } };
      async function check() {
        if (!current() || document.hidden) return;
        monitor.reading = true;
        try {
          const status = await ctx.gatewayCommand('tgstatus', { includeSession: false });
          if (!current()) return;
          if (status.error) throw new Error(status.error.message || 'Could not read worker update status.');
          const workers = new Map((status.workers || []).map(worker => [worker.worker_id, worker]));
          if ([...targets].some(([id, request]) => request && workers.get(id)?.request_id && workers.get(id).request_id !== request)) {
            hint.textContent = 'A newer worker update request replaced this status. Use Check worker update status to see the latest results.';
            monitor.reading = false; stopUpdateMonitor(); return;
          }
          output.textContent = clean(status.text || 'Worker update status is not available yet.');
          if ([...targets.keys()].every(id => ['completed', 'up_to_date', 'failed'].includes(workers.get(id)?.state))) {
            hint.textContent = [...targets.keys()].some(id => workers.get(id).state === 'failed') ? 'Update checks finished. See worker results above.' : 'All worker update checks finished.';
            monitor.reading = false; stopUpdateMonitor(); return;
          }
          schedule();
        } catch (error) {
          if (current()) {
            hint.textContent = 'Automatic status checks stopped. ' + clean(error.message) + ' Use Check worker update status to check again; the update request will not be resent.';
            monitor.reading = false; stopUpdateMonitor();
          }
        } finally { monitor.reading = false; }
      }
      schedule();
    }
    document.addEventListener('visibilitychange', () => {
      if (!updateMonitor) return;
      if (document.hidden) clearTimeout(updateMonitor.timer);
      else updateMonitor.resume();
    });
    function activateChoice(next, focus = false) {
      if (!choiceRows.length || panel.hidden) return;
      choiceIndex = Math.max(0, Math.min(next, choiceRows.length - 1));
      choiceRows.forEach((item, i) => { item.dataset.active = String(i === choiceIndex); item.tabIndex = i === choiceIndex ? 0 : -1; });
      if (focus && !pending) {
        choiceRows[choiceIndex].focus({ preventScroll: true });
        choiceRows[choiceIndex].scrollIntoView({ block: 'nearest' });
      }
    }
    function focusChoices() {
      if (!choiceFocus || pending || !choiceRows.length || panel.hidden) return;
      choiceFocus = false; activateChoice(choiceIndex, true);
    }
    function button(label, action, description = '', options = {}) {
      if (!choiceRows.length) {
        const hint = node('p', 'command-choice-hint', '↑ ↓ choose · Enter select · 1–9 select · Esc back or close');
        content.append(hint); content.setAttribute('role', 'group'); content.setAttribute('aria-label', $('command-title').textContent + ' choices');
      }
      const item = node('button', 'command-choice'); item.type = 'button';
      const pointer = node('span', 'command-choice-pointer', '›'); pointer.setAttribute('aria-hidden', 'true');
      const body = node('span', 'command-choice-body'); body.append(node('span', 'command-choice-label', label));
      if (description) body.append(node('small', '', description));
      const number = node('span', 'command-choice-number', String(choiceRows.length + 1)); number.setAttribute('aria-hidden', 'true');
      item.append(pointer, number, body);
      if (options.current) {
        item.setAttribute('aria-current', 'true');
        const marker = node('span', 'command-choice-current', '✓'); marker.setAttribute('aria-hidden', 'true'); marker.title = 'Current selection'; item.append(marker);
      }
      const generation = state.generation, ticket = epoch;
      item.addEventListener('click', () => {
        if (pending || generation !== state.generation || ticket !== epoch) return;
        action();
      });
      const position = choiceRows.length;
      item.addEventListener('focus', () => { if (generation === state.generation && ticket === epoch) activateChoice(position); });
      choiceRows.push(item); content.append(item);
      activateChoice(options.current || options.preferred ? position : choiceIndex);
      choiceFocus = true;
      queueMicrotask(() => { if (generation === state.generation && ticket === epoch) focusChoices(); });
      return item;
    }
    function consume(original) {
      if (prompt.value.trim() === original.trim()) { prompt.value = ''; ctx.saveDraft(); }
    }
    function unavailable(entry) {
      if (entry.kind === 'unavailable') return entry.reason;
      if (entry.kind === 'gateway' || ['help', 'tghelp', 'theme', 'title', 'tui', 'resume', 'agents', 'tgsessions'].includes(entry.name)) return state.authenticated ? '' : 'Sign in to use commands.';
      if (!state.selected) return 'Select a session first.';
      if (state.turn && !entry.busy) return 'Wait for the active turn to finish.';
      if (entry.kind === 'worker' || ['model', 'reasoning', 'new', 'clear', 'delete', 'tgdeletesession', 'tghistory', 'tglastmessages', 'tgsteer', 'tginterrupt'].includes(entry.name)) return state.connected && !state.loading ? '' : 'Connect to a session first.';
      return '';
    }
    function paintSuggestions() {
      list.replaceChildren();
      if (!matches.length) { list.append(node('p', 'muted', 'No matching commands.')); return; }
      const generation = state.generation, ticket = epoch;
      matches.forEach((entry, i) => {
        const reason = unavailable(entry);
        const item = node('button', 'command-option' + (reason ? ' unavailable' : '')); item.type = 'button'; item.id = 'slash-option-' + i;
        item.setAttribute('role', 'option'); item.setAttribute('aria-selected', String(i === index));
        item.append(node('span', 'command-name', '/' + entry.name), node('span', 'command-description', entry.description));
        if (reason) { item.title = reason; item.append(node('span', 'command-badge', entry.kind === 'unavailable' ? 'CLI only' : 'Unavailable')); }
        item.addEventListener('pointerdown', event => event.preventDefault());
        item.addEventListener('click', () => { if (!pending && generation === state.generation && ticket === epoch) choose(entry); }); list.append(item);
      });
      const field = inline ? prompt : search;
      field.setAttribute('aria-activedescendant', 'slash-option-' + index);
    }
    function suggestions(query) {
      matches = catalog.search(query); index = Math.min(index, Math.max(0, matches.length - 1)); paintSuggestions();
    }
    function open(query = '', fromPrompt = false) {
      if (pending) return;
      inline = fromPrompt; show('Commands · ↑ ↓ choose · Enter open · Tab complete', 'list');
      search.value = query; index = 0; suggestions(query);
      if (!inline) search.focus({ preventScroll: true });
    }
    function changed() {
      if (pending) return;
      const value = prompt.value;
      if (/^\/[\w-]*$/.test(value) && document.activeElement === prompt) {
        if (mode !== 'list' || !inline) open(value, true); else { index = 0; suggestions(value); }
      } else if (mode === 'list' && inline) close();
    }
    function choose(entry) {
      if (pending) return;
      const original = inline ? prompt.value : '';
      execute(entry.name, '', original);
    }
    function keydown(event) {
      if (event.isComposing || panel.hidden || event.defaultPrevented) return false;
      if (mode !== 'list') {
        if (event.key === 'Escape') {
          event.preventDefault(); event.stopPropagation();
          if (!pending) { if (menuBack) menuBack(); else close(true); }
          return true;
        }
        if (pending || !choiceRows.length || !panel.contains(event.target) || event.target.closest('input,textarea,select,[contenteditable=true]') || event.altKey || event.ctrlKey || event.metaKey) return false;
        const movement = ['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key);
        const number = /^[1-9]$/.test(event.key) ? Number(event.key) - 1 : -1;
        const enter = event.key === 'Enter' && !!event.target.closest('.command-choice');
        if (!movement && !enter && number < 0) return false;
        event.preventDefault(); event.stopPropagation();
        if (event.repeat && !movement) return true;
        if (movement) {
          const next = event.key === 'Home' ? 0 : event.key === 'End' ? choiceRows.length - 1 : (choiceIndex + (event.key === 'ArrowDown' ? 1 : -1) + choiceRows.length) % choiceRows.length;
          activateChoice(next, true);
        } else if (number < choiceRows.length) {
          if (number >= 0) activateChoice(number, true);
          choiceRows[choiceIndex].click();
        }
        return true;
      }
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End', 'Enter', 'Tab', 'Escape'].includes(event.key)) return false;
      event.preventDefault(); event.stopPropagation();
      if (event.key === 'Escape') { close(true); return true; }
      if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
        if (matches.length) { index = event.key === 'Home' ? 0 : event.key === 'End' ? matches.length - 1 : (index + (event.key === 'ArrowDown' ? 1 : -1) + matches.length) % matches.length; paintSuggestions(); $('slash-option-' + index)?.scrollIntoView({ block: 'nearest' }); }
        return true;
      }
      const entry = matches[index];
      if (!entry) return true;
      if (event.key === 'Tab' && inline) {
        prompt.value = '/' + entry.name + (entry.args ? ' ' : ''); ctx.saveDraft(); close(); prompt.focus({ preventScroll: true });
      } else choose(entry);
      return true;
    }
    function form(title, fields, submitLabel, action, back = null) {
      show(title); const form = node('form', 'command-form');
      menuBack = back;
      const inputs = {};
      for (const field of fields) {
        const label = node('label', '', field.label);
        const input = node(field.multiline ? 'textarea' : 'input');
        if (!field.multiline) input.type = 'text';
        input.value = field.value || ''; input.required = field.required !== false;
        input.maxLength = field.maxLength || 2000; input.autocomplete = 'off';
        label.append(input); form.append(label); inputs[field.key] = input;
      }
      const controls = node('div', 'command-actions');
      const submit = node('button', 'primary', submitLabel); submit.type = 'submit';
      const cancel = node('button', 'quiet', back ? 'Back' : 'Cancel'); cancel.type = 'button';
      controls.append(submit, cancel); form.append(controls); content.append(form);
      const generation = state.generation, ticket = epoch;
      cancel.addEventListener('click', () => { if (!pending && generation === state.generation && ticket === epoch) { if (back) back(); else close(true); } });
      form.addEventListener('submit', event => {
        event.preventDefault(); if (pending || generation !== state.generation || ticket !== epoch) return;
        action(Object.fromEntries(Object.entries(inputs).map(([name, input]) => [name, input.value.trim()])));
      });
      // Opening a command form is an explicit user action; retain the viewport.
      Object.values(inputs)[0]?.focus({ preventScroll: true });
    }
    function confirm(title, description, label, action) {
      show(title); text(description); button(label, action); button('Cancel', () => close(true), '', { preferred: true });
    }
    async function perform(title, action, apply) {
      if (pending) return;
      const ticket = show(title), generation = state.generation, operation = ++pendingToken;
      text('Working…'); pending = true; ctx.updateControls();
      try {
        const result = await action();
        if (generation !== state.generation || ticket !== epoch || !state.authenticated) return;
        content.replaceChildren();
        if (result?.error) throw new Error(result.error.message || 'The command failed.');
        await apply(result);
      } catch (error) {
        if (generation === state.generation && ticket === epoch) { content.replaceChildren(); text(error.message || 'The command failed. Nothing will be resent automatically.', true); }
      } finally { if (operation === pendingToken) { pending = false; ctx.updateControls(); focusChoices(); } }
    }
    function currentModelOption(args) {
      const parts = args.trim().split(/\s+/);
      if (parts[0] === '--menu' && parts.length === 2) parts.shift();
      else if (parts[0].startsWith('--')) return false;
      const model = ctx.currentModel?.() || state.selected?.stats?.model || '';
      const available = state.models.find(value => value.id === parts[0] || value.model === parts[0]);
      const matchesModel = parts[0] === model || available?.id === model || available?.model === model;
      return matchesModel && (!parts[1] || parts[1] === ctx.currentEffort?.());
    }
    function settingChange(name, args) {
      if (!args) return false;
      if (name === 'model') return !args.startsWith('--');
      if (name === 'fast') return args === 'on' || args === 'off';
      return ['reasoning', 'permissions', 'approvals', 'plan', 'personality', 'memories', 'goal', 'rename'].includes(name);
    }
    function worker(name, args = '', original = '') {
      const settingsCheckpoint = ctx.settingsCheckpoint?.();
      return perform('/' + name, () => ctx.rpc('gateway/command', { name, args }, 60000), async result => {
        consume(original);
        const generation = state.generation, ticket = epoch;
        await ctx.applyResult(name, args, result, settingsCheckpoint);
        if (generation !== state.generation || ticket !== epoch || !state.authenticated) return;
        if (result.session && ['new', 'fork'].includes(name)) { close(); return; }
        // Menus and confirmations remain until the worker accepts the final
        // selection. Closing without focus also keeps mobile keyboards closed.
        if (!result.permissions && !result.model_menu && settingChange(name, args)) { close(); return; }
        if (result.text) text(result.text);
        if (result.permissions) {
          const options = result.permissions.options || [];
          const confirmation = options.some(option => option.id === 'confirm-full-access');
          if (confirmation) menuBack = () => worker('permissions');
          for (const option of options) button(option.label, () => option.id === 'cancel' ? close(true) : worker('permissions', option.id), option.description,
            { current: option.current === true, preferred: confirmation && option.id === 'cancel' });
        }
        if (result.model_menu) {
          const options = result.model_menu.options || [];
          const reasoning = options.some(option => option.args === '--menu') || args.startsWith('--menu ') || (args && !args.startsWith('--') && options.some(option => option.args.startsWith(args + ' ')));
          if (reasoning) menuBack = () => worker('model');
          else if (/^--page [1-9]\d*$/.test(args)) menuBack = () => worker('model', '--page ' + (Number(args.split(' ')[1]) - 1));
          for (const option of options) button(option.label, () => option.args === '--cancel' ? close(true) : worker('model', option.args === '--menu' ? '' : option.args), '', { current: currentModelOption(option.args) });
          if (reasoning && !options.some(option => option.args === '--menu')) button('Back to models', menuBack);
        }
        if (!result.text && !result.permissions && !result.model_menu) text('Command completed.');
      });
    }
    function choices(name, description, values) {
      show('/' + name); text(description);
      for (const [args, label, details] of values) button(label, () => worker(name, args), details || '');
    }
    async function history(count) {
      return perform('Recent messages', () => ctx.readHistory(count), result => {
        if (!result.length) { text('No saved messages were returned.'); return; }
        for (const item of result) {
          const article = node('article', 'command-history');
          article.append(node('h3', '', item.role + ' · ' + item.time), node('div', 'plain', clean(item.text))); content.append(article);
        }
      });
    }
    function sessionChoices(name, query, page = 0) {
      show('/' + name + ' · Sessions');
      const filter = query.toLocaleLowerCase().trim();
      const sessions = state.sessions.filter(session => !session.archived && !session.deleted && (!filter ||
        [ctx.title(session), session.cwd, session.worker_name, session.runtime_name].some(value => String(value || '').toLocaleLowerCase().includes(filter))));
      const size = 15, pages = Math.max(1, Math.ceil(sessions.length / size));
      page = Math.min(page, pages - 1);
      if (!sessions.length) { text(filter ? 'No sessions match “' + query + '”.' : 'No saved sessions are available.'); button('Show all sessions', () => sessionChoices(name, '')); return; }
      text(sessions.length + (sessions.length === 1 ? ' session' : ' sessions') + (pages > 1 ? ' · Page ' + (page + 1) + ' of ' + pages : '') + '\nChoose the conversation to connect to.');
      for (const session of sessions.slice(page * size, (page + 1) * size)) {
        const location = [session.worker_name || session.worker_id, session.runtime_name].filter(Boolean).join(' / ');
        const details = [location, session.cwd, session.worker_connectivity && !['online', 'connected'].includes(session.worker_connectivity) ? 'Worker ' + session.worker_connectivity : ''].filter(Boolean).join('\n');
        button(ctx.title(session), () => {
          const current = state.sessions.find(value => value.session_id === session.session_id && !value.archived && !value.deleted);
          if (!current) { show('/' + name); text('This session is no longer available. Refresh the session list.'); return; }
          close(); ctx.selectSession(current);
        }, details, { current: state.selected?.session_id === session.session_id });
      }
      if (page > 0) { menuBack = () => sessionChoices(name, query, page - 1); button('Previous sessions', menuBack); }
      if (page + 1 < pages) button('Next sessions', () => sessionChoices(name, query, page + 1));
      button('Cancel', () => close(true));
    }
    function sessions(name, query) {
      return perform('/' + name, () => ctx.refreshSessions(), () => sessionChoices(name, query));
    }
    async function execute(name, args = '', original = '') {
      if (pending) return;
      const entry = catalog.find(name);
      if (!entry) { show('Unknown command'); text('/' + name + ' is not a recognized command. Nothing was sent to Codex.', true); return; }
      name = entry.name;
      const reason = unavailable(entry);
      if (reason) { show('/' + name); text(reason); return; }
      if (entry.kind === 'gateway') {
        if (args) { show('/' + name); text('Use /' + name + ' without arguments.', true); return; }
        return perform('/' + name, () => ctx.gatewayCommand(name), result => {
          consume(original); const output = text(result.text || 'Done.');
          if (name === 'tgupdateworkers') {
            monitorWorkerUpdates(result, output);
            button('Check worker update status', () => execute('tgstatus'));
          }
        });
      }
      if (['model', 'permissions', 'status', 'usage'].includes(name)) return worker(name, args, original);
      if (name === 'reasoning') {
        if (args) return worker(name, args, original);
        consume(original); show('/reasoning'); text('Choose the reasoning effort for subsequent turns.');
        const options = ctx.reasoningOptions?.() || [];
        for (const option of options.filter(option => option.value)) button(option.label, () => worker('reasoning', option.value), option.description || '', { current: option.value === ctx.currentEffort?.() });
        if (!options.length) button('Choose model and reasoning', () => worker('model'));
        return;
      }
      if (name === 'archive' || name === 'delete' || name === 'tgdeletesession') {
        consume(original); const action = name === 'archive' ? 'archive' : 'delete';
        const selected = state.selected;
        return confirm('/' + name, (action === 'delete' ? 'Permanently delete ' : 'Archive ') + ctx.title(selected) + '?\n' + (action === 'delete' ? 'Its conversation and child sessions will be removed. ' : '') + 'The working directory and files will be kept:\n' + selected.cwd,
          action === 'delete' ? 'Delete conversation' : 'Archive conversation', () => worker(action, action === 'delete' ? 'confirm ' + selected.codex_thread_id : ''));
      }
      if (name === 'new' || name === 'clear') {
        consume(original);
        return form('New session', [{ key: 'name', label: 'Session name', value: args, maxLength: 120 }, { key: 'cwd', label: 'Existing working directory on this worker', value: state.selected.cwd, maxLength: 4096 }], 'Create session', values => worker('new', JSON.stringify(values)));
      }
      if (name === 'rename' && !args) { consume(original); return form('/rename', [{ key: 'name', label: 'Session name', value: ctx.title(state.selected), maxLength: 120 }], 'Rename', values => worker('rename', values.name)); }
      if (['plan', 'fast', 'personality', 'memories', 'approvals'].includes(name) && !args) {
        consume(original);
        const presets = {
          plan: [['on', 'Plan mode'], ['off', 'Default mode']],
          fast: [['status', 'Show current tier'], ['on', 'Enable fast tier'], ['off', 'Use standard tier']],
          personality: [['friendly', 'Friendly'], ['pragmatic', 'Pragmatic'], ['none', 'No personality']],
          memories: [['enabled', 'Enable session memory'], ['disabled', 'Disable session memory']],
          approvals: [['', 'Show current approval policy'], ['untrusted', 'Ask for untrusted commands'], ['on-request', 'Ask when needed'], ['never', 'Never ask for approval']]
        };
        return choices(name, 'Choose a setting for this session. Changes apply to subsequent turns.', presets[name]);
      }
      if (name === 'goal' && !args) {
        consume(original); show('/goal'); text('Manage this session’s goal.');
        button('View goal', () => worker('goal'));
        button('Set goal', () => form('Set goal', [{ key: 'objective', label: 'Objective', multiline: true, maxLength: 10000 }], 'Set goal', values => worker('goal', values.objective), () => execute('goal')));
        for (const action of ['pause', 'resume', 'clear']) button(action[0].toUpperCase() + action.slice(1) + ' goal', () => worker('goal', action));
        return;
      }
      if (name === 'review' && !args) {
        consume(original); show('/review'); text('Choose what Codex should review.');
        button('Review uncommitted changes', () => worker('review'));
        button('Custom review instructions', () => form('/review', [{ key: 'instructions', label: 'Review instructions', multiline: true, maxLength: 10000 }], 'Start review', values => worker('review', values.instructions), () => execute('review')));
        return;
      }
      if (entry.kind === 'worker') return worker(name, args, original);
      consume(original);
      if (['help', 'tghelp'].includes(name)) { open(name === 'tghelp' ? 'tg' : ''); return; }
      if (['resume', 'agents', 'tgsessions'].includes(name)) return sessions(name, args);
      if (['quit', 'tgdisconnect'].includes(name)) { close(); ctx.disconnect(); return; }
      if (name === 'tginterrupt') {
        if (!state.turn) { show('/tginterrupt'); text('No turn is running in this session.'); return; }
        return perform('/tginterrupt', () => ctx.rpc('turn/interrupt', { threadId: state.selected.codex_thread_id, turnId: state.turn }), () => text('Interrupt requested.'));
      }
      if (name === 'tgsteer') {
        if (!state.turn) { show('/tgsteer'); text('There is no running turn to steer.', true); return; }
        if (!args) return form('/tgsteer', [{ key: 'text', label: 'Guidance for the running turn', multiline: true, maxLength: 200000 }], 'Send guidance', values => execute('tgsteer', values.text));
        const turnId = state.turn;
        return perform('/tgsteer', () => ctx.rpc('turn/steer', { threadId: state.selected.codex_thread_id, expectedTurnId: turnId, input: [{ type: 'text', text: args }] }), () => { close(); ctx.queuedSteer(turnId); });
      }
      if (name === 'tgquestions' || name === 'tginput') {
        close(); ctx.renderQuestions();
        if (state.questions.size) $('questions').querySelector('input,textarea,button')?.focus({ preventScroll: true });
        else { show('/' + name); text('No pending questions or approvals are visible in this session.'); }
        return;
      }
      if (name === 'tghistory' || name === 'tglastmessages') {
        const count = args ? Number(args) : name === 'tghistory' ? 2 : 1;
        if (!Number.isInteger(count) || count < 1 || count > 50) { show('/' + name); text('Choose a message count from 1 to 50.', true); return; }
        return history(count);
      }
      if (name === 'copy') {
        const last = ctx.lastResponse(); show('/copy');
        if (!last) { text('No Codex response is loaded.'); return; }
        try { await navigator.clipboard.writeText(last); text('Latest Codex response copied.'); } catch (_) { text('Select and copy the response below.'); content.append(node('pre', 'command-raw', last)); }
        return;
      }
      if (name === 'export') {
        show('/export'); text('Download the conversation currently loaded in this browser. Scroll to the top or use Load 20 earlier messages to include more history.');
        button('Download loaded conversation', () => {
          const data = ctx.exportText();
          const url = URL.createObjectURL(new Blob([data], { type: 'text/markdown;charset=utf-8' }));
          const link = node('a'); link.href = url; link.download = 'codex-conversation.md'; document.body.append(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
        }); return;
      }
      if (name === 'mention') {
        if (!args) return form('/mention', [{ key: 'path', label: 'File path relative to the session working directory', maxLength: 4096 }], 'Insert reference', values => execute('mention', values.path));
        close(); prompt.value = '@' + args + ' '; ctx.saveDraft(); prompt.focus({ preventScroll: true }); return;
      }
      if (name === 'raw') {
        if (args && !['on', 'off'].includes(args)) { show('/raw'); text('Use /raw [on|off].', true); return; }
        raw = args ? args === 'on' : !raw; ctx.setRaw(raw); close(); return;
      }
      if (name === 'theme') {
        if (!args) { show('/theme'); for (const value of ['dark', 'light', 'system']) button(value[0].toUpperCase() + value.slice(1), () => execute('theme', value), '', { current: value === (document.documentElement.dataset.theme || 'dark') }); return; }
        if (!['dark', 'light', 'system'].includes(args)) { show('/theme'); text('Choose dark, light, or system.', true); return; }
        document.documentElement.dataset.theme = args; close(); return;
      }
      if (name === 'title') {
        if (!args) return form('/title', [{ key: 'title', label: 'Browser tab title (or reset)', value: document.title, maxLength: 200 }], 'Set title', values => execute('title', values.title));
        document.title = args === 'reset' ? 'Codex · Gateway' : args; close(); return;
      }
      if (name === 'statusline') {
        if (!args) { show('/statusline'); const hidden = $('session-status').classList.contains('hide-details'); button('Show status line', () => execute('statusline', 'on'), '', { current: !hidden }); button('Hide status details', () => execute('statusline', 'off'), '', { current: hidden }); return; }
        if (!['on', 'off'].includes(args)) { show('/statusline'); text('Use /statusline [on|off].', true); return; }
        $('session-status').classList.toggle('hide-details', args === 'off'); close(); return;
      }
      if (name === 'tui') { show('Web interface controls'); text('Type / to find commands. Use ↑/↓ and Enter to choose, Tab to complete, Escape to close. Enter sends prompts on desktop; Shift+Enter inserts a newline. On phones, use Send. Use −/+ in the header for desktop text size. /theme, /raw and /statusline change this browser’s display.'); return; }
      show('/' + name); text('This command is not available in this browser. Nothing was sent.', true);
    }
    function handle(value) {
      const parsed = catalog.parse(value); if (!parsed) return false;
      if (!parsed.name) { open('', true); return true; }
      execute(parsed.name, parsed.args, value); return true;
    }
    function update() {
      if (mode === 'list') paintSuggestions();
      for (const button of content.querySelectorAll('button')) button.disabled = pending;
      $('show-commands').disabled = !state.authenticated || pending;
      $('close-commands').disabled = pending;
    }
    $('show-commands').addEventListener('click', () => open());
    $('close-commands').addEventListener('click', () => close(true));
    search.addEventListener('input', () => { index = 0; suggestions(search.value); });
    search.addEventListener('keydown', keydown);
    panel.addEventListener('keydown', keydown);
    return { open, close, handle, changed, keydown, update,
      gatewayAllowed(value) { const parsed = catalog.parse(value); return state.authenticated && catalog.find(parsed?.name)?.kind === 'gateway'; },
      get pending() { return pending; }, sessionChanged() { close(); pendingToken++; pending = false; ctx.cancelGatewayCommand(); } };
  } };
})();

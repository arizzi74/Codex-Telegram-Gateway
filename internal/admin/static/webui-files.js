/* Worker files are fetched only after an explicit action in their rendered session. */
'use strict';
window.CodexFiles = (() => {
  const maxBytes = 128 * 1024 * 1024;
  function create(adapter) {
    const $ = id => document.getElementById(id);
    const dialog = $('file-dialog');
    let active = null;
    const urls = new Map();
    const contextMatches = operation => {
      const context = adapter.getContext();
      return context.authenticated && context.sessionId === operation.sessionId && context.authGeneration === operation.authGeneration;
    };
    const current = operation => active === operation && dialog.open && contextMatches(operation) && (adapter.getContext().connected || operation.disconnected);
    function revoke(url) { clearTimeout(urls.get(url)); urls.delete(url); URL.revokeObjectURL(url); }
    function stop(operation) { clearTimeout(operation?.timeout); operation?.controller?.abort(); }
    function clear() {
      const operation = active; active = null; stop(operation);
      if (dialog.open) dialog.close();
      dialog.removeAttribute('aria-busy');
      for (const url of urls.keys()) revoke(url);
      $('file-name').textContent = ''; $('file-path').textContent = ''; $('file-size').textContent = ''; $('file-worker').textContent = ''; $('file-message').textContent = '';
      $('file-download').disabled = true; $('file-download').textContent = 'Download'; $('file-retry').hidden = true;
    }
    function message(text, error = false) {
      $('file-message').textContent = text; $('file-message').dataset.error = String(error);
    }
    function size(bytes) {
      if (bytes < 1024) return bytes + (bytes === 1 ? ' byte' : ' bytes');
      const unit = bytes < 1024 * 1024 ? 'KiB' : 'MiB';
      return (bytes / (unit === 'KiB' ? 1024 : 1024 * 1024)).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' ' + unit + ' (' + bytes.toLocaleString() + ' bytes)';
    }
    async function request(operation, download) {
      stop(operation);
      operation.controller = new AbortController();
      operation.timedOut = false;
      operation.timeout = setTimeout(() => { operation.timedOut = true; operation.controller.abort(); }, download ? 300000 : 30000);
      try {
        const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
        const response = await fetch('/tgw/api/v1/webui/files', {
          method: 'POST', credentials: 'same-origin', cache: 'no-store', signal: operation.controller.signal,
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
          body: JSON.stringify({ session_id: operation.sessionId, path: operation.path, download })
        });
        if (!current(operation)) return null;
        if (response.status === 401) { adapter.expired(); return null; }
        if (!response.ok) {
          const error = await response.json().catch(() => null);
          throw new Error(typeof error?.message === 'string' ? error.message : 'Could not retrieve this file (' + response.status + '). Try again.');
        }
        if (!download) return await response.json();
        const lengthHeader = response.headers.get('Content-Length');
        const length = lengthHeader !== null && /^\d+$/.test(lengthHeader) ? Number(lengthHeader) : NaN;
        if (!Number.isSafeInteger(length) || length < 0) throw new Error('The worker returned an invalid download size. Try again.');
        if (length > maxBytes) throw new Error('This file exceeds the 128 MiB download limit.');
        const reader = response.body?.getReader();
        if (!reader) throw new Error('The file download was incomplete. Try again.');
        const chunks = []; let received = 0;
        try {
          for (;;) {
            const part = await reader.read();
            if (!current(operation)) { await reader.cancel(); return null; }
            if (part.done) break;
            received += part.value.byteLength;
            if (received > maxBytes || received > length) {
              await reader.cancel();
              throw new Error(received > maxBytes ? 'This file exceeds the 128 MiB download limit.' : 'The worker returned an invalid download size. Try again.');
            }
            chunks.push(part.value);
          }
        } finally { reader.releaseLock(); }
        if (received !== length) throw new Error('The file download was incomplete. Try again.');
        return new Blob(chunks, { type: 'application/octet-stream' });
      } catch (error) {
        if (operation.timedOut) throw new Error('The file request timed out. Try again.');
        if (error.name === 'TypeError') throw new Error('Could not reach the worker. Check your connection and try again.');
        throw error;
      } finally { clearTimeout(operation.timeout); }
    }
    async function metadata(operation) {
      operation.ready = false;
      $('file-download').disabled = true; $('file-retry').hidden = true;
      message('Checking file on the worker…'); dialog.setAttribute('aria-busy', 'true');
      try {
        const result = await request(operation, false);
        if (!current(operation)) return;
        if (!result || typeof result.name !== 'string' || !result.name || typeof result.path !== 'string' || !result.path || !Number.isSafeInteger(result.size) || result.size < 0) throw new Error('The worker returned invalid file details. Try again.');
        $('file-name').textContent = result.name; $('file-path').textContent = result.path; $('file-size').textContent = size(result.size);
        operation.name = result.name; operation.ready = result.size <= maxBytes;
        $('file-download').disabled = !operation.ready;
        message(operation.ready ? 'Ready to download.' : 'This file exceeds the 128 MiB download limit.', !operation.ready);
      } catch (error) { if (current(operation)) { message(error.message, true); $('file-retry').hidden = false; } }
      finally { if (current(operation)) dialog.removeAttribute('aria-busy'); }
    }
    async function download() {
      const operation = active;
      if (!operation?.ready || !current(operation) || operation.downloading) return;
      operation.downloading = true; $('file-download').disabled = true; $('file-download').textContent = 'Downloading…';
      message('Downloading from the worker…'); dialog.setAttribute('aria-busy', 'true');
      try {
        const blob = await request(operation, true);
        if (!current(operation) || !blob) return;
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a'); link.href = url; link.download = operation.name.replace(/[\\/\x00-\x1f\x7f]/g, '_');
        link.hidden = true; document.body.append(link);
        try { link.click(); } finally { link.remove(); urls.set(url, setTimeout(() => revoke(url), 1000)); }
        message('Download started.');
      } catch (error) { if (current(operation)) message(error.message, true); }
      finally {
        operation.downloading = false;
        if (current(operation)) { $('file-download').disabled = !operation.ready; $('file-download').textContent = 'Download'; dialog.removeAttribute('aria-busy'); }
      }
    }
    $('messages').addEventListener('click', event => {
      const link = event.target.closest('a.file-link');
      if (!link || !$('messages').contains(link)) return;
      event.preventDefault();
      const article = link.closest('article.message'), context = adapter.getContext();
      if (!article?.dataset.fileSessionId || !context.authenticated || article.dataset.fileSessionId !== context.sessionId) return;
      clear();
      const operation = { sessionId: article.dataset.fileSessionId, authGeneration: context.authGeneration, path: link.dataset.filePath, origin: link, workerName: article.dataset.workerName || 'Worker', disconnected: !context.connected };
      active = operation;
      $('file-name').textContent = 'File'; $('file-path').textContent = operation.path; $('file-size').textContent = 'Checking…'; $('file-worker').textContent = operation.workerName;
      document.activeElement?.blur(); dialog.showModal(); $('file-close').focus({ preventScroll: true });
      if (operation.disconnected) { $('file-size').textContent = 'Unavailable'; message('Reconnect to this session to download files.', true); }
      else metadata(operation);
    });
    $('file-close').addEventListener('click', () => dialog.close());
    $('file-download').addEventListener('click', download);
    $('file-retry').addEventListener('click', () => { if (active && current(active)) metadata(active); });
    dialog.addEventListener('close', () => {
      // A delayed close event from a replaced dialog must not cancel its new request.
      if (dialog.open) return;
      const operation = active; active = null; stop(operation); dialog.removeAttribute('aria-busy');
      for (const url of urls.keys()) revoke(url);
      const origin = operation?.origin;
      const replacement = origin?.isConnected ? origin : [...$('messages').querySelectorAll('a.file-link')].find(link => link.dataset.filePath === operation?.path && link.closest('article')?.dataset.fileSessionId === operation?.sessionId);
      const target = contextMatches(operation || {}) ? replacement || adapter.focusFallback?.() : adapter.focusFallback?.();
      target?.focus({ preventScroll: true });
    });
    dialog.addEventListener('click', event => {
      if (event.target !== dialog) return;
      const bounds = dialog.getBoundingClientRect();
      if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
    });
    window.addEventListener('pagehide', clear);
    return { clear };
  }
  return { create };
})();

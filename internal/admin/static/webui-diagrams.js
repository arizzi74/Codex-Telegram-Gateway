/* Mermaid executes in an opaque-origin sandbox; only inert SVG images enter the chat. */
'use strict';
window.CodexDiagrams = (() => {
  const { node, code } = window.CodexFormat;
  const version = new URL(document.currentScript.src).searchParams.get('v');
  const encoder = new TextEncoder();
  const maxSource = 32 * 1024, maxSVG = 2 * 1024 * 1024, maxCache = 8 * 1024 * 1024;
  const cards = new Set(), cache = new Map();
  const colorScheme = matchMedia('(prefers-color-scheme: light)');
  let generation = 0, counter = 0, cacheBytes = 0;
  let frame = null, ready = null, readyResolve = null, readyReject = null, readyTimer = null;
  let request = null, queue = Promise.resolve();
  const theme = () => ['dark', 'light'].includes(document.documentElement.dataset.theme) ? document.documentElement.dataset.theme : colorScheme.matches ? 'light' : 'dark';

  function resetFrame() {
    clearTimeout(readyTimer);
    readyReject?.(new Error('Diagram rendering was interrupted.'));
    if (request) { clearTimeout(request.timer); request.reject(new Error('Diagram rendering was interrupted.')); }
    request = null; readyResolve = null; readyReject = null; ready = null;
    frame?.remove(); frame = null;
  }
  function renderer() {
    if (ready) return ready;
    const element = node('iframe', 'mermaid-renderer');
    element.title = 'Isolated diagram renderer'; element.tabIndex = -1;
    element.setAttribute('aria-hidden', 'true'); element.setAttribute('sandbox', 'allow-scripts');
    element.src = '/tgw/webui/diagram-renderer' + (version ? '?v=' + encodeURIComponent(version) : '');
    frame = element;
    ready = new Promise((resolve, reject) => { readyResolve = resolve; readyReject = reject; });
    readyTimer = setTimeout(() => { if (frame === element) resetFrame(); }, 15000);
    document.body.append(element);
    return ready;
  }
  window.addEventListener('message', event => {
    if (!frame || event.source !== frame.contentWindow || event.origin !== 'null') return;
    const data = event.data;
    if (!data || typeof data !== 'object') return;
    if (data.type === 'codex-mermaid-ready') {
      clearTimeout(readyTimer); readyResolve?.(); readyResolve = null; readyReject = null;
      return;
    }
    if (data.type !== 'codex-mermaid-result' || !request || data.id !== request.id) return;
    const pending = request; request = null; clearTimeout(pending.timer);
    if (typeof data.error === 'string') { pending.reject(new Error(data.error.slice(0, 200))); return; }
    if (typeof data.svg !== 'string' || encoder.encode(data.svg).byteLength > maxSVG ||
      !Number.isFinite(data.width) || !Number.isFinite(data.height) || data.width <= 0 || data.height <= 0 || data.width > 16384 || data.height > 16384) {
      pending.reject(new Error('Diagram output is too large or unsupported.')); return;
    }
    pending.resolve(data);
  });
  async function render(source, selectedTheme, epoch) {
    if (epoch !== generation) throw new Error('Diagram rendering was interrupted.');
    await renderer();
    if (epoch !== generation || !frame) throw new Error('Diagram rendering was interrupted.');
    return new Promise((resolve, reject) => {
      const id = 'diagram-' + (++counter);
      request = { id, resolve, reject, timer: setTimeout(resetFrame, 10000) };
      frame.contentWindow.postMessage({ type: 'codex-mermaid-render', id, source, theme: selectedTheme }, '*');
    });
  }
  function trimCache(bytes = 0, extra = 0) {
    for (const [key, entry] of cache) {
      if (cache.size + extra <= 32 && cacheBytes + bytes <= maxCache) break;
      if (!entry.done || [...cards].some(card => card.figure.isConnected && card.entry === entry)) continue;
      if (entry.url) URL.revokeObjectURL(entry.url);
      cacheBytes -= entry.bytes || 0; cache.delete(key);
    }
    return cache.size + extra <= 32 && cacheBytes + bytes <= maxCache;
  }
  function result(source, selectedTheme) {
    const key = selectedTheme + '\n' + source;
    if (cache.has(key)) { const value = cache.get(key); cache.delete(key); cache.set(key, value); return value; }
    if (!trimCache(0, 1)) throw new Error('Too many diagrams to display at once. View the source below.');
    const epoch = generation, entry = { done: false, bytes: 0 };
    cache.set(key, entry);
    const job = queue.then(() => render(source, selectedTheme, epoch));
    queue = job.catch(() => {});
    entry.promise = job.then(data => {
      if (epoch !== generation) throw new Error('Diagram rendering was interrupted.');
      const blob = new Blob([data.svg], { type: 'image/svg+xml' });
      if (!trimCache(blob.size)) throw new Error('Diagram cache is full. View the source below.');
      entry.url = URL.createObjectURL(blob); entry.bytes = blob.size; cacheBytes += blob.size;
      entry.width = data.width; entry.height = data.height;
      return entry;
    }).finally(() => { entry.done = true; });
    return entry;
  }
  function zoom(card) {
    if (!card.entry?.url) return;
    const base = Math.min(card.entry.width, card.viewport.clientWidth);
    card.image.style.width = card.scale === 1 ? '' : Math.max(1, base * card.scale) + 'px';
    card.image.style.maxWidth = card.scale === 1 ? '100%' : 'none';
    card.minus.disabled = card.scale <= .5;
    card.plus.disabled = card.scale >= 4;
    card.fit.disabled = false;
  }
  function error(card, message) {
    card.figure.dataset.state = 'error'; card.status.hidden = false;
    card.status.textContent = message; card.details.open = true; card.image.hidden = true;
    card.retry.hidden = card.disposed;
    for (const button of [card.minus, card.plus, card.fit]) button.disabled = true;
  }
  function keepReadingPosition(card, change) {
    const scroller = card.figure.closest('#transcript');
    const before = card.figure.getBoundingClientRect();
    const above = scroller && before.bottom <= scroller.getBoundingClientRect().top;
    change();
    if (above) scroller.scrollTop += card.figure.getBoundingClientRect().height - before.height;
  }
  async function display(card) {
    if (card.disposed || !card.figure.isConnected) return;
    const selectedTheme = theme(), epoch = generation;
    if (card.theme === selectedTheme) return;
    card.theme = selectedTheme;
    card.retry.hidden = true;
    try {
      const entry = result(card.source, selectedTheme); card.entry = entry;
      await entry.promise;
      if (epoch !== generation || card.disposed || !card.figure.isConnected || card.entry !== entry) return;
      card.image.onload = () => {
        if (!card.disposed && card.entry === entry) keepReadingPosition(card, () => { card.status.hidden = true; card.figure.dataset.state = 'ready'; });
      };
      card.image.onerror = () => { if (!card.disposed && card.entry === entry) error(card, 'This browser could not display the diagram. View its source below.'); };
      keepReadingPosition(card, () => {
        card.image.width = entry.width; card.image.height = entry.height;
        card.image.src = entry.url; card.image.hidden = false;
        card.details.open = card.sourceOpen;
        zoom(card);
      });
    } catch (failure) {
      if (epoch === generation && !card.disposed && card.figure.isConnected && card.theme === selectedTheme) error(card, failure.message);
    }
  }
  const visible = new IntersectionObserver(entries => {
    for (const entry of entries) if (entry.isIntersecting) {
      const card = [...cards].find(value => value.figure === entry.target);
      if (card) { card.visible = true; display(card); }
      visible.unobserve(entry.target);
    }
  }, { rootMargin: '300px' });
  const dimensions = new ResizeObserver(entries => {
    for (const entry of entries) {
      const card = [...cards].find(value => value.viewport === entry.target);
      if (card) zoom(card);
    }
  });
  function track() {
    for (const card of cards) {
      if (card.disposed) continue;
      if (card.figure.isConnected && !card.attached) {
        card.attached = true; visible.observe(card.figure); dimensions.observe(card.viewport);
      } else if (!card.figure.isConnected) {
        visible.unobserve(card.figure); dimensions.unobserve(card.viewport); card.disposed = true; cards.delete(card);
      }
    }
  }
  new MutationObserver(track).observe(document.body, { childList: true, subtree: true });
  function refreshTheme() {
    for (const card of cards) if (card.visible && !card.disposed) display(card);
  }
  new MutationObserver(refreshTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  colorScheme.addEventListener('change', refreshTheme);

  function create(source) {
    const figure = node('figure', 'mermaid-diagram'); figure.dataset.state = 'loading';
    const toolbar = node('figcaption', 'diagram-toolbar'); toolbar.append(node('span', 'diagram-label', 'Mermaid'));
    const controls = node('div', 'diagram-controls');
    const button = (label, text) => { const value = node('button', '', text); value.type = 'button'; value.title = label; value.setAttribute('aria-label', label); value.disabled = true; controls.append(value); return value; };
    const minus = button('Zoom out diagram', '−'), plus = button('Zoom in diagram', '+'), fit = button('Fit diagram', 'Fit');
    const retry = button('Retry diagram', 'Retry'); retry.hidden = true; retry.disabled = false;
    toolbar.append(controls);
    const status = node('p', 'diagram-status', 'Rendering diagram…'); status.setAttribute('role', 'status');
    const viewport = node('div', 'diagram-viewport'), image = node('img', 'mermaid-image');
    image.alt = 'Mermaid diagram'; image.hidden = true; image.draggable = false;
    viewport.append(image);
    const details = node('details', 'mermaid-source'), summary = node('summary', '', 'Source');
    details.open = true; details.append(summary, code(source));
    figure.append(toolbar, status, viewport, details);
    const card = { figure, viewport, image, details, status, minus, plus, fit, retry, source, scale: 1, sourceOpen: false };
    summary.addEventListener('click', () => { card.sourceOpen = !details.open; });
    minus.addEventListener('click', () => { card.scale = Math.max(.5, card.scale / 1.5); zoom(card); });
    plus.addEventListener('click', () => { card.scale = Math.min(4, card.scale * 1.5); zoom(card); });
    fit.addEventListener('click', () => { card.scale = 1; zoom(card); viewport.scrollTo(0, 0); });
    retry.addEventListener('click', () => {
      const key = card.theme + '\n' + card.source;
      if (cache.get(key)?.done && !cache.get(key).url) cache.delete(key);
      card.theme = null; card.figure.dataset.state = 'loading'; card.status.textContent = 'Rendering diagram…';
      display(card);
    });
    if (encoder.encode(source).byteLength > maxSource || source.split('\n').length > 500) {
      card.disposed = true; error(card, 'Diagram source is too large to display.');
    } else { cards.add(card); queueMicrotask(track); }
    return figure;
  }
  function clear() {
    generation++; resetFrame(); queue = Promise.resolve();
    for (const card of cards) {
      card.disposed = true; card.image.removeAttribute('src'); card.image.hidden = true;
      card.figure.dataset.state = 'cleared';
      visible.unobserve(card.figure); dimensions.unobserve(card.viewport);
    }
    cards.clear();
    for (const entry of cache.values()) if (entry.url) URL.revokeObjectURL(entry.url);
    cache.clear(); cacheBytes = 0;
  }
  window.addEventListener('pagehide', clear);
  window.addEventListener('pageshow', event => {
    if (event.persisted) for (const figure of document.querySelectorAll('figure.mermaid-diagram[data-state=cleared]')) {
      const source = figure.querySelector('.mermaid-source code')?.textContent;
      if (source !== undefined) figure.replaceWith(create(source));
    }
  });
  return { create, clear };
})();

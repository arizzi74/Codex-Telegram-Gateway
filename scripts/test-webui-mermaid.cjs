#!/usr/bin/env node
// Real bundled Mermaid under the production parent and opaque renderer CSPs.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const parentCSP = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; worker-src 'self'; manifest-src 'self'";
const fixture = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Diagram browser checks</title><link rel="stylesheet" href="/tgw/webui/static/webui.css"><link rel="stylesheet" href="/fixture.css"><script defer src="/tgw/webui/static/webui-format.js"></script><script defer src="/tgw/webui/static/webui-diagrams.js"></script></head><body><main id="workspace"><header class="topbar"><div class="session-heading"><h1>Gateway architecture</h1><p>Demo worker / Primary Codex</p></div></header><div id="connection" data-state="connected"><span id="connection-dot"></span>Connected · Diagram preview</div><section id="transcript"><div id="messages"></div></section><footer id="composer"><div id="prompt-wrap">› Ask Codex to do anything</div><div id="session-status"><span>Codex · high · On request</span><span>Ready</span></div></footer></main></body></html>`;
const fixtureCSS = '#workspace{height:100dvh}#composer{padding:16px 24px;border-top:1px solid var(--border)}#prompt-wrap{padding:12px;border:1px solid var(--border);border-radius:8px;color:var(--muted)}#session-status{display:flex;justify-content:space-between;margin-top:10px;font-size:.8rem;color:var(--muted)}';
const flow = 'flowchart LR\n  Telegram[Telegram bot] --> Gateway[Gateway]\n  Browser[Web UI] --> Gateway\n  Gateway --> Worker[Worker]\n  Worker --> Session[Codex session]';
const sequence = 'sequenceDiagram\n  participant User\n  participant Gateway\n  participant Worker\n  User->>Gateway: Send a prompt\n  Gateway->>Worker: Start the turn\n  Worker-->>Gateway: Response\n  Gateway-->>User: Show the result';
const classDiagram = 'classDiagram\n  class Gateway {\n    connect()\n  }\n  class Worker {\n    startTurn()\n  }\n  Gateway --> Worker';
const fence = source => '```mermaid\n' + source + '\n```';
(async () => {
  let origin;
  const serverRequests = [];
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, origin); serverRequests.push(url.pathname);
    let body, contentType = 'text/html; charset=utf-8', csp = parentCSP;
    if (url.pathname === '/tgw/webui/') body = fixture;
    else if (url.pathname === '/fixture.css') { body = fixtureCSS; contentType = 'text/css'; }
    else if (url.pathname === '/favicon.ico') { response.writeHead(204); response.end(); return; }
    else if (url.pathname === '/tgw/webui/diagram-renderer') {
      body = fs.readFileSync(path.join(assets, 'webui-mermaid-frame.html'));
      csp = "default-src 'none'; base-uri 'none'; frame-ancestors 'self'; form-action 'none'; object-src 'none'; connect-src 'none'; script-src " + origin + "/tgw/webui/static/webui-mermaid-runtime.js; style-src 'unsafe-inline'; img-src 'none'; font-src 'none'; frame-src 'none'; worker-src 'none'; sandbox allow-scripts";
      response.setHeader('X-Frame-Options', 'SAMEORIGIN');
    } else {
      const filename = path.basename(url.pathname);
      if (!url.pathname.startsWith('/tgw/webui/static/') || !['webui.css', 'webui-format.js', 'webui-diagrams.js', 'webui-mermaid-runtime.js'].includes(filename)) { response.writeHead(404); response.end(); return; }
      contentType = filename.endsWith('.js') ? 'text/javascript' : 'text/css'; body = fs.readFileSync(path.join(assets, filename));
    }
    response.writeHead(200, { 'Content-Type': contentType, 'Content-Security-Policy': csp, 'X-Content-Type-Options': 'nosniff', 'Cache-Control': 'no-store' }); response.end(body);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, colorScheme: 'dark' });
  const errors = [], remoteRequests = [], parentViolations = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => { if (!request.url().startsWith(origin) && !request.url().startsWith('blob:')) remoteRequests.push(request.url()); });
  await page.addInitScript(() => {
    if (window !== window.top) return;
    window.createdDiagramURLs = []; window.revokedDiagramURLs = []; window.diagramSVGs = {}; window.diagramMessages = [];
    const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
    URL.createObjectURL = blob => { const url = create(blob); window.createdDiagramURLs.push(url); blob.text().then(text => { window.diagramSVGs[url] = text; }); return url; };
    URL.revokeObjectURL = url => { window.revokedDiagramURLs.push(url); return revoke(url); };
    window.addEventListener('message', event => window.diagramMessages.push(event.data));
    window.diagramPolicyViolations = [];
    document.addEventListener('securitypolicyviolation', event => window.diagramPolicyViolations.push({ directive: event.violatedDirective, blocked: event.blockedURI }));
  });
  const mount = async (markdown, reset = true) => page.evaluate(({ markdown, reset }) => {
    const messages = document.querySelector('#messages');
    if (reset) { window.CodexDiagrams.clear(); messages.replaceChildren(); }
    const article = document.createElement('article'); article.className = 'message assistant';
    const header = document.createElement('div'); header.className = 'message-header';
    const role = document.createElement('span'); role.className = 'role'; role.textContent = '● Codex'; header.append(role);
    article.append(header, window.CodexFormat.markdown(markdown)); messages.append(article);
    document.querySelector('#transcript').scrollTop = 0;
  }, { markdown, reset });
  const ready = async (index = 0) => {
    const diagram = page.locator('figure.mermaid-diagram').nth(index);
    await diagram.waitFor(); await page.waitForFunction(index => document.querySelectorAll('figure.mermaid-diagram')[index]?.dataset.state === 'ready', index, { timeout: 20000 });
    assert.equal(await diagram.locator('img.mermaid-image').evaluate(image => image.complete && image.naturalWidth > 0), true, 'The diagram is a decoded SVG image');
    return diagram;
  };
  const settled = async () => {
    await page.waitForFunction(() => { const figures = Array.from(document.querySelectorAll('figure.mermaid-diagram')); return figures.length && figures.every(figure => ['ready', 'error'].includes(figure.dataset.state)); }, null, { timeout: 20000 });
  };
  try {
    await page.goto(origin + '/tgw/webui/');
    await page.waitForFunction(() => window.CodexDiagrams && window.CodexFormat);
    for (const [label, source] of [['flowchart', flow], ['sequence diagram', sequence], ['class diagram', classDiagram]]) {
      await mount(fence(source));
      const diagram = await ready();
      assert.equal(await diagram.locator('details.mermaid-source pre').textContent(), source, label + ' retains its complete source');
      assert.equal(await diagram.locator('details.mermaid-source').getAttribute('open'), null, label + ' source starts collapsed');
      assert.equal(await diagram.locator('svg').count(), 0, 'Untrusted SVG is never inserted into the application DOM');
      assert.ok(await diagram.locator('img').getAttribute('alt'), 'A diagram image has an accessible description');
      assert.match(await diagram.locator('img').getAttribute('src'), /^blob:/);
      assert.equal(await page.locator('iframe').count(), 1, 'One isolated renderer serves the page');
      const child = page.frames().find(frame => frame.url().includes('/diagram-renderer'));
      assert.ok(child, 'The runtime loads through its dedicated renderer endpoint');
      assert.equal(await child.evaluate(() => { try { return !!parent.document; } catch (_) { return false; } }), false, 'The renderer cannot access the authenticated parent DOM');
    }
    await mount('# One gateway, multiple sessions\n\nThe gateway routes each frontend to its selected worker session.\n\n' + fence(flow));
    let diagram = await ready();
    const initialURL = await diagram.locator('img').getAttribute('src');
    const initialMessages = await page.evaluate(() => window.diagramMessages.length);
    await mount(fence(flow), false);
    await page.locator('figure.mermaid-diagram').nth(1).scrollIntoViewIfNeeded();
    const duplicate = await ready(1);
    assert.equal(await duplicate.locator('img').getAttribute('src'), initialURL, 'Repeated diagrams reuse the cached SVG URL');
    assert.equal(await page.evaluate(() => window.diagramMessages.length), initialMessages, 'Repeated source does not invoke the renderer again');
    await page.locator('.message').last().evaluate(element => element.remove());
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
      await diagram.scrollIntoViewIfNeeded();
      const fits = await diagram.evaluate(element => { const bounds = element.getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth + 1 && document.documentElement.scrollWidth <= innerWidth; });
      assert.equal(fits, true, 'Diagram fits ' + width + 'px without horizontal page overflow');
      const viewport = diagram.locator('.diagram-viewport');
      const before = await diagram.locator('img').boundingBox();
      await diagram.getByRole('button', { name: 'Zoom in diagram', exact: true }).click();
      const after = await diagram.locator('img').boundingBox();
      assert.ok(after.width > before.width, 'Zoom makes a diagram larger at ' + width + 'px');
      assert.equal(await viewport.evaluate(element => element.scrollWidth >= element.clientWidth), true, 'Overflow stays within the diagram viewport');
      if (width <= 390) {
        await diagram.getByRole('button', { name: 'Zoom in diagram', exact: true }).click();
        assert.equal(await viewport.evaluate(element => { element.scrollLeft = element.scrollWidth; return element.scrollWidth > element.clientWidth && element.scrollLeft > 0; }), true, 'A zoomed mobile diagram can pan inside its own viewport');
      }
      await diagram.getByRole('button', { name: 'Zoom out diagram', exact: true }).click();
      await diagram.getByRole('button', { name: 'Fit diagram', exact: true }).click();
      assert.ok((await diagram.locator('img').boundingBox()).width <= (await viewport.boundingBox()).width + 1, 'Fit restores the diagram to the available width');
      if (process.env.WEBUI_SCREENSHOTS && width !== 320) {
        fs.mkdirSync(process.env.WEBUI_SCREENSHOTS, { recursive: true });
        await page.screenshot({ path: path.join(process.env.WEBUI_SCREENSHOTS, 'webui-mermaid-' + (width === 1440 ? 'desktop' : 'mobile') + '.png') });
      }
    }
    const darkSVG = await page.evaluate(url => window.diagramSVGs[url], initialURL);
    await page.emulateMedia({ colorScheme: 'light' });
    await mount(fence(flow)); diagram = await ready();
    const lightSVG = await page.evaluate(url => window.diagramSVGs[url], await diagram.locator('img').getAttribute('src'));
    assert.notEqual(darkSVG, lightSVG, 'Light mode uses a matching diagram palette');
    await page.emulateMedia({ colorScheme: 'dark' });
    await mount('```mermaid\nflowchart LR\nA --> B');
    assert.equal(await page.locator('figure.mermaid-diagram').count(), 0, 'An incomplete streamed fence remains readable source');
    assert.ok((await page.locator('#messages pre').textContent()).includes('A --> B'));
    await mount('```javascript\nconst mermaid = "ordinary code";\n```');
    assert.equal(await page.locator('figure.mermaid-diagram').count(), 0, 'Other code languages retain their existing formatting');
    for (const source of ['flowchart LR\n A[broken', 'flowchart LR\n' + 'A'.repeat(33000), 'flowchart LR\n' + '%% comment\n'.repeat(501), '%%{init: {"securityLevel":"loose"}}%%\nflowchart LR\nA --> B', '---\nconfig:\n  securityLevel: loose\n---\nflowchart LR\nA --> B']) {
      await mount(fence(source)); await settled();
      assert.equal(await page.locator('figure.mermaid-diagram').getAttribute('data-state'), 'error', 'Invalid or excessive source has an explicit fallback');
      assert.equal(await page.locator('details.mermaid-source').getAttribute('open') !== null, true, 'The fallback opens the readable source');
      assert.equal(await page.locator('details.mermaid-source pre').textContent(), source, 'Fallback retains the full original source');
      assert.equal(await page.locator('figure.mermaid-diagram img:visible').count(), 0, 'A failed diagram does not leave a blank image');
    }
    const hostileSources = [
      'flowchart LR\nA["<img src=https://attacker.invalid/image onerror=parent.injected=true>"] --> B',
      'flowchart LR\nA --> B\nclick A "javascript:parent.injected=true"',
      'flowchart LR\nA --> B\nclick A "https://attacker.invalid/diagram"',
      'flowchart LR\nA["<script>parent.injected=true</script>"] --> B',
    ];
    for (const source of hostileSources) {
      await mount(fence(source)); await settled();
      assert.equal(await page.evaluate(() => !!window.injected), false, 'Hostile diagram markup cannot execute in the parent');
      assert.equal(await page.locator('#messages script, #messages iframe, #messages svg, #messages a[href^="javascript:"]').count(), 0, 'Hostile content stays outside the parent DOM');
      const figure = page.locator('figure.mermaid-diagram');
      if (await figure.getAttribute('data-state') === 'ready') {
        const svg = await page.evaluate(url => window.diagramSVGs[url], await figure.locator('img').getAttribute('src'));
        const rendererFrame = page.frames().find(frame => frame.url().includes('/diagram-renderer'));
        const unsafeSVG = await rendererFrame.evaluate(svg => {
          const root = new DOMParser().parseFromString(svg, 'image/svg+xml');
          return Array.from(root.querySelectorAll('*')).some(element => ['script', 'foreignobject', 'iframe', 'image'].includes(element.localName.toLowerCase()) || Array.from(element.attributes).some(attribute => /^on/i.test(attribute.name) || (/^(?:href|xlink:href)$/i.test(attribute.name) && /^(?:https?:|javascript:|data:)/i.test(attribute.value))));
        }, svg);
        assert.equal(unsafeSVG, false, 'The SVG image contains no executable elements, event handlers or remote references');
      }
    }
    await page.evaluate(source => {
      window.CodexDiagrams.clear();
      const messages = document.querySelector('#messages'); messages.replaceChildren();
      const spacer = document.createElement('div'); spacer.style.height = '4000px';
      const article = document.createElement('article'); article.className = 'message'; article.append(window.CodexDiagrams.create(source));
      messages.append(spacer, article); document.querySelector('#transcript').scrollTop = 0;
    }, sequence);
    await page.waitForTimeout(100);
    assert.equal(await page.locator('iframe').count(), 0, 'Offscreen history does not load or execute the renderer');
    await page.locator('figure.mermaid-diagram').scrollIntoViewIfNeeded(); await ready();
    await page.evaluate(() => { window.CodexDiagrams.clear(); document.querySelector('#messages').replaceChildren(); });
    let heldRuntime, notifyRuntime;
    const runtimeHeld = new Promise(resolve => { notifyRuntime = resolve; });
    await page.route(origin + '/tgw/webui/static/webui-mermaid-runtime.js', route => { heldRuntime = route; notifyRuntime(); }, { times: 1 });
    const createdBeforeCancel = await page.evaluate(() => window.createdDiagramURLs.length);
    await mount(fence(flow)); await runtimeHeld;
    await page.evaluate(() => { window.CodexDiagrams.clear(); document.querySelector('#messages').replaceChildren(); });
    await heldRuntime.abort().catch(() => {});
    await page.waitForTimeout(100);
    assert.equal(await page.locator('iframe').count(), 0, 'Session cleanup cancels a renderer whose bundle is still loading');
    assert.equal(await page.evaluate(() => window.createdDiagramURLs.length), createdBeforeCancel, 'Cancelled rendering cannot recreate private blob images');
    let firstCachedURL;
    for (let index = 0; index < 34; index++) {
      await page.evaluate(() => document.querySelector('#messages').replaceChildren());
      await mount(fence('flowchart LR\nA[Gateway] --> B[Worker ' + index + ']'), false);
      const cached = await ready();
      if (!index) firstCachedURL = await cached.locator('img').getAttribute('src');
    }
    assert.ok(await page.evaluate(url => window.revokedDiagramURLs.includes(url), firstCachedURL), 'Cache overflow evicts and revokes the oldest unreferenced diagram');
    assert.ok(await page.evaluate(() => window.createdDiagramURLs.filter(url => !window.revokedDiagramURLs.includes(url)).length) <= 32, 'Completed diagram caching remains bounded');
    await mount(fence(flow)); diagram = await ready();
    const url = await diagram.locator('img').getAttribute('src');
    await page.evaluate(() => { document.querySelector('#messages').replaceChildren(); window.CodexDiagrams.clear(); });
    assert.equal(await page.locator('iframe').count(), 0, 'Clearing a session removes the isolated renderer');
    assert.ok(await page.evaluate(url => window.revokedDiagramURLs.includes(url), url), 'Session and authentication cleanup revoke cached diagram URLs');
    await page.evaluate(source => { const orphan = window.CodexDiagrams.create(source); document.querySelector('#messages').append(orphan); orphan.remove(); }, sequence);
    await page.waitForTimeout(150);
    assert.equal(await page.locator('figure.mermaid-diagram, #messages img').count(), 0, 'Removed nodes cannot repopulate a cleared transcript');
    assert.deepEqual(remoteRequests, [], 'All diagram rendering runs locally without network requests');
    parentViolations.push(...await page.evaluate(() => window.diagramPolicyViolations));
    assert.deepEqual(parentViolations, [], 'The authenticated parent does not relax or violate its CSP');
    assert.deepEqual(errors, [], 'Renderer failures are handled without unhandled browser exceptions');
    assert.ok(serverRequests.includes('/tgw/webui/static/webui-mermaid-runtime.js'), 'The checked-in real bundle ran in the browser');
    console.log('Mermaid browser checks passed: real flowchart/sequence/class rendering, opaque sandbox and strict parent CSP, safe SVG images, source fallback, streamed fences, invalid/config/size limits, hostile content without remote access, cache reuse and eviction, desktop/mobile fit and zoom, both themes, lazy history, cancelled loads, cleared URLs and detached nodes.');
  } finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
})().catch(error => { console.error(error); process.exitCode = 1; });

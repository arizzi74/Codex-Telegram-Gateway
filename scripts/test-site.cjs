#!/usr/bin/env node
'use strict';
// Static Pages checks: no production/build dependency, and no external requests.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const site = path.resolve(__dirname, '../site');
const repo = path.resolve(__dirname, '..');
const prefix = '/Codex-Telegram-Gateway/';
const screenshots = process.env.SITE_SCREENSHOTS || '/tmp/telegramgw-site-preview';
const mime = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript', '.png': 'image/png', '.svg': 'image/svg+xml', '.json': 'application/json' };
const server = http.createServer((request, response) => {
  const url = new URL(request.url, 'http://localhost');
  if (!url.pathname.startsWith(prefix)) { response.writeHead(404); response.end(); return; }
  const relative = decodeURIComponent(url.pathname.slice(prefix.length)) || 'index.html';
  const file = path.resolve(site, relative);
  if (!file.startsWith(site + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end(); return; }
  response.writeHead(200, { 'Content-Type': mime[path.extname(file)] || 'text/plain' });
  response.end(fs.readFileSync(file));
});

(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ colorScheme: 'dark', permissions: ['clipboard-read', 'clipboard-write'], reducedMotion: 'reduce' });
    const page = await context.newPage();
    const errors = [], failed = [], external = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('response', response => { if (response.status() >= 400) failed.push(response.url()); });
    await page.route('**/*', route => { if (new URL(route.request().url()).origin !== origin) { external.push(route.request().url()); return route.abort('blockedbyclient'); } return route.continue(); });
    fs.mkdirSync(screenshots, { recursive: true });
    for (const width of [1440, 1024, 768, 390, 320]) {
      await page.setViewportSize({ width, height: width <= 390 ? 844 : 1000 });
      await page.goto(origin + prefix);
      await page.locator('h1').waitFor();
      await page.evaluate(async () => { for (const image of document.images) image.loading = 'eager'; await Promise.all([...document.images].map(image => image.decode())); });
      const layout = await page.evaluate(() => ({ width: innerWidth, pageWidth: document.documentElement.scrollWidth, bodyWidth: document.body.scrollWidth, images: [...document.images].map(image => ({ alt: image.alt, loaded: image.naturalWidth > 0, width: image.clientWidth, height: image.clientHeight, ratio: image.naturalWidth / image.naturalHeight })) }));
      assert.ok(layout.pageWidth <= width + 1 && layout.bodyWidth <= width + 1, `Horizontal overflow at ${width}: ${JSON.stringify(layout)}`);
      for (const image of layout.images) { assert.ok(image.loaded && image.alt.length > 20, 'Screenshot missing or inaccessible'); assert.ok(Math.abs(image.width / image.height - image.ratio) < .02, 'Screenshot aspect ratio distorted'); }
      assert.equal(await page.locator('h1').count(), 1);
      assert.equal(await page.locator('#screenshots').count(), 1);
      if (width === 1440 || width === 390) {
        await page.screenshot({ path: path.join(screenshots, `site-${width}.png`) });
        await page.screenshot({ path: path.join(screenshots, `site-${width}-full.png`), fullPage: true });
      }
    }
    // Every local URL must work under a repository Pages prefix, not just '/'.
    const links = await page.locator('a[href],link[href],script[src],img[src]').evaluateAll(nodes => nodes.map(node => node.getAttribute('href') || node.getAttribute('src')));
    for (const link of links) {
      if (link.startsWith('#')) { if (link.length > 1) assert.equal(await page.locator('[id="' + link.slice(1) + '"]').count(), 1, 'Broken anchor ' + link); continue; }
      if (/^https:\/\//.test(link)) {
        assert.ok(link.startsWith('https://github.com/arizzi74/Codex-Telegram-Gateway'), 'Unexpected outbound link: ' + link);
        const local = link.split('/blob/main/')[1] || link.split('/tree/main/')[1];
        if (local) assert.ok(fs.existsSync(path.join(repo, local.split('#')[0])), 'Broken repository documentation link: ' + link);
        continue;
      }
      assert.ok(!link.startsWith('/'), 'Asset URL breaks repository Pages prefix: ' + link);
      assert.ok(fs.existsSync(path.join(site, link)), 'Missing site asset: ' + link);
    }
    for (const name of ['gateway', 'worker']) {
      const button = page.getByRole('button', { name: `Copy ${name} install command`, exact: true });
      await button.click();
      const expected = await page.locator('#' + name + '-command').textContent();
      assert.equal(await page.evaluate(() => navigator.clipboard.readText()), expected.trim());
      assert.equal(await button.getAttribute('data-copied'), 'true');
    }
    assert.match(await page.locator('#gateway-command').innerText(), /\| sudo sh$/);
    assert.match(await page.locator('#worker-command').innerText(), /\| sh$/);
    await page.evaluate(() => Object.defineProperty(navigator.clipboard, 'writeText', { configurable: true, value: () => Promise.reject(new Error('Clipboard unavailable')) }));
    await page.getByRole('button', { name: 'Copy worker install command', exact: true }).click();
    assert.match(await page.locator('#copy-status').innerText(), /command is selected/);
    assert.equal(await page.evaluate(() => getSelection().toString()), (await page.locator('#worker-command').textContent()).trim());
    await page.goto(origin + prefix);
    await page.keyboard.press('Tab');
    assert.equal(await page.evaluate(() => document.activeElement.className), 'skip-link');
    await page.keyboard.press('Enter');
    assert.equal(new URL(page.url()).hash, '#main');
    assert.deepEqual(errors, []); assert.deepEqual(failed, []); assert.deepEqual(external, []);
    const capture = JSON.parse(fs.readFileSync(path.join(site, 'assets/screenshots/capture.json'), 'utf8'));
    assert.equal(capture.demo_data, true); assert.ok(!Number.isNaN(Date.parse(capture.captured_at)));
    console.log('Website passed: 320/390/768/1024/1440 px, real screenshot assets, repository-prefix URLs, documentation links, clipboard success/fallback, keyboard navigation, and zero external requests.');
    console.log('Page previews: ' + screenshots);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; }).finally(() => server.close());

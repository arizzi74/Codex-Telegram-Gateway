/* Independent DOM renderer informed by OpenAI Codex's Apache-2.0 TUI.
 * Upstream revision and source mapping: docs/webui.md. No HTML from messages is executed. */
'use strict';
window.CodexFormat = (() => {
  const node = (tag, cls, text) => {
    const value = document.createElement(tag);
    if (cls) value.className = cls;
    if (text !== undefined) value.textContent = String(text);
    return value;
  };
  // Neither terminal escapes nor control characters are meaningful to a browser transcript.
  const clean = value => String(value ?? '').replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, '').replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '').replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
  function safeLink(value) {
    try {
      const parsed = new URL(value);
      if (['https:', 'http:', 'mailto:'].includes(parsed.protocol)) return parsed.href;
    } catch (_) { /* Local file references remain readable text, never browser fetches. */ }
    return null;
  }
  function inline(parent, value, depth = 0) {
    const text = clean(value);
    if (depth > 8) { parent.append(document.createTextNode(text)); return; }
    const pattern = /(`+)([^`\n]+?)\1|\[([^\]\n]+)\]\(([^\s)]+)\)|\*\*([^*\n]+)\*\*|__([^_\n]+)__|~~([^~\n]+)~~|\*([^*\n]+)\*|_([^_\n]+)_|https?:\/\/[^\s<>]+/g;
    let cursor = 0;
    for (const match of text.matchAll(pattern)) {
      parent.append(document.createTextNode(text.slice(cursor, match.index)));
      let child;
      if (match[1]) child = node('code', '', match[2]);
      else if (match[3]) {
        const href = safeLink(match[4]);
        child = node(href ? 'a' : 'span', '', match[3]);
        if (href) { child.href = href; child.target = '_blank'; child.rel = 'noopener noreferrer'; }
        else child.title = clean(match[4]);
      } else if (match[5] || match[6]) { child = node('strong'); inline(child, match[5] || match[6], depth + 1); }
      else if (match[7]) { child = node('s'); inline(child, match[7], depth + 1); }
      else if (match[8] || match[9]) { child = node('em'); inline(child, match[8] || match[9], depth + 1); }
      else { child = node('a', '', match[0]); child.href = safeLink(match[0]); child.target = '_blank'; child.rel = 'noopener noreferrer'; }
      parent.append(child);
      cursor = match.index + match[0].length;
    }
    parent.append(document.createTextNode(text.slice(cursor)));
  }
  function code(value, language = '') {
    const pre = node('pre');
    if (language) pre.append(node('span', 'code-label', clean(language).slice(0, 60)));
    const content = node('code');
    const text = clean(value);
    if (/^(diff|patch)$/i.test(language)) {
      for (const line of text.split('\n')) content.append(node('span', 'diff-line ' + (line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-delete' : /^(?:@@|diff |index )/.test(line) ? 'diff-meta' : ''), line));
    } else if (/^(?:js|javascript|ts|typescript|json|go|rust|rs|python|py|sh|bash|sql|c|cpp|java|css|yaml|yml)$/i.test(language)) {
      const pattern = /("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')|((?:\/\/|#)[^\n]*)|\b(const|let|var|function|return|if|else|for|while|class|import|from|export|async|await|true|false|null|None|def|fn|pub|use|struct|func|package|type|nil|SELECT|FROM|WHERE)\b|\b(\d+(?:\.\d+)?)\b/g;
      let cursor = 0;
      for (const match of text.matchAll(pattern)) {
        content.append(document.createTextNode(text.slice(cursor, match.index)), node('span', match[1] ? 'syntax-string' : match[2] ? 'syntax-comment' : match[3] ? 'syntax-keyword' : 'syntax-number', match[0]));
        cursor = match.index + match[0].length;
      }
      content.append(document.createTextNode(text.slice(cursor)));
    } else content.textContent = text;
    pre.append(content);
    return pre;
  }
  const cells = text => text.trim().replace(/^\||\|$/g, '').split('|').map(value => value.trim());
  function markdown(value, depth = 0) {
    const root = document.createDocumentFragment();
    if (depth > 12) { root.append(node('p', '', clean(value))); return root; }
    const lines = clean(value).split('\n');
    for (let i = 0; i < lines.length;) {
      const line = lines[i];
      if (!line.trim()) { i++; continue; }
      const fence = line.match(/^\s*(`{3,}|~{3,})([^\s]*)\s*$/);
      if (fence) {
        const body = [];
        i++;
        while (i < lines.length && !lines[i].trim().startsWith(fence[1])) body.push(lines[i++]);
        if (i < lines.length) i++;
        root.append(code(body.join('\n'), fence[2]));
        continue;
      }
      const heading = line.match(/^(#{1,6})\s+(.+)$/);
      if (heading) { const h = node('h' + Math.min(6, heading[1].length + 1)); inline(h, heading[1] + ' ' + heading[2]); root.append(h); i++; continue; }
      if (/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line)) { root.append(node('hr')); i++; continue; }
      if (/^\s*>/.test(line)) {
        const quote = node('blockquote'); const body = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*> ?/, ''));
        quote.append(markdown(body.join('\n'), depth + 1)); root.append(quote); continue;
      }
      if (line.includes('|') && i + 1 < lines.length && /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[i + 1])) {
        const labels = cells(line); const wrap = node('div', 'table-wrap'); const table = node('table'); const head = node('thead'); const row = node('tr');
        for (const label of labels) { const th = node('th'); th.scope = 'col'; inline(th, label); row.append(th); }
        head.append(row); table.append(head); const body = node('tbody'); i += 2;
        while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
          const tr = node('tr'); const values = cells(lines[i++]);
          labels.forEach((label, index) => { const td = node('td'); td.dataset.label = label; inline(td, values[index] || ''); tr.append(td); });
          body.append(tr);
        }
        table.append(body); wrap.append(table); root.append(wrap); continue;
      }
      const list = line.match(/^\s*([-+*]|\d+[.)])\s+(.+)$/);
      if (list) {
        const ordered = /^\d/.test(list[1]); const ul = node(ordered ? 'ol' : 'ul');
        if (ordered) ul.start = parseInt(list[1], 10);
        while (i < lines.length) {
          const item = lines[i].match(/^\s*([-+*]|\d+[.)])\s+(.+)$/);
          if (!item || /^\d/.test(item[1]) !== ordered) break;
          const li = node('li'); inline(li, item[2]); ul.append(li); i++;
        }
        root.append(ul); continue;
      }
      const para = [line]; i++;
      while (i < lines.length && lines[i].trim() && !/^(?:\s*[`~]{3,}|#{1,6}\s|\s*>|\s*[-+*]\s|\s*\d+[.)]\s)/.test(lines[i]) && !(lines[i].includes('|') && i + 1 < lines.length && /^[\s|:-]+$/.test(lines[i + 1]))) para.push(lines[i++]);
      const p = node('p'); inline(p, para.join('\n')); root.append(p);
    }
    return root;
  }
  return { node, clean, inline, markdown, code };
})();

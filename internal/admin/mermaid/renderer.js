// This module only runs inside the opaque-origin diagram iframe. The gateway
// and Web UI never insert Mermaid's SVG into their authenticated documents.
import mermaid from 'mermaid';

const parentOrigin = new URL(location.href).origin;
const maxSourceBytes = 32 * 1024;
const maxSVGBytes = 2 * 1024 * 1024;
const maxDimension = 16384;
const encoder = new TextEncoder();
const stage = document.getElementById('diagram-stage');
let rendering = false;
let sequence = 0;

function reply(message) {
  parent.postMessage(message, parentOrigin);
}

function safeSVG(serialized) {
  if (encoder.encode(serialized).byteLength > maxSVGBytes) throw new Error('Diagram is too large to display.');
  const parsed = new DOMParser().parseFromString(serialized, 'image/svg+xml');
  const svg = parsed.documentElement;
  if (svg.localName !== 'svg' || parsed.querySelector('parsererror')) throw new Error('Diagram could not be rendered.');

  // An image is the only supported output. Strip interactive/HTML content and
  // all resource references except local SVG fragments, even in strict mode.
  for (const node of [...svg.querySelectorAll('*')]) {
    if (['script', 'foreignobject', 'image', 'iframe', 'object', 'embed', 'animate', 'animatetransform', 'animatemotion', 'set'].includes(node.localName.toLowerCase()) || node.namespaceURI !== 'http://www.w3.org/2000/svg') {
      node.remove();
      continue;
    }
    if (node.localName === 'a') node.replaceWith(...node.childNodes);
  }
  for (const node of [svg, ...svg.querySelectorAll('*')]) {
    for (const attribute of [...node.attributes]) {
      const name = attribute.name.toLowerCase();
      if (name.startsWith('on') || name === 'target' || name === 'xml:base' || ((name === 'href' || name === 'xlink:href') && !/^#[a-zA-Z0-9_.:-]+$/.test(attribute.value))) node.removeAttributeNode(attribute);
      else if (name === 'style') node.setAttribute(name, safeCSS(attribute.value));
      else if (/url\s*\(/i.test(attribute.value) && !/^url\(\s*['"]?#[a-zA-Z0-9_.:-]+['"]?\s*\)$/.test(attribute.value)) node.removeAttributeNode(attribute);
    }
    if (node.localName === 'style') node.textContent = safeCSS(node.textContent);
  }
  const box = svg.getAttribute('viewBox')?.trim().split(/[\s,]+/).map(Number);
  const width = box?.length === 4 ? box[2] : Number.parseFloat(svg.getAttribute('width'));
  const height = box?.length === 4 ? box[3] : Number.parseFloat(svg.getAttribute('height'));
  if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0 || width > maxDimension || height > maxDimension) throw new Error('Diagram dimensions are too large to display.');
  svg.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  svg.setAttribute('width', String(Math.ceil(width)));
  svg.setAttribute('height', String(Math.ceil(height)));
  svg.setAttribute('preserveAspectRatio', 'xMidYMid meet');
  svg.removeAttribute('style');
  const result = new XMLSerializer().serializeToString(svg);
  if (encoder.encode(result).byteLength > maxSVGBytes) throw new Error('Diagram is too large to display.');
  return {svg: result, width: Math.ceil(width), height: Math.ceil(height)};
}

function safeCSS(css) {
  // Mermaid generates local marker references. Keep those, but no imports,
  // external URLs, CSS escapes or HTML syntax from user-supplied class styles.
  if (/[\\<]|@import|@font-face|expression\s*\(/i.test(css)) return '';
  return css.replace(/url\s*\(([^)]*)\)/gi, (value, target) => /^\s*['"]?#[a-zA-Z0-9_.:-]+['"]?\s*$/.test(target) ? value : 'none');
}

window.addEventListener('message', async event => {
  if (event.source !== parent || event.origin !== parentOrigin) return;
  const request = event.data;
  if (!request || request.type !== 'codex-mermaid-render' || typeof request.id !== 'string' || request.id.length > 128) return;
  const result = {type: 'codex-mermaid-result', id: request.id};
  if (rendering) {
    reply({...result, error: 'Another diagram is rendering. Please retry.'});
    return;
  }
  if (typeof request.source !== 'string' || encoder.encode(request.source).byteLength > maxSourceBytes || request.source.split('\n').length > 500) {
    reply({...result, error: 'Diagram source is too large to display.'});
    return;
  }
  if (/%%\s*\{/.test(request.source) || /^\s*---(?:\s|$)/.test(request.source)) {
    reply({...result, error: 'Diagram configuration directives are not supported.'});
    return;
  }
  rendering = true;
  try {
    mermaid.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      htmlLabels: false,
      flowchart: {htmlLabels: false},
      layout: 'dagre',
      secure: ['secure', 'securityLevel', 'startOnLoad', 'maxTextSize', 'maxEdges', 'htmlLabels', 'layout'],
      theme: request.theme === 'light' ? 'default' : 'dark',
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      maxTextSize: maxSourceBytes,
      maxEdges: 200,
      suppressErrorRendering: true,
      logLevel: 'fatal',
    });
    const {svg} = await mermaid.render(`codex-diagram-${++sequence}`, request.source, stage);
    reply({...result, ...safeSVG(svg)});
  } catch {
    reply({...result, error: 'This diagram could not be rendered. View its source to check the syntax.'});
  } finally {
    stage.replaceChildren();
    rendering = false;
  }
});

reply({type: 'codex-mermaid-ready'});

// ui.js — small page-level behaviors shared by every virtweb page.
//
// Kept as an external module (not inline handlers) so the site can run
// under a strict Content-Security-Policy (script-src 'self').

// Destructive forms carry their confirmation text in data-confirm;
// cancelling the dialog also cancels the submission.
for (const form of document.querySelectorAll('form[data-confirm]')) {
  form.addEventListener('submit', (e) => {
    if (!window.confirm(form.dataset.confirm)) e.preventDefault();
  });
}

// The VM detail page's "Copy" button copies the domain XML (the
// clipboard API needs a user gesture, which the click is).
// Domain XML: rendered as a collapsible tree (DOMParser — no
// libraries), so a long domain definition becomes browsable: the
// root element starts open, its children collapsed. The raw text
// stays in a hidden <pre>, one click away (the "Raw" toggle) and is
// what Copy copies, so it is always the byte-exact document.
function xmlRenderNode(el, open) {
  const kids = [...el.children];
  const tag = document.createElement('span');
  tag.className = 'xml-tag';
  tag.textContent = el.nodeName;
  const attrs = document.createElement('span');
  attrs.className = 'xml-attrs';
  attrs.textContent =
    ' ' + el.getAttributeNames().map((n) => `${n}="${el.getAttribute(n)}"`).join(' ');
  if (kids.length === 0) {
    const node = document.createElement('div');
    node.className = 'xml-leaf';
    const text = document.createElement('span');
    text.className = 'xml-text';
    text.textContent = '=' + (el.textContent ?? '');
    node.append(tag, attrs, text);
    return node;
  }
  const det = document.createElement('details');
  det.open = open;
  const sum = document.createElement('summary');
  sum.append(tag, attrs);
  const ul = document.createElement('ul');
  for (const c of kids) ul.append(xmlRenderNode(c, false));
  det.append(sum, ul);
  return det;
}

const copyBtn = document.getElementById('copy-xml');
const xmlPre = document.getElementById('vm-xml');
const xmlTreeEl = document.getElementById('vm-xml-tree');
const xmlToggle = document.getElementById('xml-toggle');
if (xmlTreeEl && xmlPre && xmlPre.textContent.trim()) {
  const parsed = new DOMParser().parseFromString(
    xmlPre.textContent, 'application/xml');
  if (!parsed.querySelector('parsererror')) {
    xmlTreeEl.append(xmlRenderNode(parsed.documentElement, true));
    xmlPre.hidden = true;
  } else {
    // not well-formed (should not happen — virsh produced it):
    // fall back to the raw text view.
    xmlTreeEl.hidden = true;
    xmlToggle.remove();
  }
}
if (xmlToggle && xmlTreeEl && xmlPre) {
  xmlToggle.addEventListener('click', () => {
    const showTree = !xmlPre.hidden;
    xmlTreeEl.hidden = !showTree;
    xmlPre.hidden = showTree;
    xmlToggle.textContent = showTree ? 'Raw' : 'Tree';
  });
}
if (copyBtn && xmlPre && navigator.clipboard?.writeText) {
  copyBtn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(xmlPre.textContent);
      copyBtn.textContent = 'Copied';
      setTimeout(() => (copyBtn.textContent = 'Copy'), 1500);
    } catch {
      copyBtn.textContent = 'Copy failed';
      setTimeout(() => (copyBtn.textContent = 'Copy'), 1500);
    }
  });
}

// The VM detail page's "Console" row reflects whether a console
// session is open right now (one-shot fetch on page load; the
// Refresh link re-runs the page, so it re-fetches too).
const consoleStatusEl = document.getElementById('vm-console-status');
const vmCfgEl = document.getElementById('vm-cfg');
if (consoleStatusEl && vmCfgEl) {
  try {
    const vmCfg = JSON.parse(vmCfgEl.textContent);
    if (vmCfg.consoleStatusUrl) {
      fetch(vmCfg.consoleStatusUrl)
        .then((r) => (r.ok ? r.json() : null))
        .then((out) => {
          if (!out) return;
          consoleStatusEl.textContent = out.open ? 'open' : '–';
          consoleStatusEl.classList.toggle('in-use', !!out.open);
        })
        .catch(() => {});
    }
  } catch { /* malformed cfg: leave the placeholder */ }
}

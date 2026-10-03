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
const copyBtn = document.getElementById('copy-xml');
const xmlPre = document.getElementById('vm-xml');
if (copyBtn && xmlPre && navigator.clipboard?.writeText) {
  copyBtn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(xmlPre.innerText);
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

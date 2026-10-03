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

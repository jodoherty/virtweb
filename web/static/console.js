// Serial console: xterm.js 6.0.0 (vendored, third-party/xtermjs, MIT)
// over the plain-HTTP streams transport — no WebSocket:
//   server -> client: chunked GET, consumed as a ReadableStream
//   client -> server: small POSTs of raw bytes
// One console session per (server, VM); a concurrent open gets 409.
const cfg = JSON.parse(document.getElementById('console-cfg').textContent);
const statusEl = document.getElementById('console-status');
const container = document.getElementById('term');

const term = new Terminal({
  cursorBlink: true,
  fontSize: 13,
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
  scrollback: 2000,
});
const fit = (() => {
  // addon-fit's UMD build assigns `globalThis.FitAddon = <namespace>`,
  // so the constructor is `FitAddon.FitAddon` (core xterm instead spreads
  // its exports onto global, giving a direct `Terminal`).
  const Ctor = typeof FitAddon === 'function' ? FitAddon : FitAddon.FitAddon;
  return new Ctor();
})();
term.loadAddon(fit);
term.open(container);
fit.fit();
new ResizeObserver(() => fit.fit()).observe(container);
term.focus();

// Debug/test hook (e2e suite reads the buffer through this).
window.__term = term;

const encoder = new TextEncoder();
term.onData((data) => {
  fetch(cfg.inputUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: encoder.encode(data),
  });
});

try {
  const res = await fetch(cfg.streamUrl);
  if (res.status === 409) {
    statusEl.textContent = 'A console session for this VM is already open.';
  } else if (!res.ok || !res.body) {
    statusEl.textContent = 'Failed to open console (HTTP ' + res.status + ').';
    res.body?.cancel().catch(() => {});
  } else {
    statusEl.textContent = 'Connected.';
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      term.write(decoder.decode(value, { stream: true }));
    }
    term.write(decoder.decode());
    statusEl.textContent = 'Session ended.';
  }
} catch (err) {
  statusEl.textContent = 'Console error: ' + err.message;
}

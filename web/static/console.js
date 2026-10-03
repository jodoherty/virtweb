// Serial console: xterm.js 6.0.0 fork (third-party/xtermjs, MIT)
// over DuplexChannel (web/static/duplex.js): one GET stream for the
// downlink, ordered POSTs for the uplink. No WebSocket.
// One console session per (server, VM); a concurrent open gets 409.
import { DuplexChannel } from './duplex.js';

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

// Debug/test hook (e2e suite reads the buffer through __term, and
// drives the transport through __duplex).
window.__term = term;

const ch = new DuplexChannel(cfg.streamUrl, cfg.inputUrl, {
  // The server holds the connection while it sets up the remote ssh
  // session; bound it so a wedged server can't leave "Connecting…" up
  // forever.
  connectTimeoutMs: 20_000,
});
window.__duplex = ch;

// Reconnect affordance, shown once the session is no longer live.
const reconnectBtn = document.getElementById('console-reconnect');
if (reconnectBtn) reconnectBtn.addEventListener('click', () => location.reload());
function setStatus(msg, { failed = false, reconnect = false } = {}) {
  statusEl.textContent = msg;
  statusEl.classList.toggle('status-err', failed);
  if (reconnectBtn) reconnectBtn.hidden = !reconnect;
}

const encoder = new TextEncoder();
term.onData((data) => {
  const bytes = encoder.encode(data);
  if (!bytes.length) return;
  // Session loss surfaces through the stream below; the channel
  // rejects writes after it dies, which is fine to drop.
  ch.write(bytes).catch(() => {});
});

let connected = false;
try {
  await ch.open();
  connected = true;
  setStatus('Connected.', { reconnect: false });
  const decoder = new TextDecoder();
  for await (const value of ch.readable) {
    term.write(decoder.decode(value, { stream: true }));
  }
  term.write(decoder.decode());
  // Clean server-side EOF (VM stopped, console detached, …).
  setStatus('Session ended.', { reconnect: true });
} catch (err) {
  if (err.code === 'conflict') {
    setStatus('A console session for this VM is already open in another tab.', { reconnect: true });
  } else if (err.code === 'timeout') {
    setStatus('Connection timed out — the server did not open the console in time.', { failed: true, reconnect: true });
  } else if (connected) {
    setStatus('Connection lost — the session was dropped mid-stream.', { failed: true, reconnect: true });
  } else {
    setStatus('Could not connect: ' + err.message, { failed: true, reconnect: true });
  }
}

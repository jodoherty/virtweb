// Serial console: xterm.js 6.0.0 fork (third-party/xtermjs, MIT)
// over DuplexChannel (web/static/duplex.js): one GET stream for the
// downlink, ordered POSTs for the uplink. No WebSocket.
// One console session per (server, VM); a concurrent open gets 409.
import { DuplexChannel } from './duplex.js';
import { scheduleRetry, clearRetries } from './retry.js';

const cfg = JSON.parse(document.getElementById('console-cfg').textContent);
const statusEl = document.getElementById('console-status');
const container = document.getElementById('term');
// Shut-off VM: the page shows the "start it first" card and there is
// no terminal to drive — everything below is skipped.
const live = !!container;

// Fullscreen: pin the terminal area to the screen (Esc leaves it).
document.getElementById('console-fullscreen')?.addEventListener('click', () => {
  if (!live) return;
  const main = document.getElementById('console-main');
  if (document.fullscreenElement) document.exitFullscreen();
  else main.requestFullscreen?.();
});
if (!live) {
  const fs = document.getElementById('console-fullscreen');
  if (fs) fs.disabled = true;
}

(async () => {
  if (!live) return; // shut-off VM
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
  // Reconnect also resets the auto-retry budget (a fresh attempt set).
  const reconnectBtn = document.getElementById('console-reconnect');
  if (reconnectBtn)
    reconnectBtn.addEventListener('click', () => {
      clearRetries();
      location.reload();
    });
  function setStatus(msg, { failed = false, reconnect = false } = {}) {
    statusEl.textContent = msg;
    statusEl.classList.toggle('status-err', failed);
    if (reconnectBtn) reconnectBtn.hidden = !reconnect;
  }

  // A failed or lost session is retried automatically: a bounded
  // number of page reloads (see retry.js). Clean session ends are NOT
  // retried — the session is over, and re-attaching is the user's call.
  function retryOrReconnect(base) {
    if (scheduleRetry()) {
      setStatus(base + ' — retrying shortly…', { failed: true, reconnect: true });
    } else {
      setStatus(base + ' — retry limit reached.', { failed: true, reconnect: true });
    }
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
    clearRetries();
    setStatus('Connected.', { reconnect: false });
    const decoder = new TextDecoder();
    for await (const value of ch.readable) {
      term.write(decoder.decode(value, { stream: true }));
    }
    term.write(decoder.decode());
    // The stream ended. Wire protocol v2 tells us HOW: a SESSION_ERROR
    // frame (the session failed server-side, with the reason) is an
    // error even though the stream closed cleanly; everything else is
    // a clean end (VM stopped, console detached, …).
    if (ch.closeReason === 'error') {
      const why = ch.terminalMessage
        ? ': ' + ch.terminalMessage
        : ' — the session was dropped';
      retryOrReconnect('Connection lost' + why);
    } else {
      setStatus('Session ended.', { reconnect: true });
    }
  } catch (err) {
    if (err.code === 'conflict') {
      // The session is in use elsewhere; keep trying until it frees.
      retryOrReconnect(
        'A console session for this VM is already open in another tab',
      );
    } else if (err.code === 'timeout') {
      retryOrReconnect('Connection timed out — the server did not open the console in time');
    } else if (connected) {
      retryOrReconnect('Connection lost — the session was dropped mid-stream');
    } else {
      retryOrReconnect('Could not connect: ' + err.message);
    }
  }
})();

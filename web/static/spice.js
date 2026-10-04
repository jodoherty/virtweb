// SPICE viewer: spice-html5 fork (third-party/spice-html5-fork) over
// the tunnel endpoints. Each SPICE channel gets its own
// (GET stream + POST input) session, i.e. its own ssh -W tunnel —
// the same shape as the WebSocket-per-channel websockify topology.
import { SpiceMainConn } from '/spice/src/main.js';
import { scheduleRetry, clearRetries } from '/static/retry.js';

const cfg = JSON.parse(document.getElementById('spice-cfg').textContent);
const statusEl = document.getElementById('spice-status');
const reconnectBtn = document.getElementById('spice-reconnect');
if (reconnectBtn)
  reconnectBtn.addEventListener('click', () => {
    clearRetries();
    location.reload();
  });
// The Disconnect button is only on screen while a session is live;
// Reconnect takes over afterwards (both never share the screen).
const disconnectBtn = document.getElementById('spice-disconnect');
function spiceStatus(msg, { failed = false, reconnect = false, connected = false } = {}) {
  statusEl.textContent = msg;
  statusEl.classList.toggle('status-err', failed);
  if (reconnectBtn) reconnectBtn.hidden = !reconnect;
  if (disconnectBtn) disconnectBtn.hidden = !connected;
}
if (disconnectBtn)
  disconnectBtn.addEventListener('click', () => {
    // Deliberate teardown in the fork's own idiom: siblings first, then
    // the main channel in "closing" state, so the close events report
    // nothing. Each channel dies with its tunnel session (the server
    // kills the ssh -W process when the stream request ends).
    const sp = window.__spice;
    if (sp) {
      sp.close_sibling_channels();
      sp.cleanup();
      window.__spice = undefined;
    }
    // The display area is the page's own element (#spice); the fork's
    // canvases (and the simulated-cursor overlay, when in use) are its
    // children. Drop the last frame and the guest cursor so the
    // "Disconnected." state cannot read as a live session.
    const screen = document.getElementById('spice');
    if (screen) {
      screen.innerHTML = '';
      screen.style.cursor = '';
    }
    clearRetries();
    spiceStatus('Disconnected.', { reconnect: true });
    // Reconnect reloads the page; put the connect form back so the
    // next Connect (possibly with a password) is one click away.
    if (formEl) formEl.hidden = false;
  });

// The connect form is an overlay card that disappears once the
// session is live; the display area never moves either way.
const formEl = document.getElementById('spice-form');
const logPanel = document.getElementById('spice-log');
document.getElementById('spice-logtoggle')?.addEventListener('click', () => {
  logPanel.hidden = !logPanel.hidden;
});
document.getElementById('spice-fullscreen')?.addEventListener('click', () => {
  const main = document.getElementById('spice-main');
  if (document.fullscreenElement) document.exitFullscreen();
  else main.requestFullscreen?.();
});

document.getElementById('spice-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const q = new URLSearchParams(new FormData(e.target));
  // The tunnel destination is resolved on the server from the VM's live
  // display info (virsh domdisplay); the client only names which display
  // to view. There is deliberately no user-supplied host:port — that
  // would let the tunnel reach any TCP service from the server host.
  const p = '?display=spice';
  spiceStatus('Connecting…', { reconnect: false });
  // A failed/dropped session is retried automatically (a bounded
  // number of page reloads — see retry.js). SPICE may report one
  // failure through several events (error, then an "unexpected
  // close"), so report only the first.
  let reported = false;
  // Test/debug hook (parity with the console and VNC pages): the live
  // connection, readable from DevTools and the e2e suite.
  window.__spice = new SpiceMainConn({
    uri: cfg.tunnelBase + '/stream' + p,
    inputUri: cfg.tunnelBase + '/input' + p,
    password: q.get('password') || '',
    screen_id: 'spice',
    message_id: 'spice-messages',
    onsuccess: () => {
      clearRetries();
      spiceStatus('Connected.', { reconnect: false, connected: true });
      if (formEl) formEl.hidden = true;
    },
    onerror: (err) => {
      if (reported) return;
      reported = true;
      const base = 'SPICE error: ' + err.message;
      if (scheduleRetry()) {
        spiceStatus(base + ' — retrying shortly…', { failed: true, reconnect: true });
      } else {
        spiceStatus(base + ' — retry limit reached.', { failed: true, reconnect: true });
      }
    },
  });
});

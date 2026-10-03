// SPICE viewer: spice-html5 fork (third-party/spice-html5-fork) over
// the tunnel endpoints. Each SPICE channel gets its own
// (GET stream + POST input) session, i.e. its own ssh -W tunnel —
// the same shape as the WebSocket-per-channel websockify topology.
import { SpiceMainConn } from '/spice/src/main.js';

const cfg = JSON.parse(document.getElementById('spice-cfg').textContent);
const statusEl = document.getElementById('spice-status');
const reconnectBtn = document.getElementById('spice-reconnect');
if (reconnectBtn) reconnectBtn.addEventListener('click', () => location.reload());
function spiceStatus(msg, { failed = false, reconnect = false } = {}) {
  statusEl.textContent = msg;
  statusEl.classList.toggle('status-err', failed);
  if (reconnectBtn) reconnectBtn.hidden = !reconnect;
}

document.getElementById('spice-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const q = new URLSearchParams(new FormData(e.target));
  let addr = (q.get('address') || '127.0.0.1:5991').trim();
  // A bare port number means the server host's loopback (where SPICE
  // normally listens); anything else is taken as host:port.
  if (/^\d+$/.test(addr)) addr = '127.0.0.1:' + addr;
  const p = '?target=' + encodeURIComponent(addr);
  spiceStatus('Connecting…', { reconnect: false });
  new SpiceMainConn({
    uri: cfg.tunnelBase + '/stream' + p,
    inputUri: cfg.tunnelBase + '/input' + p,
    password: q.get('password') || '',
    screen_id: 'spice',
    message_id: 'spice-messages',
    onsuccess: () => { spiceStatus('Connected.', { reconnect: false }); },
    onerror: (err) => {
      spiceStatus('SPICE error: ' + err.message, { failed: true, reconnect: true });
    },
  });
});

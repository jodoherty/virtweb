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
      screen.style.height = '';
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

// --------------------------------------------------------------------
// Viewer preferences (audio / guest resize / zoom), persisted per
// browser in localStorage: they are the user's display habits, not
// per-VM state.
// --------------------------------------------------------------------
const settings = {
  audio: localStorage.getItem('vw.spice.audio') !== '0',
  guestResize: localStorage.getItem('vw.spice.resize') === '1',
  zoom: Number(localStorage.getItem('vw.spice.zoom')) || 100,
};
// A persisted value outside the dropdown (hand-edited localStorage)
// would desync the select from the engine; snap back to 100.
if (![25, 33, 50, 66, 75, 100, 150, 200].includes(settings.zoom))
  settings.zoom = 100;
const screenEl = document.getElementById('spice');
const audioTgl = document.getElementById('spice-audio');
const resizeTgl = document.getElementById('spice-guestresize');
const zoomSel = document.getElementById('spice-zoom');
audioTgl.checked = settings.audio;
resizeTgl.checked = settings.guestResize;
zoomSel.value = String(settings.zoom);

// Zoom: the canvas element is sized in *logical* CSS pixels — fitted
// (contain) size times the zoom factor — so 100% looks identical on
// HiDPI and normal-DPI displays. At zoom > 100% the element overflows
// the display area and is clipped centred (the area is overflow:hidden);
// at < 100% it letterboxes with extra margin. The bitmap size comes
// from the canvas width/height attributes, which the fork updates on
// every SURFACE_CREATE; the container size from its own box.
function activeCanvas() {
  const cs = screenEl.querySelectorAll('canvas');
  return cs.length ? cs[cs.length - 1] : null;
}
function applyZoom() {
  const c = activeCanvas();
  if (!c || !c.width || !c.height) return;
  const aw = screenEl.clientWidth, ah = screenEl.clientHeight;
  if (!aw || !ah) return;
  const fit = Math.min(aw / c.width, ah / c.height);
  const z = settings.zoom / 100;
  c.style.width = `${Math.round(c.width * fit * z)}px`;
  c.style.height = `${Math.round(c.height * fit * z)}px`;
}
new ResizeObserver(applyZoom).observe(screenEl);
// Fires for new canvases (childList), guest resolution changes
// (width/height attributes), and anything the fork re-parents.
new MutationObserver(applyZoom).observe(screenEl, {
  childList: true,
  subtree: true,
  attributes: true,
  attributeFilter: ['width', 'height'],
});
zoomSel.addEventListener('change', () => {
  settings.zoom = Number(zoomSel.value);
  localStorage.setItem('vw.spice.zoom', String(settings.zoom));
  applyZoom();
});

// Audio: the fork creates its <audio> element when the guest first
// starts sending (PLAYBACK_START, on the playback channel). The
// toggle mutes it; watching the screen for the element means "audio
// off" also holds from before connect through to the first sound.
function applyAudio() {
  const a = window.__spice?.playback?.audio;
  if (!a) return;
  a.muted = !settings.audio;
  if (settings.audio && a.paused) a.play().catch(() => {});
}
new MutationObserver(applyAudio).observe(screenEl, { childList: true });
audioTgl.addEventListener('change', () => {
  settings.audio = audioTgl.checked;
  localStorage.setItem('vw.spice.audio', settings.audio ? '1' : '0');
  applyAudio();
});

// Guest resize: ask the vdagent to resize the guest display to this
// window's logical size (1 guest px = 1 CSS px at 100% zoom), so the
// display fills the area at native resolution. Off by default; sent
// on agent (re)connect, when the toggle is turned on, and when the
// window settles (debounced). resize_window queues the MonConfig
// until the agent is up, so no extra guarding is needed.
function guestResize() {
  const sp = window.__spice;
  if (!sp || !settings.guestResize) return;
  let w = screenEl.clientWidth || window.innerWidth;
  let h = screenEl.clientHeight || window.innerHeight;
  w -= w % 8; // Xorg requires multiples of 8 (matches the fork's resize_helper)
  h -= h % 8;
  if (w < 64 || h < 48) return;
  sp.resize_window(0, w, h, 32, 0, 0);
  const st = document.getElementById('spice-status');
  if (st && !st.classList.contains('status-err'))
    // Whether the guest actually follows depends on its display server:
    // X11/Windows+QXL resize; Wayland sessions ignore MonConfig.
    st.textContent = `Connected. Guest resize requested (${w}×${h}).`;
}
let guestResizeTimer;
window.addEventListener('resize', () => {
  if (!settings.guestResize) return;
  clearTimeout(guestResizeTimer);
  guestResizeTimer = setTimeout(guestResize, 400);
});
resizeTgl.addEventListener('change', () => {
  settings.guestResize = resizeTgl.checked;
  localStorage.setItem('vw.spice.resize', settings.guestResize ? '1' : '0');
  if (settings.guestResize) guestResize();
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
    // The fork calls this when the guest agent connects (and again on
    // each reconnect): the moment a MonConfig can actually be acted on.
    onagent: () => guestResize(),
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

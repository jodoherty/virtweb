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

// Clipboard: explicit operations. The fork no longer syncs the clipboard
// in the background (that made the page call the Clipboard API on every
// guest focus change with no user action — on Firefox that surfaced a
// stray "Paste" affordance in the browser UI). Copy pulls the guest
// clipboard into the local one; Paste pushes the local clipboard into
// the guest. Both need a live session with the guest agent connected.
function clipboardConn() {
  const sp = window.__spice;
  if (!sp || !sp.agent_connected) {
    spiceStatus('Not connected — the guest clipboard is unavailable.', { failed: true });
    return null;
  }
  return sp;
}
document.getElementById('spice-copy')?.addEventListener('click', () => {
  const sp = clipboardConn();
  if (!sp) return;
  sp.log_info('Copy: requesting the guest clipboard…');
  sp.copy_remote_clipboard();
});
document.getElementById('spice-paste')?.addEventListener('click', () => {
  const sp = clipboardConn();
  if (!sp) return;
  sp.log_info('Paste: sending the local clipboard to the guest…');
  sp.paste_to_remote_clipboard();
});

// --------------------------------------------------------------------
// Viewer preferences. Guest resize and zoom persist per browser in
// localStorage: they are display habits, not per-VM state. Audio does
// not: it is off by default on every page open, so enabling it is a
// per-session opt-in — which also satisfies Firefox's autoplay policy
// (checking the box is a user gesture).
// --------------------------------------------------------------------
const settings = {
  audio: false,
  guestResize: localStorage.getItem('vw.spice.resize') === '1',
  zoom: Number(localStorage.getItem('vw.spice.zoom')) || 100,
};
// A persisted value outside the dropdown (hand-edited localStorage)
// would desync the select from the engine; snap back to 100.
if (![25, 33, 50, 66, 75, 100, 150, 200].includes(settings.zoom))
  settings.zoom = 100;
const screenEl = document.getElementById('spice');
// The display area is the remote screen, not the page: the host
// context menu (right-click, and on Firefox also a middle-click) must
// not pop up over it — the guest has its own context menus. The fork
// already suppresses it on the canvas; this covers the letterbox and
// zoom-overflow around it.
screenEl.addEventListener('contextmenu', (e) => e.preventDefault());
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
  let z = settings.zoom / 100;
  /* If the guest has followed the zoom-scaled resolution request
     (guest resize on), the density is already baked into the bitmap:
     fitting it to the area again would apply the zoom a second time
     (the view shrinks by z^2 instead of z, while the guest doubles its
     resolution). Bitmap ≈ the last request is the signal that it
     followed (guests may round to a nearby mode). */
  const rq = lastGuestRequest;
  if (
    rq &&
    Math.abs(c.width - rq[0]) <= 0.05 * rq[0] &&
    Math.abs(c.height - rq[1]) <= 0.05 * rq[1]
  )
    z = 1;
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
  // The requested guest resolution scales with the zoom.
  scheduleGuestResize();
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
  settings.audio = audioTgl.checked; // per-session opt-in; not persisted
  applyAudio();
});

// Guest resize: ask the vdagent to resize the guest display to the
// logical pixels available, *scaled by the zoom*: at 100% the guest
// fills the display area (1 guest px = 1 CSS px), at 50% it is 2x
// larger along each axis (the zoom then magnifies a larger canvas),
// at 200% it is half as large. Off by default; sent on agent
// (re)connect, when the toggle is turned on, on zoom changes, and
// when the window settles (debounced). resize_window queues the
// MonConfig until the agent is up, so no extra guarding is needed.
// [w, h] of the last zoom-scaled MonConfig the page sent; used by
// applyZoom to tell "the guest followed" (cancel the zoom factor) from
// "the guest has a fixed resolution" (zoom is a plain magnifier).
let lastGuestRequest = null;
function guestResize() {
  const sp = window.__spice;
  if (!sp || !settings.guestResize) return;
  const z = settings.zoom / 100;
  let w = (screenEl.clientWidth || window.innerWidth) / z;
  let h = (screenEl.clientHeight || window.innerHeight) / z;
  w = Math.floor(w / 8) * 8; // Xorg requires multiples of 8 (the fork's resize_helper does the same)
  h = Math.floor(h / 8) * 8;
  if (w < 64 || h < 48) return;
  sp.resize_window(0, w, h, 32, 0, 0);
  lastGuestRequest = [w, h];
  const st = document.getElementById('spice-status');
  if (st && !st.classList.contains('status-err'))
    // Whether the guest actually follows depends on its display server:
    // X11/Windows+QXL resize; Wayland sessions ignore MonConfig.
    st.textContent = `Connected. Guest resize requested (${w}×${h}).`;
}
let guestResizeTimer;
function scheduleGuestResize() {
  if (!settings.guestResize) return;
  clearTimeout(guestResizeTimer);
  guestResizeTimer = setTimeout(guestResize, 400);
}
window.addEventListener('resize', scheduleGuestResize);
resizeTgl.addEventListener('change', () => {
  settings.guestResize = resizeTgl.checked;
  localStorage.setItem('vw.spice.resize', settings.guestResize ? '1' : '0');
  // Off: the bitmap is fixed again, so zoom is a plain magnifier.
  if (!settings.guestResize) lastGuestRequest = null;
  else guestResize();
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

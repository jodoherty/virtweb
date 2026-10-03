// VNC viewer: noVNC 1.7.0 (vendored, third-party/novnc, unmodified,
// MPL-2.0) over the plain-HTTP tunnel endpoints — no websockify.
//
// noVNC needs no fork for this: its transport layer (core/websock.js)
// accepts any "raw channel" object via Websock.attach(), requiring an
// 8-property surface (send, close, binaryType, onerror, onmessage,
// onopen, protocol, readyState) — WebSocket and RTCDataChannel are the
// two reference examples. ChannelSocket below provides that surface
// over DuplexChannel (GET stream + ordered POST uplink), and RFB's
// constructor takes the channel directly.
import RFB from '/novnc/core/rfb.js';
import { DuplexChannel } from '/static/duplex.js';

class ChannelSocket {
  constructor(ch) {
    // The raw-channel surface (websock.js rawChannelProps).
    // readyState uses the RTCDataChannel string states, which
    // websock.js understands alongside the numeric WebSocket ones.
    // onmessage is a setter: websock.js assigns it during attach(),
    // and chunks that arrive before the assignment are queued.
    this.protocol = '';
    this.binaryType = '';
    this.onopen = null;
    this.onclose = null;
    this.onerror = null;
    this.readyState = 'connecting';
    this._ch = ch;
    this._queued = [];
    this._onmessage = null;

    ch.open().then(() => {
      if (this.readyState === 'connecting') {
        this.readyState = 'open';
        this.onopen?.();
      }
    }).catch((e) => this._close(e));
    ch.closed.then(() => this._close(null));

    // Downlink: ReadableStream chunks -> message events.
    (async () => {
      try {
        for await (const value of ch.readable) {
          this._deliver(value);
        }
      } catch {
        /* the close path handles the error state */
      }
    })();
  }

  get onmessage() {
    return this._onmessage;
  }

  set onmessage(fn) {
    this._onmessage = fn;
    while (this._queued.length && this._onmessage) {
      this._onmessage({ data: this._queued.shift() });
    }
  }

  _deliver(data) {
    if (this._onmessage) this._onmessage({ data });
    else this._queued.push(data);
  }

  _close(err) {
    if (this.readyState === 'closed') return;
    this.readyState = 'closed';
    if (err) this.onerror?.(err);
    this.onclose?.({
      code: err ? 1006 : 1000,
      reason: err ? String(err.message || err) : '',
    });
  }

  send(chunk) {
    if (this.readyState !== 'open') return;
    // A failed POST kills the channel, which closes the socket here.
    this._ch.write(chunk).catch(() => {});
  }

  close() {
    if (this.readyState === 'connecting' || this.readyState === 'open') {
      this._ch.close();
    }
  }
}

const cfg = JSON.parse(document.getElementById('vnc-cfg').textContent);
const statusEl = document.getElementById('vnc-status');

if (!cfg.streamUrl) {
  statusEl.textContent = cfg.reason || 'VNC is not available for this VM.';
  throw new Error('no VNC available'); // stop the module
}

const ch = new DuplexChannel(cfg.streamUrl, cfg.inputUrl);
window.__vncChannel = ch; // e2e hook

// RFB's constructor kicks the connection off (state 'connecting' →
// _connect → Websock.attach), so construct = connect.
const rfb = new RFB(document.getElementById('vnc'), new ChannelSocket(ch), {
  shared: false,
  credentials: { password: cfg.password || '' },
});
window.__rfb = rfb; // e2e hook

rfb.addEventListener('connect', () => {
  statusEl.textContent = 'Connected.';
});
rfb.addEventListener('disconnect', (e) => {
  statusEl.textContent = e.detail.clean ? 'Disconnected.' : 'Connection lost.';
});
rfb.addEventListener('credentials-required', (e) => {
  const pw = window.prompt(e.detail?.title || 'VNC password:');
  if (pw !== null) rfb.sendCredentials({ password: pw });
});

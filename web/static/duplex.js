// DuplexChannel — virtweb's browser-side duplex channel over plain HTTP.
//
// A duplex channel is two byte streams bound to one server-side session:
//
//   downlink: GET streamUrl — the response body *is* the ReadableStream
//   uplink:   the WritableStream we expose drains into POST inputUrl,
//             one POST per chunk, in a strictly ordered chain
//
// Design notes:
//
// * No WebSocket, no websockify, and no duplex fetch (Firefox does not
//   support it). The server endpoints only need a chunked, flushed,
//   unbuffered (X-Accel-Buffering: no) GET response and small POSTs.
// * HTTP/2 is the preferred transport, and nothing here fights it: both
//   directions are ordinary requests, so the browser multiplexes them
//   over one connection and per-stream reset (reader.cancel / fetch
//   abort) gives clean teardown. (Under HTTP/1.1 the same code works
//   over the browser's connection pool.)
// * Ordering: h2 preserves order only *within* a stream, so the uplink
//   chain serializes the POSTs head-of-line (chunk N goes out only
//   after N-1 completes). That — not the connection — is the ordering
//   guarantee, and it holds on both h1 and h2.
// * Lifecycle: open() resolves when the GET response headers arrive
//   (the session is live on the server, so POSTs are accepted); it
//   rejects with code 'conflict' when a session is already open (409)
//   or on any other failure. Writes before open are queued and flushed
//   in order. close() aborts the GET, which ends the server session
//   through the request context. A failed uplink POST or downlink
//   failure terminates the whole channel.
//
// Wire protocol (v1; mirrored server-side in internal/api/frame.go and
// in the e2e harness in e2e/src/frames.js): every direction speaks
// length-framed, tagged frames — [tag:1][len:4 big-endian][payload].
// Tags: HELLO (0x01, downlink, first frame, [version][flags] payload),
// DATA (0x02, the session's raw bytes), KEEPALIVE (0x03, empty, idle
// liveness). Each uplink POST body is exactly one frame. The framing
// exists so (a) idle streams can't trip a reverse proxy's read/idle
// timeouts — each direction emits KEEPALIVEs while silent — and (b) a
// corrupted/truncated stream is detectable instead of silently
// desyncing the guest protocol. Consumers of this channel (xterm,
// noVNC, the SPICE fork) only ever see DATA payloads.

const FRAME = {
  hello: 0x01,
  data: 0x02,
  keepalive: 0x03,
};
const PROTO_VERSION = 1;
const MAX_FRAME_PAYLOAD = 16 * 1024 * 1024; // parser bound, not a perf knob

// encodeFrame builds one [tag][len:4 BE][payload] frame.
export function encodeFrame(tag, payload) {
  const out = new Uint8Array(5 + payload.length);
  out[0] = tag;
  out[1] = (payload.length >>> 24) & 0xff;
  out[2] = (payload.length >>> 16) & 0xff;
  out[3] = (payload.length >>> 8) & 0xff;
  out[4] = payload.length & 0xff;
  out.set(payload, 5);
  return out;
}

// FrameDecoder reassembles frames from raw stream chunks and reports
// their DATA payloads (HELLO is validated once, KEEPALIVEs dropped).
// It throws on a torn/oversized/unknown frame — the caller fails the
// channel, which surfaces as "connection lost" to the UI.
class FrameDecoder {
  constructor() {
    this.pending = null; // Uint8Array: not-yet-framed trailing bytes
    this.helloSeen = false;
  }
  feed(chunk) {
    const old = this.pending;
    const buf = new Uint8Array((old ? old.length : 0) + chunk.length);
    if (old) buf.set(old, 0);
    buf.set(chunk, old ? old.length : 0);
    this.pending = buf;
    const payloads = [];
    for (;;) {
      if (this.pending.length < 5) break;
      const p = this.pending;
      const tag = p[0];
      const len = ((p[1] << 24) | (p[2] << 16) | (p[3] << 8) | p[4]) >>> 0;
      if (len > MAX_FRAME_PAYLOAD) {
        throw new Error('frame payload exceeds the maximum');
      }
      if (this.pending.length < 5 + len) break; // incomplete frame
      const payload = this.pending.slice(5, 5 + len); // copy
      // Re-slice from the head; the next feed() re-copies, so the large
      // backing array is released.
      this.pending = this.pending.subarray(5 + len);
      switch (tag) {
        case FRAME.hello:
          if (this.helloSeen) throw new Error('duplicate HELLO frame');
          if (payload.length !== 2 || payload[0] !== PROTO_VERSION) {
            throw new Error(
              `unsupported protocol version ${payload.length ? payload[0] : '(none)'}`,
            );
          }
          this.helloSeen = true;
          break;
        case FRAME.data:
          payloads.push(payload);
          break;
        case FRAME.keepalive:
          break; // idle liveness; nothing to deliver
        default:
          throw new Error('unknown frame tag 0x' + tag.toString(16));
      }
    }
    return payloads;
  }
}

function toBytes(chunk) {
  if (chunk instanceof Uint8Array) return chunk;
  if (chunk instanceof ArrayBuffer) return new Uint8Array(chunk);
  return new Uint8Array(chunk); // other BufferSource (any TypedArray)
}

export class DuplexChannel {
  /**
   * @param {string} streamUrl GET endpoint returning the downlink bytes
   * @param {string} inputUrl  POST endpoint accepting uplink byte bodies
   */
  constructor(streamUrl, inputUrl, options = {}) {
    if (!streamUrl) throw new Error('DuplexChannel: streamUrl required');
    if (!inputUrl) throw new Error('DuplexChannel: inputUrl required');
    this.streamUrl = streamUrl;
    this.inputUrl = inputUrl;
    // connectTimeoutMs bounds how long open() may wait for the GET
    // response headers before giving up (0 disables the bound).
    this.connectTimeoutMs = options.connectTimeoutMs ?? 20_000;
    // keepaliveMs: emit an idle uplink KEEPALIVE frame after this much
    // silence (covers client-leg idle timeouts; the server pings the
    // downlink on its own). 0 disables.
    this.keepaliveMs = options.keepaliveMs ?? 15_000;

    this._ctrl = null;
    this._opened = false;
    this._closed = false; // terminal: no more sends or reads
    this._reason = ''; // terminal closeReason: '' | 'ended' | 'error' | 'closed'
    this._sid = null; // server-assigned session id (X-Tunnel-Id), if any
    this._openP = null;
    this._openTimer = null;
    this._openResolve = null;
    this._openReject = null;
    this._kaTimer = null; // uplink keepalive interval
    this._lastUplink = 0; // last uplink send (for idle gating)

    // Uplink: chunks waiting for open, plus the serialized POST chain
    // (head-of-line ordering; see file comment).
    this._pending = [];
    this._chain = Promise.resolve();

    // Downlink: ReadableStream fed from the GET response body.
    this.readable = new ReadableStream({
      start: (c) => { this._sink = c; },
      cancel: () => this._abortGet(),
    });

    // Uplink: WritableStream over the ordered POST chain.
    this.writable = new WritableStream({
      write: (chunk) => this.write(chunk),
      close: async () => { await this._chain; },
      abort: () => { this.close(); },
    });

    // Settles (with undefined or an error) when the downlink fully
    // ends — normal EOF, failure, or close().
    this.closed = new Promise((res) => { this._doneRes = res; });
  }

  /** 'connecting' | 'open' | 'closed' */
  get state() {
    if (this._closed) return 'closed';
    return this._opened ? 'open' : 'connecting';
  }

  // '' while open; 'ended' (clean server EOF), 'error' (the channel
  // failed) or 'closed' (the client closed it) once terminal. The UI
  // uses this to tell "session ended" from "connection lost".
  get closeReason() {
    return this._reason;
  }

  /**
   * Starts the downlink GET. Resolves when the response headers arrive
   * (session live server-side, uplink POSTs accepted). The server may
   * name the session via the X-Tunnel-Id response header; that id is
   * captured and appended to every uplink POST so a client that opens
   * several concurrent sessions (e.g. SPICE's per-channel tunnels) can
   * route each uplink to the right one. Rejects with a
   * `code: 'conflict'` error on 409, or on any other failure.
   */
  open() {
    if (this._openP) return this._openP;
    this._openP = new Promise((resolve, reject) => {
      this._openResolve = resolve;
      this._openReject = reject;
      this._ctrl = new AbortController();
      if (this.connectTimeoutMs > 0) {
        // Give up if the session headers never arrive (server accepted
        // the connection but the tunnel/session never came up).
        this._openTimer = setTimeout(() => {
          if (this._opened || this._closed) return;
          const err = new Error('connection timed out');
          err.code = 'timeout';
          this._fail(err);
          reject(err);
        }, this.connectTimeoutMs);
      }
      fetch(this.streamUrl, { signal: this._ctrl.signal })
        .then((res) => this._onHeaders(res))
        .catch((err) => {
          if (!this._opened && !this._closed) this._fail(err);
          reject(err);
        });
    });
    this._openP.finally(() => {
      if (this._openTimer) clearTimeout(this._openTimer);
    });
    return this._openP;
  }

  async _onHeaders(res) {
    // The session handle for uplink POSTs, if the endpoint provides one.
    this._sid = res.headers.get('x-tunnel-id');
    if (res.status === 409) {
      const err = new Error('a session is already open');
      err.code = 'conflict';
      await res.body?.cancel().catch(() => {});
      this._openReject(err);
      return;
    }
    if (!res.ok || !res.body) {
      const err = new Error('HTTP ' + res.status + ' opening stream');
      await res.body?.cancel().catch(() => {});
      this._openReject(err);
      return;
    }
    this._opened = true;
    this._openResolve();
    this._startKeepalive();
    this._flushPending();
    this._pumpDownlink(res.body);
  }

  _pumpDownlink(body) {
    const reader = body.getReader();
    this._reader = reader;
    const frames = new FrameDecoder();
    (async () => {
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const payload of frames.feed(value)) {
            if (!this._closed) this._sink.enqueue(payload);
          }
        }
        if (!this._closed) {
          this._reason = 'ended';
          this._sink.close();
        }
      } catch (err) {
        this._fail(err);
        return;
      }
      this._doneRes(undefined);
    })();
  }

  /**
   * Enqueue bytes on the uplink. Resolves once delivered to the server
   * (the POST is accepted); rejects if the channel fails or closes
   * first. Writes before open() are queued and flushed in order.
   */
  write(chunk) {
    return new Promise((resolve, reject) => {
      const item = { bytes: toBytes(chunk), resolve, reject };
      if (this._closed) { reject(new Error('channel is closed')); return; }
      if (this._opened) this._send(item);
      else this._pending.push(item);
    });
  }

  _flushPending() {
    const items = this._pending;
    this._pending = [];
    for (const item of items) this._send(item);
  }

  // One POST, chained after the previous one so delivery order is
  // preserved regardless of HTTP version or connection reuse. A failed
  // POST means the session is gone: fail the whole channel.
  _send(item) {
    const p = this._chain.then(async () => {
      if (this._closed) throw new Error('channel is closed');
      // The uplink speaks frames too (protocol v1): wrap the payload in
      // a DATA frame (keepalives are sent as their own frames).
      const res = await fetch(this._inputUrlFor(), {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: item.frame ?? encodeFrame(FRAME.data, item.bytes),
        keepalive: true,
      });
      if (!res.ok) throw new Error('uplink rejected: HTTP ' + res.status);
      await res.body?.cancel().catch(() => {});
    });
    p.then(
      () => {
        this._lastUplink = Date.now();
        item.resolve();
      },
      (err) => { item.reject(err); this._fail(err); },
    );
    this._chain = p.catch(() => {});
  }

  // Uplink keepalives: an idle client is invisible to the network, so
  // while the channel is open and no uplink has gone out for
  // keepaliveMs, send an empty KEEPALIVE frame. This keeps the client
  // leg of the connection warm (NATs, mobile OSes, proxy idle
  // timeouts); the server pings the downlink on its own. Hidden-tab
  // timer throttling degrades this to ~1/min, which is still useful.
  _startKeepalive() {
    if (this.keepaliveMs <= 0) return;
    const tick = Math.max(this.keepaliveMs / 2, 250);
    this._lastUplink = Date.now();
    this._kaTimer = setInterval(() => {
      if (!this._opened || this._closed) {
        this._stopKeepalive();
        return;
      }
      if (Date.now() - this._lastUplink < this.keepaliveMs) return;
      this._lastUplink = Date.now();
      this._send({
        bytes: new Uint8Array(0),
        frame: encodeFrame(FRAME.keepalive, new Uint8Array(0)),
        resolve: () => {},
        reject: () => {},
      });
    }, tick);
    if (typeof this._kaTimer.unref === 'function') this._kaTimer.unref();
  }

  _stopKeepalive() {
    if (this._kaTimer) {
      clearInterval(this._kaTimer);
      this._kaTimer = null;
    }
  }

  // The uplink URL for the current session: the base inputUrl with the
  // server-assigned session id appended (if the endpoint provides one).
  _inputUrlFor() {
    if (!this._sid) return this.inputUrl;
    const sep = this.inputUrl.includes('?') ? '&' : '?';
    return this.inputUrl + sep + 'id=' + encodeURIComponent(this._sid);
  }

  /**
   * Tear down: stop accepting input and abort the GET. The server
   * ends the session when it notices the disconnect.
   */
  close() {
    if (this._closed) return;
    this._closed = true;
    this._reason = 'closed';
    this._stopKeepalive();
    this._abortGet();
    this._doneRes(undefined);
  }

  _fail(err) {
    if (this._closed) return;
    this._closed = true;
    this._reason = 'error';
    this._stopKeepalive();
    try {
      if (!this._sink.closed) this._sink.error(err);
    } catch { /* sink already settled */ }
    this._abortGet();
    this._doneRes(err);
  }

  _abortGet() {
    if (this._ctrl) {
      this._ctrl.abort();
      this._ctrl = null;
    }
  }
}

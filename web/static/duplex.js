// DuplexChannel — virtweb's browser-side duplex channel over WebSocket.
//
// A duplex channel is two byte streams bound to one server-side session
// (a serial console or a TCP tunnel channel):
//
//   downlink: the WebSocket's binary messages, surfaced as a
//             ReadableStream
//   uplink:   the WritableStream we expose, each chunk sent as one
//             binary WebSocket message
//
// Design notes:
//
// * The server endpoint is the same URL for the whole session:
//   browsers negotiate HTTP/2 (extended CONNECT, RFC 8441) over ALPN
//   on the TLS listener and all of a tab's sessions ride one h2
//   connection; the plain HTTP/1.1 Upgrade is the fallback leg.
//   Nothing here is protocol-specific — `new WebSocket` is all of it.
// * Liveness: the server probes a silent session with pings (the
//   browser answers them automatically), so no client-side keepalive
//   timers are needed — and none are throttled in hidden tabs.
// * Terminal state: the session's close code carries it. A clean end
//   is 1000 (closeReason 'ended'); a failed end is 1011 and the close
//   reason is the server's message (terminalMessage); a transport
//   death (1006, proxy cut, backend restart) is 'error'. Consumers
//   (xterm, noVNC, the SPICE fork) only ever see downlink bytes and
//   the terminal state.
// * Lifecycle: open() does a HEAD preflight (the browser's WebSocket
//   API does not surface the handshake status, so the server's 409
//   "session in use" / 429 / 503 must be read before the handshake),
//   then the handshake itself. It rejects with code 'conflict' on 409
//   or on any other failure. Writes before open are queued and sent in
//   order. close() sends a polite 1000 close; the server ends the
//   session when it notices the disconnect either way.

// toWS rewrites an http(s) endpoint URL to its ws(s) form.
function toWS(url) {
  const u = new URL(url, location.origin);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.href;
}

export class DuplexChannel {
  /**
   * @param {string} streamUrl the session endpoint (same URL for the
   *   whole session; a relative path is resolved against the origin)
   * @param {{connectTimeoutMs?: number}} [options]
   */
  constructor(streamUrl, options = {}) {
    if (!streamUrl) throw new Error('DuplexChannel: streamUrl required');
    this.streamUrl = streamUrl;
    // connectTimeoutMs bounds how long open() may wait for the session
    // to come up (preflight + handshake). 0 disables the bound.
    this.connectTimeoutMs = options.connectTimeoutMs ?? 20_000;

    this._ws = null;
    this._opened = false;
    this._closed = false; // terminal: no more sends or reads
    this._reason = ''; // terminal closeReason: '' | 'ended' | 'error' | 'closed'
    this._terminalMessage = ''; // the server's terminal message (close reason)
    this._openP = null;
    this._openTimer = null;
    this._openResolve = null;
    this._openReject = null;

    // Uplink: chunks waiting for open, sent in order.
    this._pending = [];

    // Downlink: ReadableStream fed from the ws binary messages.
    this.readable = new ReadableStream({
      start: (c) => { this._sink = c; },
      cancel: () => this.close(),
    });

    // Uplink: WritableStream over ws.send.
    this.writable = new WritableStream({
      write: (chunk) => this.write(chunk),
      close: async () => {},
      abort: () => { this.close(); },
    });

    // Settles (with undefined or an error) when the downlink fully
    // ends — normal close, failure, or close().
    this.closed = new Promise((res) => { this._doneRes = res; });
  }

  /** 'connecting' | 'open' | 'closed' */
  get state() {
    if (this._closed) return 'closed';
    return this._opened ? 'open' : 'connecting';
  }

  // '' while open; 'ended' (clean 1000 close), 'error' (a failed
  // session — terminalMessage carries the server's message when the
  // server had one) or 'closed' (the client closed it) once terminal.
  // The UI uses this to tell "session ended" from "connection lost".
  get closeReason() {
    return this._reason;
  }

  /**
   * The server's terminal message: what the 1011 close reason
   * carried, or '' for clean ends / transport-level failures that
   * produced no server message.
   */
  get terminalMessage() {
    return this._terminalMessage;
  }

  /**
   * Whether the page's connection to this origin negotiated HTTP/2
   * (from the navigation entry — a reliable proxy for the session's
   * own ALPN outcome, since both ride the same origin). The UI shows
   * it as a marker so an h1 session is visible at a glance.
   */
  get http2() {
    try {
      const nav = performance.getEntriesByType('navigation')[0];
      return nav?.nextHopProtocol === 'h2';
    } catch {
      return true; // no timing info: assume the happy path
    }
  }

  /**
   * Preflights then opens the session. Resolves when the session is
   * live (handshake complete, uplink accepted). Rejects with
   * `code: 'conflict'` on 409 (a session is already open) or with the
   * status in the message on any other preflight/handshake failure.
   */
  open() {
    if (this._openP) return this._openP;
    this._openP = new Promise((resolve, reject) => {
      this._openResolve = resolve;
      this._openReject = reject;
      if (this.connectTimeoutMs > 0) {
        // Give up if the session never comes up (server accepted the
        // request but the session never established — e.g. the backend
        // ssh/tunnel setup hangs).
        this._openTimer = setTimeout(() => {
          if (this._opened || this._closed) return;
          const err = new Error('connection timed out');
          err.code = 'timeout';
          this._fail(err);
          reject(err);
        }, this.connectTimeoutMs);
      }
      // The browser's WebSocket API does not surface the handshake
      // status, so read the pre-upgrade status first.
      fetch(this.streamUrl, { method: 'HEAD' })
        .then((res) => {
          if (res.status === 409) {
            const err = new Error('a session is already open');
            err.code = 'conflict';
            this._fail(err);
            return;
          }
          if (!res.ok) {
            const err = new Error('HTTP ' + res.status + ' opening session');
            err.code = 'http';
            this._fail(err);
            return;
          }
          this._handshake();
        })
        .catch((err) => {
          const e = new Error('preflight failed: ' + err.message);
          this._fail(e);
          reject(e);
        });
    });
    this._openP.finally(() => {
      if (this._openTimer) clearTimeout(this._openTimer);
    });
    return this._openP;
  }

  _handshake() {
    const ws = new WebSocket(toWS(this.streamUrl));
    this._ws = ws;
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
      if (this._closed) return;
      this._opened = true;
      this._openResolve();
      this._flushPending();
    };

    // Downlink: binary messages are enqueued on the ReadableStream the
    // consumer iterates. A cancelled/ended sink rejects the enqueue —
    // the session is over, so drop the bytes.
    ws.onmessage = (ev) => this._onData(ev);

    // onError precedes onClose (which always follows); the close
    // event is the single place terminal state is recorded.
    ws.onerror = () => {};

    ws.onclose = (ev) => this._onClose(ev.code, ev.reason);
  }

  _onData(ev) {
    if (this._closed) return;
    try {
      this._sink.enqueue(ev.data);
    } catch {
      /* sink already ended (consumer cancelled or stream closed) */
    }
  }

  _onClose(code, reason) {
    if (this._closed) return;
    if (this._reason !== 'closed') {
      if (code === 1000) {
        // A clean end: the server closed the session normally.
        this._reason = 'ended';
      } else {
        // A failed session (1011 carries the server's message in the
        // close reason) or a transport-level death (1006 and friends:
        // proxy cut, backend restart, keepalive timeout).
        this._reason = 'error';
        if (code !== 1006 && reason) this._terminalMessage = reason;
      }
    }
    // A session can end before the browser fires 'open': a fast
    // server-side failure delivers the close frame before the client
    // handshake completes. The open promise must still settle, or the
    // consumer hangs on "Connecting…" until its timeout. A close frame
    // from the server means the session WAS established and is now over:
    // resolve, and let the consumer read the (already ended) stream to
    // the terminal state. A bare 1006 — nothing exchanged — means it
    // never came up: reject.
    if (!this._opened && this._openP) {
      if (code === 1006) {
        const err = new Error('connection failed before the session opened');
        err.code = 'http';
        this._fail(err);
        return; // _fail finished the channel
      }
      this._opened = true;
      if (this._openResolve) { this._openResolve(); this._openResolve = null; }
    }
    this._finish();
  }

  /**
   * Enqueue bytes on the uplink. Resolves once the bytes are handed
   * to the socket (ws.send queues them; per-connection ordering is
   * guaranteed by the transport). Rejects if the channel fails or
   * closes first. Writes before open() are queued and sent in order.
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

  _send(item) {
    try {
      this._ws.send(item.bytes);
      item.resolve();
    } catch (err) {
      item.reject(err);
      this._fail(err);
    }
  }

  /**
   * Tear down politely: send a 1000 close and stop accepting input.
   * The server ends the session when the connection closes.
   */
  close() {
    if (this._closed) return;
    this._closed = true;
    this._reason = 'closed';
    try {
      this._ws?.close(1000, 'client closed');
    } catch { /* already gone */ }
    this._finish();
  }

  _finish() {
    // Settle the writes queued before open: the channel is terminal,
    // so they will never be sent.
    for (const item of this._pending) item.reject(new Error('channel is closed'));
    this._pending = [];
    if (this._doneRes) {
      this._doneRes(undefined);
      this._doneRes = null;
    }
    try {
      if (!this._sink.closed && this._reason !== 'closed') {
        // Surface the terminal state to the downlink consumer: an
        // errored session rejects the stream (the consumer reports
        // closeReason/terminalMessage), a clean end closes it.
        if (this._reason === 'error') {
          this._sink.error(new Error(this._terminalMessage || 'connection lost'));
        } else {
          this._sink.close();
        }
      }
    } catch { /* sink already settled */ }
  }

  _fail(err) {
    if (this._closed) return;
    this._closed = true;
    this._reason = 'error';
    if (err.message) this._terminalMessage = err.message;
    // Settle the open promise if the channel never came up: a failure
    // before the handshake completes must reject open() rather than leave
    // it hanging until the connection timeout.
    if (!this._opened && this._openReject) {
      this._openReject(err);
      this._openReject = null;
      this._openResolve = null;
    }
    try { this._ws?.close(); } catch { /* already gone */ }
    this._finish();
  }
}

function toBytes(chunk) {
  if (chunk instanceof Uint8Array) return chunk;
  if (chunk instanceof ArrayBuffer) return new Uint8Array(chunk);
  return new Uint8Array(chunk); // other BufferSource (any TypedArray)
}

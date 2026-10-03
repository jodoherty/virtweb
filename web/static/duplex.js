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
  constructor(streamUrl, inputUrl) {
    if (!streamUrl) throw new Error('DuplexChannel: streamUrl required');
    if (!inputUrl) throw new Error('DuplexChannel: inputUrl required');
    this.streamUrl = streamUrl;
    this.inputUrl = inputUrl;

    this._ctrl = null;
    this._opened = false;
    this._closed = false; // terminal: no more sends or reads
    this._sid = null; // server-assigned session id (X-Tunnel-Id), if any
    this._openP = null;

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
      this._ctrl = new AbortController();
      fetch(this.streamUrl, { signal: this._ctrl.signal })
        .then((res) => this._onHeaders(res, resolve, reject))
        .catch((err) => {
          if (!this._opened && !this._closed) this._fail(err);
          reject(err);
        });
    });
    return this._openP;
  }

  async _onHeaders(res, resolve, reject) {
    // The session handle for uplink POSTs, if the endpoint provides one.
    this._sid = res.headers.get('x-tunnel-id');
    if (res.status === 409) {
      const err = new Error('a session is already open');
      err.code = 'conflict';
      await res.body?.cancel().catch(() => {});
      reject(err);
      return;
    }
    if (!res.ok || !res.body) {
      const err = new Error('HTTP ' + res.status + ' opening stream');
      await res.body?.cancel().catch(() => {});
      reject(err);
      return;
    }
    this._opened = true;
    resolve();
    this._flushPending();
    this._pumpDownlink(res.body);
  }

  _pumpDownlink(body) {
    const reader = body.getReader();
    this._reader = reader;
    (async () => {
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          this._sink.enqueue(value);
        }
        if (!this._closed) this._sink.close();
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
      const res = await fetch(this._inputUrlFor(), {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: item.bytes,
        keepalive: true,
      });
      if (!res.ok) throw new Error('uplink rejected: HTTP ' + res.status);
      await res.body?.cancel().catch(() => {});
    });
    p.then(
      () => item.resolve(),
      (err) => { item.reject(err); this._fail(err); },
    );
    this._chain = p.catch(() => {});
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
    this._abortGet();
    this._doneRes(undefined);
  }

  _fail(err) {
    if (this._closed) return;
    this._closed = true;
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

// A raw WebSocket session against a virtweb stream endpoint — the
// API-level twin of web/static/duplex.js (the browser's DuplexChannel).
// The endpoint is one URL for the whole session: binary messages down
// (session bytes), binary messages up (input); the session's terminal
// state is the close code/reason (1000 clean; 1011 + the server's
// message on failure; 1006 transport death).
//
// The pre-upgrade statuses (404/400/409/429/503) are read with a HEAD
// preflight first — the browser's WebSocket API (and Node's) does not
// surface the handshake status.
//
// Every byte in both directions is appended to logPath.

import { appendFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';

function toWS(url) {
  const u = new URL(url);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.href;
}

export class StreamSession {
  /**
   * @param {object} o
   * @param {string} o.baseUrl virtweb base URL (http or https)
   * @param {string} o.path the stream endpoint path (including query)
   * @param {string} [o.logPath] where to record both directions
   * @param {(chunk: Uint8Array) => void} [o.onChunk]
   */
  async open({ baseUrl, path: ep, logPath, onChunk }) {
    this.url = `${baseUrl}${ep}`;
    this.logPath = logPath;
    this.text = '';
    this.output = new TextDecoder();
    this.chunkCount = 0;
    this.terminal = null; // { code, reason } from the close event

    this._record = (label, bytes) => {
      if (!logPath) return;
      mkdirSync(path.dirname(logPath), { recursive: true });
      appendFileSync(
        logPath,
        label + '\n' + Buffer.from(bytes).toString('hex') + '  ' +
          JSON.stringify(new TextDecoder().decode(bytes)) + '\n',
      );
    };

    // Pre-upgrade status: the browser's WebSocket API cannot read the
    // handshake status, so it is probed first. 200 = the session may
    // open; anything else is the server's rejection.
    const pf = await fetch(this.url, { method: 'HEAD' });
    await pf.body?.cancel().catch(() => {});
    this.status = pf.status;
    if (this.status !== 200) {
      this.ws = null;
      this.done = Promise.resolve();
      this.close = async () => {};
      return this;
    }

    this.ws = new WebSocket(toWS(this.url));
    this.ws.binaryType = 'arraybuffer';
    this._record('== session open ==', new Uint8Array());

    // The WebSocket API opens asynchronously (and the handshake status
    // isn't surfaced, so a preflight-200 can still be rejected here —
    // e.g. another session takes the slot between the HEAD and the
    // handshake). Await the open event before the session is usable.
    let opened = false;
    this.openP = new Promise((res, rej) => {
      this.ws.onopen = () => {
        opened = true;
        res();
      };
      this._openRej = rej;
    });

    this.closedP = new Promise((res) => { this._closeRes = res; });
    this.ws.onmessage = (e) => {
      const payload = new Uint8Array(e.data);
      this._record('<<', payload);
      this.chunkCount++; // each binary message is one unit of arrival
      this.text += this.output.decode(payload, { stream: true });
      onChunk?.(payload);
    };
    this.ws.onclose = (e) => {
      this.terminal = { code: e.code, reason: e.reason ?? '' };
      this._record(`== close ${e.code} ${JSON.stringify(e.reason ?? '')} ==`, new Uint8Array());
      if (!opened) {
        this._openRej(new Error(`session did not open (closed ${e.code} ${JSON.stringify(e.reason ?? '')})`));
        return;
      }
      this._closeRes(this.terminal);
    };
    this.ws.onerror = () => { /* onclose follows and is the recorder */ };

    await this.openP;

    this.done = (async () => {
      await this.closedP;
      this.text += this.output.decode();
    })();
    this.close = async () => {
      this._record('== client close ==', new Uint8Array());
      try {
        this.ws.close(1000, 'test closed');
      } catch { /* already gone */ }
    };
    return this;
  }

  // Sends raw input bytes as one binary message. Resolves once handed
  // to the socket (per-connection ordering is the transport's job).
  // Throws if the session is not open.
  async input(bytes) {
    const data = typeof bytes === 'string' ? new TextEncoder().encode(bytes) : bytes;
    this._record?.('>>', data);
    if (!this.ws) throw new Error('session is not open (status ' + this.status + ')');
    this.ws.send(data);
  }

  // Lines of output seen so far (without trailing empty line).
  get lines() {
    return this.text.split('\n').filter((l) => l !== '');
  }

  // Resolves when the session has ended (any terminal close) and
  // returns the accumulated text. this.terminal carries {code, reason}.
  async closed(timeoutMs = 10000) {
    if (this.ws === null) return this.text; // refused pre-upgrade
    const timeout = new Promise((_, reject) => {
      const t = setTimeout(
        () => reject(new Error(`session did not end within ${timeoutMs}ms`)),
        timeoutMs,
      );
      t.unref?.();
    });
    await Promise.race([this.done, timeout]);
    return this.text;
  }
}

// preflightStatus returns the endpoint's pre-upgrade status (200
// openable, 409 in use, 429 capped, 400/503 display problems, 404
// unknown server) without opening a session.
export async function preflightStatus(url) {
  const res = await fetch(url, { method: 'HEAD' });
  await res.body?.cancel().catch(() => {});
  return res.status;
}

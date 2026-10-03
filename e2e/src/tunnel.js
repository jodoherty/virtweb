// Consumes a generic TCP tunnel stream (chunked GET → ReadableStream)
// and sends input (POST raw bytes) — the API-level twin of what
// DuplexChannel does in the browser. Mirrors ConsoleSession from
// console.js; every byte in both directions is appended to logPath.
import { appendFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';

export class TunnelSession {
  /**
   * @param {object} o
   * @param {string} o.baseUrl virtweb base URL
   * @param {string} o.serverId
   * @param {string} o.vm
   * @param {number|string} o.port tunnel port (or o.target)
   * @param {string} [o.target] full host:port target (overrides port)
   * @param {string} [o.logPath]
   * @param {(chunk: Uint8Array) => void} [o.onChunk]
   */
  async open({ baseUrl, serverId, vm, port, target, logPath, onChunk }) {
    const vmName = encodeURIComponent(vm);
    const q = target ? `?target=${encodeURIComponent(target)}` : `?port=${port}`;
    this.streamUrl = `${baseUrl}/api/servers/${serverId}/vms/${vmName}/tunnel/stream${q}`;
    this.logPath = logPath;
    this.text = '';
    this.output = new TextDecoder();
    this.chunkCount = 0;

    this.res = await fetch(this.streamUrl);
    this.status = this.res.status;
    this.sid = this.res.headers.get('x-tunnel-id');
    this.inputUrl = `${baseUrl}/api/servers/${serverId}/vms/${vmName}/tunnel/input?id=${this.sid ?? ''}`;
    this.headers = {
      'x-accel-buffering': this.res.headers.get('x-accel-buffering'),
      'cache-control': this.res.headers.get('cache-control'),
    };
    if (!this.res.ok || !this.res.body) {
      this.body = null;
      this.done = Promise.resolve();
      return this;
    }

    const reader = this.res.body.getReader();
    this._reader = reader;
    this._record = (label, bytes) => {
      if (!logPath) return;
      mkdirSync(path.dirname(logPath), { recursive: true });
      const text =
        label +
        '\n' +
        Buffer.from(bytes).toString('hex') +
        '  ' +
        JSON.stringify(new TextDecoder().decode(bytes)) +
        '\n';
      appendFileSync(logPath, text);
    };
    this._record('== session open ==', new Uint8Array());
    this.done = (async () => {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        this.chunkCount++;
        this.text += this.output.decode(value, { stream: true });
        onChunk?.(value);
        this._record('<<', value);
      }
      this.text += this.output.decode();
    })();
    this.close = async () => {
      this._record('== client disconnect ==', new Uint8Array());
      await reader.cancel().catch(() => {});
    };
    return this;
  }

  // Sends raw input bytes; returns the HTTP status of the POST.
  async input(bytes) {
    const data = typeof bytes === 'string' ? new TextEncoder().encode(bytes) : bytes;
    this._record?.('>>', data);
    const res = await fetch(this.inputUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: data,
    });
    await res.body?.cancel().catch(() => {});
    return res.status;
  }

  // Resolves when the server ends the stream.
  async closed(timeoutMs = 10000) {
    const timeout = new Promise((_, reject) => {
      const t = setTimeout(
        () => reject(new Error(`tunnel stream did not end within ${timeoutMs}ms`)),
        timeoutMs,
      );
      t.unref?.();
    });
    await Promise.race([this.done, timeout]);
    return this.text;
  }
}

export async function openTunnel(opts) {
  return await new TunnelSession().open(opts);
}

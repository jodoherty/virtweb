// Consumes the console output stream (chunked GET → ReadableStream) and
// sends keystrokes (POST raw bytes), mirroring what the browser does.
// Every byte in both directions is appended to logPath.
import { appendFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';
import { FrameDecoder, encodeFrame, TAG } from './frames.js';

export class ConsoleSession {
  /**
   * @param {object} o
   * @param {string} o.baseUrl virtweb base URL
   * @param {string} o.serverId
   * @param {string} o.vm
   * @param {string} [o.logPath] where to record both directions
   * @param {(chunk: Uint8Array) => void} [o.onChunk]
   */
  async open({ baseUrl, serverId, vm, logPath, onChunk }) {
    const vmName = encodeURIComponent(vm);
    this.streamUrl = `${baseUrl}/api/servers/${serverId}/vms/${vmName}/console/stream`;
    this.inputUrl = `${baseUrl}/api/servers/${serverId}/vms/${vmName}/console/input`;
    this.logPath = logPath;
    this.text = '';
    this.output = new TextDecoder();

    this.res = await fetch(this.streamUrl);
    this.status = this.res.status;
    if (!this.res.ok || !this.res.body) {
      this.body = null;
      this.done = Promise.resolve();
      return this;
    }

    const reader = this.res.body.getReader();
    this._reader = reader;
    // The downlink speaks the wire protocol: reassemble frames,
    // consume the DATA payloads; surface terminal frames (v2) on
    // this.terminal.
    this.terminal = null;
    this.frames = new FrameDecoder(undefined, (term) => {
      this.terminal = term;
    });
    this._record = (label, bytes) => {
      if (!logPath) return;
      mkdirSync(path.dirname(logPath), { recursive: true });
      const text = label + '\n' + Buffer.from(bytes).toString('hex') + '  ' + JSON.stringify(new TextDecoder().decode(bytes)) + '\n';
      appendFileSync(logPath, text);
    };
    this._record('== session open ==', new Uint8Array());
    this.done = (async () => {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        this._record('<<', value);
        // Reassemble frames and consume the DATA payloads
        // (KEEPALIVEs dropped, HELLO checked, terminals surfaced).
        for (const payload of this.frames.feed(value)) {
          this.text += this.output.decode(payload, { stream: true });
          onChunk?.(payload);
        }
      }
      this.text += this.output.decode();
    })();
    this.close = async () => {
      this._record('== client disconnect ==', new Uint8Array());
      await reader.cancel().catch(() => {});
    };
    return this;
  }

  // Sends raw input bytes (wrapped in a DATA frame); returns the HTTP
  // status of the POST.
  async input(bytes) {
    const data = typeof bytes === 'string' ? new TextEncoder().encode(bytes) : bytes;
    this._record('>>', data);
    const res = await fetch(this.inputUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: encodeFrame(TAG.data, data),
    });
    await res.body?.cancel().catch(() => {});
    return res.status;
  }

  // Lines of output seen so far (without trailing empty line).
  get lines() {
    return this.text.split('\n').filter((l) => l !== '');
  }

  // Resolves when the server ends the stream.
  async closed(timeoutMs = 10000) {
    const timeout = new Promise((_, reject) => {
      const t = setTimeout(
        () => reject(new Error(`console stream did not end within ${timeoutMs}ms`)),
        timeoutMs,
      );
      t.unref?.();
    });
    await Promise.race([this.done, timeout]);
    return this.text;
  }
}

export async function openConsole(opts) {
  return await new ConsoleSession().open(opts);
}

// Waits until the server has dropped the console session for (serverId, vm)
// (probe input until it returns 404). The server detects a client
// disconnect asynchronously, so a just-closed session may still be
// registered for a moment; this settles that so the next test starts from
// a clean state in the shared persistent env.
export async function waitForNoSession(baseUrl, serverId, vm, { timeout = 10000, interval = 100 } = {}) {
  const inputUrl = `${baseUrl}/api/servers/${serverId}/vms/${encodeURIComponent(vm)}/console/input`;
  // Probe with a protocol-correct KEEPALIVE frame: 204 while the
  // session is live, 404 once the server has dropped it.
  const probe = encodeFrame(TAG.keepalive, new Uint8Array(0));
  const deadline = Date.now() + timeout;
  for (;;) {
    const res = await fetch(inputUrl, { method: 'POST', body: probe });
    await res.body?.cancel().catch(() => {});
    if (res.status === 404) return;
    if (Date.now() > deadline) throw new Error(`console session for ${vm} did not close within ${timeout}ms`);
    await new Promise((r) => setTimeout(r, interval));
  }
}

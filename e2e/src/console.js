// Consumes the console session (one WebSocket: binary messages down,
// binary messages up), mirroring what the browser does. Every byte in
// both directions is appended to logPath.
import { StreamSession } from './ws.js';

export class ConsoleSession extends StreamSession {
  async open({ baseUrl, serverId, vm, logPath, onChunk }) {
    const vmName = encodeURIComponent(vm);
    return super.open({
      baseUrl,
      path: `/api/servers/${serverId}/vms/${vmName}/console/stream`,
      logPath,
      onChunk,
    });
  }

  // The pre-upgrade status for this (server, VM): 200 openable, 409 a
  // session is already open, 429 at the session cap, 404 unknown server.
  get preflightUrl() {
    return this.url;
  }
}

export async function openConsole(opts) {
  return await new ConsoleSession().open(opts);
}

// Waits until the server has dropped the console session for
// (serverId, vm). The server detects a client disconnect asynchronously
// (the ws read loop notices the transport died), so a just-closed
// session may still be registered for a moment; this settles that so
// the next test starts from a clean state in the shared persistent env.
export async function waitForNoSession(baseUrl, serverId, vm, { timeout = 10000, interval = 100 } = {}) {
  const url = `${baseUrl}/api/servers/${serverId}/vms/${encodeURIComponent(vm)}/console/status`;
  const deadline = Date.now() + timeout;
  for (;;) {
    const res = await fetch(url);
    const out = await res.json().catch(() => ({ open: true }));
    if (res.status === 200 && !out.open) return;
    if (Date.now() > deadline) throw new Error(`console session for ${vm} did not close within ${timeout}ms`);
    await new Promise((r) => setTimeout(r, interval));
  }
}

// Consumes a generic TCP tunnel session (one WebSocket: binary
// messages down, binary messages up) — the API-level twin of what
// DuplexChannel does in the browser. Mirrors ConsoleSession; every
// byte in both directions is appended to logPath.
import { StreamSession } from './ws.js';

export class TunnelSession extends StreamSession {
  async open({ baseUrl, serverId, vm, display, logPath, onChunk }) {
    const vmName = encodeURIComponent(vm);
    const q = `?display=${encodeURIComponent(display)}`;
    return super.open({
      baseUrl,
      path: `/api/servers/${serverId}/vms/${vmName}/tunnel/stream${q}`,
      logPath,
      onChunk,
    });
  }
}

export async function openTunnel(opts) {
  return await new TunnelSession().open(opts);
}

// tunnelOpenCount is the number of tunnel sessions the server reports
// open (all of them: the registry is global). The tunnel has no
// per-(server, VM) exclusivity — several concurrent sessions per VM
// are the SPICE case — so settles compare against a baseline.
export async function tunnelOpenCount(baseUrl, serverId) {
  const res = await fetch(
    `${baseUrl}/api/servers/${serverId}/vms/x/tunnel/status`,
  );
  if (res.status !== 200) throw new Error(`tunnel status: HTTP ${res.status}`);
  const out = await res.json();
  return out.open;
}

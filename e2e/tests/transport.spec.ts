// Transport tier (stub project): the plain-HTTP duplex transport —
// GET stream + ordered POST uplink — tested at both levels:
//   API level:  the tunnel endpoints (streaming/flushing, session
//               semantics, scenarios)
//   browser:    DuplexChannel driving xterm (console), noVNC and the
//               spice-html5 fork over the tunnel (transport smoke: the
//               stub tunnel is not a real RFB/SPICE service, so the
//               handshake fails with a protocol error *after* bytes
//               have flowed both ways)
import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api } from '../src/api.js';
import { openTunnel, TunnelSession } from '../src/tunnel.js';
import { encodeFrame, TAG } from '../src/frames.js';
import { waitFor } from '../src/wait.js';
import { startStub } from '../src/stubInstance.js';

let m: ReturnType<typeof requireEnv>;
let api: Api;
let serverId: string;
const url = () => m.virtweb.url;
let sessions: TunnelSession[] = [];

// The tunnel session handle for a VM+port is only known once a stream
// is opened (X-Tunnel-Id), so "settle" is per-session: each opened
// session is closed + settled by its sid in afterEach. This also models
// the SPICE case where several channels hold concurrent tunnels to the
// same target.
test.beforeAll(async () => {
  m = requireEnv();
  api = new Api(m.virtweb.url);
  serverId = m.serverId;
  // ensure debian-dev is running (the console page only shows the
  // viewer for running VMs)
  await api.action(serverId, 'debian-dev', 'destroy').catch(() => {});
  await api.action(serverId, 'debian-dev', 'start');
  await waitFor(async () => {
    const vms = await api.listVMs(serverId);
    return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
  });
});

// Settle a specific tunnel session: probe its input endpoint (by sid)
// until the server has dropped it (404). The server detects a client
// disconnect asynchronously.
async function settle(sid: string | null | undefined) {
  if (!sid) return;
  const inputUrl = `${url()}/api/servers/${serverId}/vms/x/tunnel/input?id=${sid}`;
  // Probe with a protocol-correct KEEPALIVE frame: 204 while the
  // session is live, 404 once the server has dropped it.
  const probe = encodeFrame(TAG.keepalive, new Uint8Array(0));
  await waitFor(async () => {
    const res = await fetch(inputUrl, { method: 'POST', body: probe });
    await res.body?.cancel();
    return res.status === 404;
  });
}

test.afterEach(async () => {
  for (const s of sessions) await s.close?.();
  for (const s of sessions) await settle(s.sid);
  sessions = [];
});

async function open(display, vm = 'debian-dev', outputDir) {
  const s = await openTunnel({
    baseUrl: url(),
    serverId,
    vm,
    display,
    logPath: outputDir ? `${outputDir}/tunnel.log` : undefined,
  });
  sessions.push(s);
  return s;
}

test('tunnel input routing: missing id -> 400, unknown id -> 404', async () => {
  const missing = await fetch(
    `${url()}/api/servers/${serverId}/vms/debian-dev/tunnel/input`,
    { method: 'POST', body: 'x' },
  );
  await missing.body?.cancel();
  expect(missing.status).toBe(400);

  const unknown = await fetch(
    `${url()}/api/servers/${serverId}/vms/debian-dev/tunnel/input?id=nope`,
    { method: 'POST', body: 'x' },
  );
  await unknown.body?.cancel();
  expect(unknown.status).toBe(404);
});

test('tunnel display validation: missing/invalid -> 400, ?target= ignored', async () => {
  // The destination is resolved server-side from the VM's live display
  // info; the client only names which display to view. A client-supplied
  // ?target= (the old free-form host:port) is no longer honoured.
  for (const q of ['', '?display=bogus', '?display=']) {
    const res = await fetch(
      `${url()}/api/servers/${serverId}/vms/debian-dev/tunnel/stream${q}`,
    );
    await res.body?.cancel();
    expect(res.status, `stream ${q}`).toBe(400);
  }
  const ignored = await fetch(
    `${url()}/api/servers/${serverId}/vms/debian-dev/tunnel/stream?display=spice&target=1.2.3.4:9999`,
  );
  expect(ignored.status).toBe(200);
  await ignored.body?.cancel();
});

test('stream + input round-trip, delivered in flushed chunks', async ({}, testInfo) => {
  const s = await open('spice', 'debian-dev', testInfo.outputDir);
  expect(s.status).toBe(200);
  // The stream endpoints must be proxy-stream-friendly (see AGENTS.md):
  // unbuffered, uncompressed, uncacheable.
  expect(s.headers['x-accel-buffering']).toBe('no');
  expect(s.headers['cache-control']).toBe('no-cache');

  await s.input('hello-echo');
  await waitFor(async () => s.text.includes('hello-echo'), {
    message: 'echo round-trip',
  });
  // The stub banner is written as three writes spaced 30ms apart, then
  // the echo arrives: a buffered proxy would deliver one burst instead.
  await waitFor(async () => s.chunkCount >= 2, { message: 'incremental arrival' });
  expect(s.text).toContain('stub-tunnel: debian-dev');
});

test('concurrent same-target tunnels -> distinct sessions (SPICE case)', async () => {
  // A single SPICE session opens several channels that each need their
  // own concurrent tunnel to the same target; both must succeed with
  // distinct session ids (not 409).
  const a = await open('spice');
  const b = await open('spice');
  expect(a.status).toBe(200);
  expect(b.status).toBe(200);
  expect(a.sid).toBeTruthy();
  expect(b.sid).toBeTruthy();
  expect(a.sid).not.toBe(b.sid);
});

test('client disconnect kills the session; input after close -> 404', async () => {
  const s = await open('spice');
  await s.input('ping');
  await waitFor(async () => s.text.includes('ping'));
  const sid = s.sid;
  await s.close();
  await settle(sid);
  expect(await s.input('x')).toBe(404);
});

test('cross-origin state-changing request -> 403 (CSRF protection)', async () => {
  // The server rejects non-safe cross-origin browser requests
  // (Sec-Fetch-Site/Origin, stdlib CrossOriginProtection); a
  // non-browser client (no such headers) is unaffected.
  const cross = await fetch(`${url()}/api/servers`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Origin: 'https://evil.example',
      'Sec-Fetch-Site': 'cross-site',
    },
    body: JSON.stringify({ name: 'csrf', user: 'u', host: 'h' }),
  });
  expect(cross.status).toBe(403);
  await cross.body?.cancel();

  const plain = await fetch(`${url()}/api/servers`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'csrf', user: 'u', host: 'h' }),
  });
  expect(plain.status).toBe(201);
  const created = await plain.json();
  await api.deleteServer(created.id);
});

/* ---------- STUB_SCENARIO error paths (disposable instances) ---------- */

test('scenario tunnel-drop: stream dies after the banner', async () => {
  const inst = await startStub({ scenario: 'tunnel-drop' });
  try {
    const api2 = new Api(inst.url);
    await api2.action(inst.serverId, 'debian-dev', 'start');
    await waitFor(async () => {
      const vms = await api2.listVMs(inst.serverId);
      return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
    });
    const s = await openTunnel({ baseUrl: inst.url, serverId: inst.serverId, vm: 'debian-dev', display: 'spice' });
    expect(s.status).toBe(200);
    const text = await s.closed(5000);
    expect(text).toContain('stub-tunnel: debian-dev');
  } finally {
    await inst.close();
  }
});

test('scenario ssh-down: tunnel stream ends immediately', async () => {
  const inst = await startStub({ scenario: 'ssh-down' });
  try {
    const s = await openTunnel({ baseUrl: inst.url, serverId: inst.serverId, vm: 'debian-dev', display: 'spice' });
    // The display lookup itself fails (ssh down): no banner, no stream.
    const text = await s.closed(5000);
    expect(text).not.toContain('stub-tunnel');
  } finally {
    await inst.close();
  }
});

/* ---------- browser level: the three viewers over the transport ---------- */

// read the xterm buffer as an array of non-empty lines
async function termLines(page: import('@playwright/test').Page) {
  return page.evaluate(() => {
    const b = (window as any).__term?.buffer?.active;
    if (!b) return [];
    const out: string[] = [];
    for (let y = 0; y < b.length; y++) {
      const line = b.getLine(y)?.translateToString(true) ?? '';
      if (line.trim()) out.push(line);
    }
    return out;
  });
}

test('console page: DuplexChannel drives xterm', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/console`);
  await expect(page.locator('#console-status')).toContainText('Connected.', {
    timeout: 10_000,
  });
  const probe = await page.evaluate(() => {
    const d = (window as any).__duplex;
    return {
      state: d.state,
      readable: d.readable instanceof ReadableStream,
      writable: d.writable instanceof WritableStream,
    };
  });
  expect(probe).toEqual({ state: 'open', readable: true, writable: true });
  // A live session hides the Reconnect affordance.
  await expect(page.locator('#console-reconnect')).toBeHidden();

  // Drive the transport directly (bypassing the keyboard): a write on
  // the channel must reach the stub console and echo back into xterm.
  await page.evaluate(() =>
    (window as any).__duplex.write(new TextEncoder().encode('hostname\r')),
  );
  await waitFor(async () => {
    const lines = await termLines(page);
    return lines.some((l) => l.includes('hostname'));
  });
  await page.close();
});

test('console page: clean session end shows Reconnect', async ({ page }) => {
  // The console-drop scenario serves the banner then ends the stream
  // (a clean server-side EOF, as when the VM's console detaches). The
  // page must read that as "Session ended." and offer Reconnect.
  const inst = await startStub({ scenario: 'console-drop' });
  try {
    const api2 = new Api(inst.url);
    await api2.action(inst.serverId, 'debian-dev', 'start');
    await waitFor(async () => {
      const vms = await api2.listVMs(inst.serverId);
      return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
    });
    await page.goto(`${inst.url}/servers/${inst.serverId}/vms/debian-dev/console`);
    await expect(page.locator('#console-status')).toContainText('Session ended.', {
      timeout: 15_000,
    });
    await expect(page.locator('#console-reconnect')).toBeVisible();
  } finally {
    await page.close();
    await inst.close();
  }
});

test('vnc page: noVNC over the tunnel (transport smoke)', async ({ page }) => {
  // win10 has VNCPort 5901 in the stub; the stub tunnel is not RFB, so
  // the handshake must fail *after* bytes flow down the stream.
  await page.goto(`${url()}/servers/${serverId}/vms/win10/vnc`);
  await expect(page.locator('#vnc-status')).toContainText(/Connection lost|Disconnected/, {
    timeout: 15_000,
  });
  // noVNC never auto-reconnects, so a failed/lost session offers Reconnect.
  await expect(page.locator('#vnc-reconnect')).toBeVisible();
  // And a VM without a VNC port gets the disabled page (no Reconnect).
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/vnc`);
  await expect(page.locator('#vnc-status')).toContainText(
    'VNC is not enabled on this VM',
  );
  await expect(page.locator('#vnc-reconnect')).toBeHidden();
  await page.close();
});

test('spice page: fork over the tunnel (transport smoke)', async ({ page }) => {
  // The fork sends its link header (uplink POST) right after open, and
  // the stub tunnel's banner arrives down the stream before any real
  // SPICE reply: the client must fail with a magic mismatch, which
  // proves both directions of the transport are live. The destination
  // is resolved server-side from the stub VM's SpiceAddr.
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  await expect(page.locator('#spice-status')).toContainText('127.0.0.1:5991');
  await page.click('button[type=submit]');
  await expect(page.locator('#spice-status')).toContainText(/magic mismatch/, {
    timeout: 15_000,
  });
  // A failed SPICE connect offers Reconnect.
  await expect(page.locator('#spice-reconnect')).toBeVisible();
  await page.close();
});

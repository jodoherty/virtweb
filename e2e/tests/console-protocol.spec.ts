import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api, ApiError } from '../src/api.js';
import { openConsole, ConsoleSession, waitForNoSession } from '../src/console.js';
import { FrameDecoder } from '../src/frames.js';
import { waitFor } from '../src/wait.js';
import { startStub } from '../src/stubInstance.js';

let api: Api;
let serverId: string;
let sessions: ConsoleSession[] = [];

test.beforeAll(async () => {
  const m = requireEnv();
  api = new Api(m.virtweb.url);
  serverId = m.serverId;
  // ensure debian-dev is running
  await api.action(serverId, 'debian-dev', 'destroy').catch(() => {});
  await api.action(serverId, 'debian-dev', 'start');
  await waitFor(async () => {
    const vms = await api.listVMs(serverId);
    return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
  });
});

test.afterEach(async () => {
  for (const s of sessions) await s.close?.();
  sessions = [];
  // settle: wait for the server to drop the session so the next test is clean
  await waitForNoSession(api.baseUrl, serverId, 'debian-dev');
});

async function open(name = 'debian-dev', outputDir?: string) {
  const s = await openConsole({
    baseUrl: api.baseUrl,
    serverId,
    vm: name,
    logPath: outputDir ? `${outputDir}/console.log` : undefined,
  });
  sessions.push(s);
  return s;
}

test('input without an open session -> 404', async () => {
  // must run before any session exists (declared first)
  const res = await fetch(
    `${api.baseUrl}/api/servers/${serverId}/vms/debian-dev/console/input`,
    { method: 'POST', body: 'x\r' },
  );
  await res.body?.cancel();
  expect(res.status).toBe(404);
});

test('stream + input round-trip', async ({}, testInfo) => {
  const s = await open('debian-dev', testInfo.outputDir);
  expect(s.status).toBe(200);
  await s.input('hostname\r');
  // The echoed command shares a line with the prompt (`debian-dev:~# hostname`);
  // the reply is its own line.
  await waitFor(async () => s.lines.includes('debian-dev') && s.text.includes('debian-dev:~# hostname'));
  // byte-deterministic stub: the banner must be first
  expect(s.text).toContain('Linux debian-dev');
  expect(s.text).toContain('Welcome to the virtweb stub console');
});

test('second concurrent stream -> 409', async () => {
  const first = await open('debian-dev');
  const res = await fetch(
    `${api.baseUrl}/api/servers/${serverId}/vms/debian-dev/console/stream`,
  );
  await res.body?.cancel();
  expect(res.status).toBe(409);
  await first.close();
});

test('client disconnect kills the session; input after close -> 404', async () => {
  const s = await open('debian-dev');
  await s.input('hostname\r');
  await waitFor(async () => s.lines.includes('debian-dev'));
  await s.close(); // cancel the stream => server cancels r.Context()
  await waitFor(async () => (await s.input('x\r')) === 404, {
    message: 'session gone after disconnect',
  });
});

/* ---------- STUB_SCENARIO error paths (disposable instances) ---------- */

test('idle stream emits KEEPALIVE frames while quiet', async () => {
  // Proxy-timeout protection (wire protocol v1): with a short
  // keepalive interval, an idle console stream must not go quiet —
  // the server emits KEEPALIVE frames while the session is silent.
  const inst = await startStub({ flags: ['-keepalive', '200ms'] });
  try {
    // The stub console requires a running domain (the shared env's
    // beforeAll boots it; a fresh instance starts with it shut off).
    const api2 = new Api(inst.url);
    await api2.action(inst.serverId, 'debian-dev', 'start');
    await waitFor(async () => {
      const vms = await api2.listVMs(inst.serverId);
      return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
    }, { message: 'debian-dev running', timeout: 5000 });

    const res = await fetch(
      `${inst.url}/api/servers/${inst.serverId}/vms/debian-dev/console/stream`,
    );
    expect(res.status).toBe(200);
    const reader = res.body!.getReader();
    let sawData = false;
    let sawKeepalive = false;
    const frames = new FrameDecoder(() => {
      sawKeepalive = true;
    });
    try {
      const deadline = Date.now() + 5000;
      while (!sawKeepalive && Date.now() < deadline) {
        const { value, done } = await reader.read();
        if (done) break;
        if (frames.feed(value).length > 0) sawData = true;
      }
    } finally {
      await reader.cancel().catch(() => {});
    }
    // The stub banner arrives as DATA first, then the stream goes idle
    // and keepalives start.
    expect(sawData).toBe(true);
    expect(sawKeepalive).toBe(true);
  } finally {
    await inst.close();
  }
});

test('scenario fail-ping: ping fails, list works', async () => {
  const inst = await startStub({ scenario: 'fail-ping' });
  try {
    const api2 = new Api(inst.url);
    const err = await api2.ping(inst.serverId).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(502);
    const vms = await api2.listVMs(inst.serverId);
    expect(vms.map((v) => v.name)).toEqual(['debian-dev', 'win10']);
  } finally {
    await inst.close();
  }
});

test('scenario ssh-down: every backend call fails', async () => {
  const inst = await startStub({ scenario: 'ssh-down' });
  try {
    const api2 = new Api(inst.url);
    for (const call of [
      () => api2.ping(inst.serverId),
      () => api2.listVMs(inst.serverId),
      () => api2.action(inst.serverId, 'debian-dev', 'start'),
    ]) {
      const err = await call().catch((e: unknown) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect((err as ApiError).status).toBe(502);
      expect(String(err)).toContain('Connection refused');
    }
  } finally {
    await inst.close();
  }
});

test('scenario console-drop: stream dies after the banner', async () => {
  const inst = await startStub({ scenario: 'console-drop' });
  try {
    const api2 = new Api(inst.url);
    await api2.action(inst.serverId, 'debian-dev', 'start');
    await waitFor(async () => {
      const vms = await api2.listVMs(inst.serverId);
      return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
    });
    const s = await openConsole({ baseUrl: inst.url, serverId: inst.serverId, vm: 'debian-dev' });
    expect(s.status).toBe(200);
    const text = await s.closed(5000);
    expect(text).toContain('Linux debian-dev');
    expect(text).not.toContain('debian-dev:~#'); // no prompt before the drop
  } finally {
    await inst.close();
  }
});

// Console session protocol (stub project): one WebSocket session per
// (server, VM). The session's terminal state rides the close code:
// 1000 for a clean end, 1011 + the server's message for a failed
// session; pre-upgrade problems (unknown server, session in use, cap)
// are ordinary HTTP statuses on the HEAD preflight.
import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api, ApiError } from '../src/api.js';
import { openConsole, ConsoleSession, waitForNoSession } from '../src/console.js';
import { preflightStatus } from '../src/ws.js';
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

const streamUrl = () =>
  `${api.baseUrl}/api/servers/${serverId}/vms/debian-dev/console/stream`;

test('preflight reports the session lifecycle', async () => {
  // must run before any session exists (declared first)
  expect(await preflightStatus(streamUrl())).toBe(200); // openable

  const s = await open('debian-dev');
  expect(s.status).toBe(200);
  expect(await preflightStatus(streamUrl())).toBe(409); // in use
  await s.close();
  await waitForNoSession(api.baseUrl, serverId, 'debian-dev');
  expect(await preflightStatus(streamUrl())).toBe(200); // openable again
});

test('session + input round-trip', async ({}, testInfo) => {
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

test('second concurrent session -> 409', async () => {
  const first = await open('debian-dev');
  const second = await openConsole({ baseUrl: api.baseUrl, serverId, vm: 'debian-dev' });
  expect(second.status).toBe(409);
  expect(second.ws).toBeNull(); // refused pre-upgrade: no connection
  await first.close();
});

test('client disconnect kills the session', async () => {
  const s = await open('debian-dev');
  await s.input('hostname\r');
  await waitFor(async () => s.lines.includes('debian-dev'));
  await s.close(); // the server notices the transport died asynchronously
  await waitForNoSession(api.baseUrl, serverId, 'debian-dev', { timeout: 15_000 });
  // settled: the session can be opened again
  expect(await preflightStatus(streamUrl())).toBe(200);
});

/* ---------- STUB_SCENARIO error paths (disposable instances) ---------- */

test('clean session end closes with 1000', async () => {
  // The console-drop scenario serves the banner then ends the session
  // cleanly (a clean server-side close, as when the console detaches):
  // the terminal close is 1000.
  const inst = await startStub({ scenario: 'console-drop' });
  try {
    const api2 = new Api(inst.url);
    await api2.action(inst.serverId, 'debian-dev', 'start');
    await waitFor(async () => {
      const vms = await api2.listVMs(inst.serverId);
      return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
    }, { message: 'debian-dev running', timeout: 5000 });

    const s = await openConsole({ baseUrl: inst.url, serverId: inst.serverId, vm: 'debian-dev' });
    expect(s.status).toBe(200);
    await s.closed(5000);
    expect(s.terminal).toMatchObject({ code: 1000 });
    expect(s.text).toContain('Linux debian-dev'); // DATA arrived before the end
  } finally {
    await inst.close();
  }
});

test('failed session closes with 1011 and the server message', async () => {
  // The ssh to the remote host fails: the client still gets a clean
  // transport end — but with the 1011 close code and the server's
  // message in the close reason.
  const inst = await startStub({ scenario: 'ssh-down' });
  try {
    const s = await openConsole({ baseUrl: inst.url, serverId: inst.serverId, vm: 'debian-dev' });
    expect(s.status).toBe(200);
    await s.closed(5000);
    expect(s.terminal?.code).toBe(1011);
    expect(s.terminal!.reason.length).toBeGreaterThan(0);
    expect(s.text).toBe(''); // no DATA: the session died before output
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
    expect(vms.map((v) => v.name)).toEqual(['debian-dev', 'win10', 'drip']);
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

test('scenario console-drop: session dies after the banner', async () => {
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

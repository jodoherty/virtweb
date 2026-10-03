import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api, ApiError } from '../src/api.js';
import { waitFor } from '../src/wait.js';

let api: Api;
let serverId: string;

type VM = { name: string; state: string; running: boolean; ip?: string };

function state(vm: string): Promise<VM | undefined> {
  return api.listVMs(serverId).then((vms) => vms.find((v) => v.name === vm));
}

async function ensureShutOff(vm: string) {
  await api.action(serverId, vm, 'destroy').catch(() => {});
  await api.action(serverId, vm, 'shutdown').catch(() => {});
}

async function ensureRunning(vm: string) {
  const cur = (await state(vm))?.state;
  if (cur !== 'running') {
    await api.action(serverId, vm, 'destroy').catch(() => {});
    await api.action(serverId, vm, 'start');
    await waitFor(async () => (await state(vm))?.state === 'running', {
      message: `${vm} running`,
    });
  }
}

test.beforeAll(() => {
  const m = requireEnv();
  api = new Api(m.virtweb.url);
  serverId = m.serverId;
});

test('ping ok', async () => {
  await expect(api.ping(serverId)).resolves.toEqual({ ok: true });
});

test('default VM table is shut off', async () => {
  await ensureShutOff('debian-dev');
  await ensureShutOff('win10');
  const vms = await api.listVMs(serverId);
  expect(vms.map((v) => v.name)).toEqual(['debian-dev', 'win10']);
  for (const v of vms) {
    expect(v.state).toBe('shut off');
    expect(v.running).toBe(false);
  }
  const d = vms[0];
  expect(d.vcpus).toBe(2);
  expect(d.memory).toBe(4096);
});

test('start -> starting -> running (with IP) -> shutdown', async () => {
  await ensureShutOff('debian-dev');
  await api.action(serverId, 'debian-dev', 'start');
  // the stub delays the start -> running transition; catch the in-between
  await waitFor(async () => (await state('debian-dev'))?.state === 'starting', {
    timeout: 1000,
    interval: 20,
    message: 'debian-dev starting',
  });
  await waitFor(async () => (await state('debian-dev'))?.state === 'running', {
    message: 'debian-dev running',
  });
  const running = await state('debian-dev');
  expect(running?.ip).toBe('192.168.100.50');
  await api.action(serverId, 'debian-dev', 'shutdown');
  await waitFor(async () => (await state('debian-dev'))?.state === 'shut off', {
    message: 'debian-dev shut off',
  });
});

test('start while running fails (502)', async () => {
  await ensureRunning('debian-dev');
  const err = await api
    .action(serverId, 'debian-dev', 'start')
    .catch((e: unknown) => e);
  expect(err).toBeInstanceOf(ApiError);
  expect((err as ApiError).status).toBe(502);
  await ensureShutOff('debian-dev');
});

test('actions on unknown VM fail (502)', async () => {
  const err = await api.action(serverId, 'nope', 'start').catch((e: unknown) => e);
  expect(err).toBeInstanceOf(ApiError);
  expect((err as ApiError).status).toBe(502);
});

test('server CRUD', async () => {
  const name = `tmp-${Date.now()}`;
  const created = await api.addServer({ name, user: 'u', host: 'h' });
  const listed = await api.listServers();
  expect(listed.some((s) => s.id === created.id)).toBe(true);
  await api.deleteServer(created.id);
  const after = await api.listServers();
  expect(after.some((s) => s.id === created.id)).toBe(false);
});

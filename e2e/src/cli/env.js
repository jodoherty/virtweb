// The e2e environment layer (see TESTING.md). Idempotent, non-interactive,
// --json where sensible. Phase 2 implements the stub backend; the
// libvirt container backend joins in phase 3.
//
//   node src/cli/env.js up [--backend stub] [--proxy haproxy-l7|haproxy-l4|traefik-l7|traefik-l4]
//   node src/cli/env.js down
//   node src/cli/env.js status [--json]
//   node src/cli/env.js logs
//   node src/cli/env.js reset
//   node src/cli/env.js restart-api
//   node src/cli/env.js doctor [--json]

import { spawnSync, execSync } from 'node:child_process';
import { readFileSync, rmSync, existsSync, appendFileSync, statSync } from 'node:fs';
import path from 'node:path';
import * as manifest from '../env/manifest.js';
import {
  BINARY,
  build,
  pickPort,
  isAlive,
  kill,
  waitForExit,
  start,
  waitReady,
} from '../env/virtweb.js';
import { Api } from '../api.js';
import * as libvirt from '../env/libvirt.js';
import * as proxy from '../env/proxy.js';

const [,, cmd, ...rest] = process.argv;
const flags = new Set(rest.filter((a) => a.startsWith('--')));
const flagVal = (name) => {
  const i = rest.indexOf(name);
  return i >= 0 ? rest[i + 1] : undefined;
};

async function registerServer(url, fields) {
  const api = new Api(url);
  const servers = await api.listServers();
  const existing = servers.find((s) => s.name === fields.name);
  if (existing) return existing.id;
  const created = await api.addServer(fields);
  return created.id;
}

function manifestLive(m) {
  return (
    m &&
    isAlive(m.virtweb.pid) &&
    m.virtweb.url.startsWith('http://127.0.0.1:')
  );
}

async function cmdUp() {
  const backend = flagVal('--backend') ?? 'stub';
  if (!['stub', 'libvirt'].includes(backend)) {
    console.error(`env: unknown backend ${JSON.stringify(backend)}`);
    process.exit(1);
  }
  const proxyKind = flagVal('--proxy') ?? 'none';
  if (proxyKind !== 'none' && !proxy.PROXY_KINDS.includes(proxyKind)) {
    console.error(`env: unknown proxy ${JSON.stringify(proxyKind)} (want ${proxy.PROXY_KINDS.join(', ')} or none)`);
    process.exit(1);
  }
  const m = manifest.read();
  if (m?.backend === backend && (m.proxy?.kind ?? 'none') === proxyKind && manifestLive(m)) {
    console.log(`env already up: ${m.virtweb.url} (pid ${m.virtweb.pid})${proxyKind !== 'none' ? ` [proxy: ${m.proxy.kind}]` : ''}`);
    return;
  }
  // Deterministic env: wipe the data dir so server registration is fresh.
  rmSync(path.join(manifest.ARTIFACTS, 'data'), { recursive: true, force: true });

  console.log('building virtweb...');
  build();
  const port = await pickPort();
  const { pid, logPath } = start({ port, backend: backend === 'libvirt' ? 'ssh' : 'stub' });
  console.log(`starting virtweb (${backend} backend) on ${port} (pid ${pid})...`);
  await waitReady(`http://127.0.0.1:${port}`, 15000);

  // The proxy axis (optional): a real proxy container in front of
  // virtweb; the env URL the tests use becomes the proxy's URL.
  let proxyInfo;
  let url = `http://127.0.0.1:${port}`;
  if (proxyKind !== 'none') {
    console.log(`starting the reverse proxy (${proxyKind}) in front of virtweb...`);
    proxyInfo = await proxy.startProxy(proxyKind, port, waitReady);
    url = `http://127.0.0.1:${proxyInfo.port}`;
  }

  let sshPort;
  let fields;
  if (backend === 'libvirt') {
    console.log('starting the libvirt host container...');
    ({ sshPort } = await libvirt.startContainer());
    console.log(`waiting for the host to be ready (sshd + libvirtd, ssh ${sshPort})...`);
    await libvirt.waitHostReady(sshPort);
    fields = { name: 'e2e', user: 'root', host: '127.0.0.1', port: sshPort, uri: 'qemu:///system' };
  } else {
    fields = { name: 'e2e', user: 'stub', host: 'stub-host' };
  }
  const serverId = await registerServer(url, fields);
  manifest.write({
    backend,
    created: new Date().toISOString(),
    virtweb: { url, pid, log: path.relative(manifest.E2E_ROOT, logPath) },
    // The direct virtweb port (the manifest URL may be the proxy's):
    // tests/proxy.spec.ts's negative control proxies this port.
    backendPort: port,
    ...(proxyInfo ? { proxy: proxyInfo } : {}),
    ...(sshPort ? { sshPort } : {}),
    serverId,
  });
  console.log(
    `env up: ${url}  pid ${pid}  serverId ${serverId}${proxyInfo ? `  [proxy: ${proxyInfo.kind}]` : ''}\n` +
      `  logs: npm run env:logs\n  status: npm run env:status`,
  );
}

function cmdDown() {
  // Clean any stray proxy first — a failed env:up leaves one behind
  // even though the manifest was never written.
  proxy.proxyDown();
  const m = manifest.read();
  if (!m) {
    console.log('env is not up (no manifest)');
    return;
  }
  if (isAlive(m.virtweb.pid)) {
    kill(m.virtweb.pid);
    waitForExit(m.virtweb.pid);
  }
  if (m.backend === 'libvirt') {
    // The guest-disk volume survives (next up is fast); --wipe drops it.
    libvirt.containerDown();
    if (flags.has('--wipe')) libvirt.wipe();
  }
  manifest.clear();
  console.log(m.backend === 'libvirt' && !flags.has('--wipe')
    ? 'env down (libvirt host container removed; image volume kept — env:down --wipe to drop it)'
    : 'env down');
}

async function cmdStatus() {
  const m = manifest.read();
  if (!m) {
    console.error('env is not up (no manifest). Run: npm run env:up -- --backend stub');
    process.exit(1);
  }
  let urlOk = false;
  try {
    urlOk = (await fetch(`${m.virtweb.url}/api/servers`)).ok;
  } catch {}
  const status = {
    up: isAlive(m.virtweb.pid) && urlOk,
    backend: m.backend,
    proxy: m.proxy?.kind ?? 'none',
    pid: m.virtweb.pid,
    url: m.virtweb.url,
    serverId: m.serverId,
  };
  if (flags.has('--json')) {
    console.log(JSON.stringify(status, null, 2));
  } else {
    console.log(status.up ? 'env is up' : 'env is NOT up (manifest exists but stale)');
    console.log(JSON.stringify(status, null, 2));
  }
}

function cmdLogs() {
  const log = path.join(manifest.ARTIFACTS, 'virtweb.log');
  if (!existsSync(log)) {
    console.log('(no virtweb log yet)');
    return;
  }
  const lines = readFileSync(log, 'utf8').split('\n').slice(-100);
  for (const line of lines) console.log(`virtweb | ${line}`);
  const m = manifest.read();
  if (m?.proxy) {
    const pl = proxy.proxyLogs(100).trim().split('\n');
    for (const line of pl) console.log(`proxy   | ${line}`);
  }
}

function cmdReset() {
  cmdDown();
  rmSync(path.join(manifest.ARTIFACTS, 'data'), { recursive: true, force: true });
  console.log('reset: data dir wiped (VM state and servers.json are fresh on next env:up)');
}

async function cmdRestartApi() {
  const m = manifest.read();
  if (!manifestLive(m)) {
    console.error('env is not up; run: npm run env:up -- --backend stub');
    process.exit(1);
  }
  console.log('rebuilding virtweb...');
  build();
  kill(m.virtweb.pid);
  waitForExit(m.virtweb.pid);
  const port = Number(m.virtweb.url.split(':').pop());
  const { pid, logPath } = start({ port, backend: m.backend === 'libvirt' ? 'ssh' : 'stub' });
  const url = m.virtweb.url;
  await waitReady(url, 15000);
  const serverId = await registerServer(url, {
    name: 'e2e',
    user: m.backend === 'libvirt' ? 'root' : 'stub',
    host: m.backend === 'libvirt' ? '127.0.0.1' : 'stub-host',
    ...(m.backend === 'libvirt' ? { port: m.sshPort, uri: 'qemu:///system' } : {}),
  }); // finds the existing 'e2e' server
  m.virtweb.pid = pid;
  m.virtweb.log = path.relative(manifest.E2E_ROOT, logPath);
  m.serverId = serverId;
  manifest.write(m);
  console.log(`api restarted: ${url} (pid ${pid})`);
}

function cmdDoctor() {
  const checks = [];
  const check = (name, fn) => {
    let ok = false;
    let detail = '';
    try {
      const d = fn();
      ok = true;
      detail = d ?? '';
    } catch (err) {
      detail = err.message;
    }
    checks.push({ name, ok, detail });
  };
  check('node >= 18', () => {
    const major = Number(process.versions.node.split('.')[0]);
    if (major < 18) throw new Error(`node ${process.versions.node}`);
    return `node ${process.versions.node}`;
  });
  check('playwright installed', () => {
    const r = spawnSync('node', ['--input-type=module', '-e', "import('@playwright/test')"], {
      cwd: manifest.E2E_ROOT,
    });
    if (r.status !== 0) throw new Error('npm install (in e2e/)');
    return '@playwright/test resolvable';
  });
  check('go toolchain', () => {
    const out = execSync('go version').toString().trim();
    return out;
  });
  check('virtweb binary built', () => {
    if (!existsSync(BINARY)) throw new Error('npm run env:up (builds it)');
    return BINARY;
  });
  check('/dev/kvm (informational; guests run TCG)', () => {
    return existsSync('/dev/kvm') ? 'present (unused: rootless containers cannot open it)' : 'absent';
  });
  check('gcc (tier 2 shim build)', () => {
    const out = execSync('gcc --version | head -1').toString().trim();
    return out;
  });
  check('podman (tier 2 only)', () => {
    const out = execSync('podman --version').toString().trim();
    return out;
  });
  const failed = checks.filter((c) => !c.ok);
  if (flags.has('--json')) {
    console.log(JSON.stringify({ ok: failed.length === 0, checks }, null, 2));
  } else {
    for (const c of checks) {
      console.log(`${c.ok ? 'ok    ' : 'FAIL  '} ${c.name}${c.detail ? ' — ' + c.detail : ''}`);
    }
    if (failed.length > 0) process.exit(1);
  }
}

switch (cmd) {
  case 'up': await cmdUp(); break;
  case 'down': cmdDown(); break;
  case 'status': await cmdStatus(); break;
  case 'logs': cmdLogs(); break;
  case 'reset': cmdReset(); break;
  case 'restart-api': await cmdRestartApi(); break;
  case 'doctor': cmdDoctor(); break;
  default:
    console.error(
      'usage: env.js <up [--backend stub|libvirt] [--proxy haproxy-l7|haproxy-l4|traefik-l7|traefik-l4] | ' +
        'down [--wipe] | status [--json] | logs | reset | restart-api | doctor [--json]>',
    );
    process.exit(2);
}

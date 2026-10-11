// Reverse-proxy fixtures for the e2e environment: real HAProxy and
// Traefik containers (rootless podman, --network host) placed in front
// of the virtweb process. See TESTING.md ("Reverse proxy axis") and
// the fixtures under e2e/fixtures/proxy/.
//
// The proxy is an orthogonal env axis: `env:up --proxy
// haproxy-l4|traefik-l4` puts the whole suite behind that proxy (the
// manifest URL becomes the proxy's URL), so every spec — sessions,
// keepalive, terminal state, UI — runs through a real proxy
// implementation. The production topology is L4 pass-through (the
// proxy forwards the raw TLS stream; virtweb terminates TLS itself and
// negotiates h2 over ALPN), so the axis is L4-only: an L7 proxy would
// have to be configured to pass WebSocket upgrades through, which the
// deployment contract does not require.
// `tests/proxy.spec.ts` adds the proxy-specific assertions on top.
//
// Rootless note: the proxy container runs with --network host, so it
// listens on a host 127.0.0.1 port and reaches virtweb (also a host
// process) directly — no published-port plumbing like the libvirt
// host container needs.

import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';
import { E2E_ROOT, ARTIFACTS } from './manifest.js';
import { pickPort, trustFixtureCerts } from './virtweb.js';

export const PROXY_CONTAINER = 'vw-e2e-proxy';

export const PROXY_KINDS = [
  'haproxy-l4',
  'traefik-l4',
];

// Pinned images (plain docker.io names pull in this environment).
const IMAGES = {
  'haproxy-l4': 'docker.io/haproxy:2.8',
  'traefik-l4': 'docker.io/traefik:v3.1',
};

// FIXTURES lists the template file(s) rendered into .artifacts/proxy/
// and mounted into the container.
const FIXTURES = {
  'haproxy-l4': [
    { tpl: 'haproxy/haproxy-l4.cfg', dest: '/etc/haproxy/haproxy.cfg' },
  ],
  'traefik-l4': [
    { tpl: 'traefik/static-l4.yml', dest: '/etc/traefik/static.yml' },
    { tpl: 'traefik/tcp-l4.yml', dest: '/etc/traefik/tcp.yml' },
  ],
};

function podman(args, opts = {}) {
  const r = spawnSync('podman', args, { stdio: 'inherit', ...opts });
  if (r.status !== 0) {
    throw new Error(`podman ${args.join(' ')} failed (exit ${r.status})`);
  }
}

export function imageFor(kind) {
  return IMAGES[kind];
}

function command(kind) {
  switch (kind) {
    case 'haproxy-l4':
      // The image's entrypoint assumes /usr/local/etc (the alpine
      // build); the Debian image's default path is /etc/haproxy.
      // -db = foreground + no daemon.
      return ['/usr/local/sbin/haproxy', '-f', '/etc/haproxy/haproxy.cfg', '-db'];
    case 'traefik-l4':
      return ['--configFile=/etc/traefik/static.yml'];
  }
  throw new Error(`unknown proxy kind ${kind}`);
}

// ensureImagePulled pulls the image if it is not present (podman
// `image exists` exits 1 when absent — do not route it through the
// throwing podman() helper).
function ensureImagePulled(image) {
  const r = spawnSync('podman', ['image', 'exists', image], { stdio: 'pipe' });
  if (r.status !== 0) {
    console.log(`pulling ${image} (first run only)...`);
    podman(['pull', image]);
  }
}

export function proxyRunning() {
  const r = spawnSync('podman', ['container', 'inspect', PROXY_CONTAINER], {
    stdio: 'pipe',
  });
  return r.status === 0;
}

export function proxyDown() {
  // Idempotent and manifest-free: removes the proxy and any strays
  // (negative-control containers, leftovers from failed env:ups).
  const ps = spawnSync(
    'podman',
    [
      'ps',
      '-a',
      '--filter',
      'name=vw-e2e-proxy',
      '--format',
      '{{.Names}}',
    ],
    { stdio: 'pipe' },
  );
  const names = (ps.stdout || '').toString().trim().split('\n').filter(Boolean);
  if (names.length === 0) return false;
  for (const n of names) podman(['rm', '-f', n]);
  return true;
}

// render returns .artifacts/proxy/ with the kind's config
// rendered (placeholders substituted) — the files get mounted in.
function render(kind, { proxyPort, backendPort }) {
  const dir = path.join(ARTIFACTS, 'proxy');
  mkdirSync(dir, { recursive: true });
  const mounts = [];
  for (const { tpl, dest } of FIXTURES[kind]) {
    const body = readFileSync(path.join(E2E_ROOT, 'fixtures', 'proxy', tpl), 'utf8')
      .replaceAll('{{PROXY_PORT}}', String(proxyPort))
      .replaceAll('{{BACKEND_PORT}}', String(backendPort));
    const rendered = path.join(dir, path.basename(tpl));
    writeFileSync(rendered, body);
    // Single argument (-v<src>:<dst>:ro): podman mis-parses "-v"
    // followed by a value that contains a space-including flag, and
    // even the plain two-arg form is rejected when the source path
    // itself is flagged; adjacent form is the safe one.
    mounts.push(`-v${rendered}:${dest}:ro`);
  }
  return { dir, mounts };
}

// startProxy renders the config, runs the container on a fresh
// 127.0.0.1 port, and waits until virtweb answers THROUGH the proxy.
export async function startProxy(kind, backendPort, waitReady) {
  if (!PROXY_KINDS.includes(kind)) {
    throw new Error(
      `unknown proxy kind ${JSON.stringify(kind)} (want ${PROXY_KINDS.join(', ')})`,
    );
  }
  const image = imageFor(kind);
  ensureImagePulled(image);
  proxyDown(); // recreate: a stale container holds a stale rendered port
  const proxyPort = await pickPort();
  const { mounts } = render(kind, { proxyPort, backendPort });
  podman([
    'run', '-d',
    '--name', PROXY_CONTAINER,
    '--network', 'host',
    ...mounts,
    image,
    ...command(kind),
  ]);
  await waitProxyReady(`https://127.0.0.1:${proxyPort}`);
  return { kind, port: proxyPort, container: PROXY_CONTAINER, image };
}

// waitProxyReady tolerates the Traefik cold-start window: right after
// `podman run`, traefik's entry listener answers Go's bare
// "404 page not found" for up to ~2 minutes (observed under rootless
// podman; its L4 router table activates only once the file provider
// finishes loading, which is slow on loaded hosts). HAProxy starts
// ready. Connection refusals are just "not up yet".
export async function waitProxyReady(url, timeoutMs = 300000) {
  trustFixtureCerts();
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${url}/api/servers`, {
        signal: AbortSignal.timeout(2000),
      });
      if (res.ok) return;
      if (res.status === 404 && (await res.text()) === '404 page not found\n') {
        lastErr = 'proxy warming up (404)';
      } else {
        lastErr = `HTTP ${res.status}`;
      }
    } catch (err) {
      lastErr = err.cause?.code || err.cause?.message || err.message;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`proxy at ${url} not ready after ${timeoutMs / 1000}s: ${lastErr}`);
}

export function proxyLogs(n = 100) {
  const r = spawnSync('podman', ['logs', `--tail=${n}`, PROXY_CONTAINER], {
    stdio: 'pipe',
  });
  return (r.stdout?.toString() || '') + (r.stderr?.toString() || '');
}

// Negative control for the idle-survival spec: a throwaway HAProxy
// with aggressive 4 s idle timeouts in front of the virtweb process.
// virtweb's default keepalive (15 s) cannot beat it, so an idle
// stream MUST be cut by the proxy within ~4-6 s. If the
// tests/proxy.spec.ts survival spec passes but this one does not,
// the survival test is vacuous (the proxy never cuts idle streams).
// It is separate from the env proxy because the env proxy's
// timeouts are chosen to be beaten by the keepalives.
export const NEGCTL_CONTAINER = 'vw-e2e-proxy-negctl';
const NEGCTL_CFG = `global
defaults
    mode tcp
    timeout connect 5s
    timeout client 4s
    timeout server 4s
frontend ft
    bind :{{PROXY_PORT}}
    default_backend be
backend be
    server virtweb 127.0.0.1:{{BACKEND_PORT}}
`;

export async function startNegativeControl(backendPort, waitReady) {
  ensureImagePulled(IMAGES['haproxy-l4']);
  const dir = path.join(ARTIFACTS, 'proxy-negctl');
  mkdirSync(dir, { recursive: true });
  if (proxyRunningNegctl()) podman(['rm', '-f', NEGCTL_CONTAINER]);
  const proxyPort = await pickPort();
  const rendered = path.join(dir, 'haproxy-negctl.cfg');
  writeFileSync(
    rendered,
    NEGCTL_CFG
      .replaceAll('{{PROXY_PORT}}', String(proxyPort))
      .replaceAll('{{BACKEND_PORT}}', String(backendPort)),
  );
  podman([
    'run', '-d',
    '--name', NEGCTL_CONTAINER,
    '--network', 'host',
    `-v${rendered}:/etc/haproxy/haproxy.cfg:ro`,
    IMAGES['haproxy-l4'],
    ...command('haproxy-l4'),
  ]);
  const url = `https://127.0.0.1:${proxyPort}`;
  await waitReady(url, 20000);
  return { port: proxyPort, container: NEGCTL_CONTAINER };
}

export function negativeControlDown() {
  if (proxyRunningNegctl()) podman(['rm', '-f', NEGCTL_CONTAINER]);
}

function proxyRunningNegctl() {
  const r = spawnSync('podman', ['container', 'inspect', NEGCTL_CONTAINER], {
    stdio: 'pipe',
  });
  return r.status === 0;
}

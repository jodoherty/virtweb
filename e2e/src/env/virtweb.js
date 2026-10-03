// virtweb process lifecycle for the e2e environment (stub backend for
// now; the libvirt container backend joins in phase 3).
import { spawnSync, spawn, execSync } from 'node:child_process';
import net from 'node:net';
import { mkdirSync, openSync, existsSync } from 'node:fs';
import path from 'node:path';
import { E2E_ROOT, ARTIFACTS } from './manifest.js';

export const BINARY = path.join(ARTIFACTS, 'virtweb');
export const VIRTWEB_ROOT = path.resolve(E2E_ROOT, '..');

// build compiles the virtweb binary into .artifacts/ (cached by go).
export function build() {
  mkdirSync(ARTIFACTS, { recursive: true });
  const r = spawnSync('go', ['build', '-o', BINARY, '.'], {
    cwd: VIRTWEB_ROOT,
    stdio: 'inherit',
  });
  if (r.status !== 0 || !existsSync(BINARY)) {
    throw new Error('go build failed');
  }
}

// pickPort reserves an ephemeral localhost port.
export function pickPort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

export function isAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

export function kill(pid) {
  try {
    process.kill(pid, 'SIGTERM');
  } catch {}
}

export function waitForExit(pid, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  while (isAlive(pid) && Date.now() < deadline) {
    execSync('sleep 0.1');
  }
  if (isAlive(pid)) {
    try {
      process.kill(pid, 'SIGKILL');
    } catch {}
  }
}

// spawn starts virtweb detached, logging to logPath. Returns the pid.
export function start({ port, scenario = '' }) {
  const logPath = path.join(ARTIFACTS, 'virtweb.log');
  const dataDir = path.join(ARTIFACTS, 'data');
  mkdirSync(dataDir, { recursive: true });
  const log = openSync(logPath, 'w');
  const child = spawn(
    BINARY,
    ['-backend', 'stub', '-addr', `127.0.0.1:${port}`, '-data-dir', dataDir],
    {
      env: { ...process.env, ...(scenario ? { STUB_SCENARIO: scenario } : {}) },
      stdio: ['ignore', log, log],
      detached: true,
    },
  );
  child.unref();
  return { pid: child.pid, logPath };
}

// waitReady polls until the API answers.
export async function waitReady(url, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${url}/api/servers`);
      if (res.ok) return;
      lastErr = `HTTP ${res.status}`;
    } catch (err) {
      lastErr = err.cause?.message || err.message;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`virtweb at ${url} not ready: ${lastErr}`);
}

// Disposable stub virtweb instances for STUB_SCENARIO error-path specs.
// The shared e2e env is scenario-free; a spec that needs failure
// injection spawns its own short-lived instance here.
import { mkdtempSync, rmSync, existsSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import net from 'node:net';
import { BINARY, VIRTWEB_ROOT, waitReady } from './env/virtweb.js';

function pickPort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

export async function startStub({ scenario = '', flags = [] } = {}) {
  if (!existsSync(BINARY)) {
    const r = spawnSync('go', ['build', '-o', BINARY, '.'], {
      cwd: VIRTWEB_ROOT,
      stdio: 'inherit',
    });
    if (r.status !== 0) throw new Error('go build failed');
  }
  const port = await pickPort();
  const dataDir = mkdtempSync(path.join(os.tmpdir(), 'virtweb-e2e-'));
  const logPath = path.join(dataDir, 'virtweb.log');
  const child = spawn(
    BINARY,
    ['-backend', 'stub', '-addr', `127.0.0.1:${port}`, '-data-dir', dataDir, ...flags],
    {
      env: { ...process.env, ...(scenario ? { STUB_SCENARIO: scenario } : {}) },
      stdio: ['ignore', 'ignore', 'ignore'],
      detached: true,
    },
  );
  child.unref();

  const url = `http://127.0.0.1:${port}`;
  try {
    await waitReady(url);
  } catch (err) {
    cleanup(child.pid, dataDir);
    throw err;
  }

  // Register the (fake) server.
  const res = await fetch(`${url}/api/servers`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'e2e', user: 'stub', host: 'stub-host' }),
  });
  const serverId = (await res.json()).id;

  function cleanup(pid, dir) {
    try {
      process.kill(pid, 'SIGTERM');
    } catch {}
    rmSync(dir, { recursive: true, force: true });
  }
  return {
    url,
    serverId,
    logPath,
    close: async () => {
      try {
        process.kill(child.pid, 'SIGTERM');
      } catch {}
      rmSync(dataDir, { recursive: true, force: true });
    },
  };
}

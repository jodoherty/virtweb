// E2E_ENV=own: bring up a disposable environment for the whole run.
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export default function globalSetup() {
  const e2e = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
  const r = spawnSync('node', ['src/cli/env.js', 'up', '--backend', 'stub'], {
    cwd: e2e,
    stdio: 'inherit',
  });
  if (r.status !== 0) {
    throw new Error('E2E_ENV=own: env:up failed');
  }
}

// E2E_ENV=own: tear the disposable environment down after the run.
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export default function globalTeardown() {
  const e2e = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
  spawnSync('node', ['src/cli/env.js', 'down'], { cwd: e2e, stdio: 'inherit' });
}

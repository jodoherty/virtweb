import { defineConfig } from '@playwright/test';
import { readFileSync } from 'node:fs';
import path from 'node:path';

// The e2e environment is a single shared, persistent target (one virtweb
// process + one stub host). All spec files mutate the same VMs, so they MUST
// run one at a time. workers:1 + fullyParallel:false guarantee that.
//
// Environment modes (see TESTING.md):
//   attach (default)  the env is already up (npm run env:up); tests attach to
//                     it and never tear it down, so a failed state survives
//                     for debugging.
//   own (E2E_ENV=own) globalSetup brings up a disposable env, globalTeardown
//                     tears it down.
const own = process.env.E2E_ENV === 'own';

// L7-proxy envs settle graceful client disconnects only after the
// proxy's idle timeout (HAProxy holds the backend connection until
// its timeout; see src/env/proxy.js isL7), so the session-settle
// deadlines there exceed the default 30 s test/hook timeout.
function envTimeout() {
  try {
    // The manifest is the same contract the specs read (attach mode);
    // in own mode it does not exist yet (globalSetup creates it later,
    // and the default 30 s is fine there: own mode is not used with
    // the proxy axis in CI).
    // __dirname exists when the config is loaded as CJS; import.meta
    // when as ESM (package.json "type": "module").
    const here =
      typeof __dirname !== 'undefined'
        ? __dirname
        : path.dirname(new URL(import.meta.url).pathname);
    const manifest = JSON.parse(
      readFileSync(path.join(here, '.artifacts', 'env.json'), 'utf8'),
    );
    if (manifest?.proxy && /-(l7)$/.test(String(manifest.proxy.kind))) {
      return 90_000;
    }
  } catch {}
  return 30_000;
}

export default defineConfig({
  testDir: './tests',
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: envTimeout(),
  expect: { timeout: 5_000 },
  reporter: [['list'], ['./src/cheatsheet.js']],
  globalSetup: own ? './src/env/globalSetup.js' : undefined,
  globalTeardown: own ? './src/env/globalTeardown.js' : undefined,
  projects: [
    // Tier 0: deterministic stub backend (the default, offline).
    { name: 'stub', testIgnore: /\.real\.spec\.ts$/ },
    // Tier 2: real libvirt podman host (@real specs only).
    { name: 'libvirt', testMatch: /\.real\.spec\.ts$/ },
  ],
});

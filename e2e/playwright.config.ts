import { defineConfig } from '@playwright/test';

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

export default defineConfig({
  testDir: './tests',
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 30_000,
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

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
//
// The env runs over TLS against a fixture self-signed certificate
// (virtweb requires TLS; see e2e/src/env/certs.js). Browsers ignore
// the certificate errors here — the Node-side helpers do the same via
// NODE_TLS_REJECT_UNAUTHORIZED in e2e/src/envinfo.js.
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
  use: {
    ignoreHTTPSErrors: true, // Playwright 1.63: renamed from the old
    // `ignoreHTTPSSerrors` (the old name is now silently ignored).
  },
  projects: [
    // Tier 0: deterministic stub backend (the default, offline).
    // Chromium: ignoreHTTPSErrors does not reach every connection the
    // browser opens (a WebSocket connection is a fresh one, and a
    // cert failure there degrades the session to the h1 leg or kills
    // it), so Chromium additionally ignores certificate errors at the
    // browser level.
    {
      name: 'stub',
      testIgnore: /\.real\.spec\.ts$/,
      use: {
        browserName: 'chromium',
        launchOptions: { args: ['--ignore-certificate-errors'] },
      },
    },
    // Same specs on Firefox: the production browsers include Firefox and
    // it diverges from Chromium on the transport (h2 extended-CONNECT
    // vs h1 upgrade fallback, see doc/TRANSPORT.md) and on CSS cursor
    // handling; the stub tier is where those regressions are cheapest to
    // catch.
    { name: 'stub-firefox', use: { browserName: 'firefox' },
      testIgnore: /\.real\.spec\.ts$/ },
    // Tier 2: real libvirt podman host (@real specs only).
    { name: 'libvirt', testMatch: /\.real\.spec\.ts$/ },
    { name: 'libvirt-firefox', use: { browserName: 'firefox' },
      testMatch: /\.real\.spec\.ts$/ },
  ],
});

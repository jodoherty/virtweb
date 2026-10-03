// Shared access to the test environment manifest for specs and the
// custom reporter.
import { read } from './env/manifest.js';

// requireEnv loads the manifest or fails with setup instructions.
// Attach mode: tests attach to the persistent env and never tear it down.
// Own mode (E2E_ENV=own): globalSetup brings the env up per run.
export function requireEnv() {
  const m = read();
  if (!m) {
    throw new Error(
      'no e2e environment manifest (.artifacts/env.json). ' +
        'Run: npm run env:up -- --backend stub   (attach mode, E2E_ENV=attach)',
    );
  }
  return m;
}

// envUrl is the virtweb base URL (used by Playwright `baseURL`).
export function envUrl() {
  return requireEnv().virtweb.url;
}

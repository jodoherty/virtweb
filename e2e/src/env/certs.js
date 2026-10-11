// Test TLS for the e2e environment: a self-signed server certificate
// (SAN 127.0.0.1) generated into .artifacts/certs/ and reused across
// env:up cycles. virtweb requires TLS (the WebSocket transport needs
// it), so the whole suite runs over https/wss against this certificate;
// the clients that must not verify it are:
//   - Node-level helpers: NODE_TLS_REJECT_UNAUTHORIZED=0 (set in-process
//     by envinfo.js / virtweb.js before the first TLS use)
//   - browsers: Playwright's ignoreHTTPSErrors
// The certificate is a test fixture, not a trust anchor — production
// deployments use real certificates (Let's Encrypt; doc/USAGE.md).
import { spawnSync } from 'node:child_process';
import { mkdirSync, existsSync } from 'node:fs';
import path from 'node:path';
import { ARTIFACTS } from './manifest.js';

export const CERT_DIR = path.join(ARTIFACTS, 'certs');
export const SERVER_CERT = path.join(CERT_DIR, 'server.crt');
export const SERVER_KEY = path.join(CERT_DIR, 'server.key');

// ensureCerts generates the certificate if it is missing (idempotent;
// a cert that outlived its env is reusable — the SAN is fixed).
export function ensureCerts() {
  if (existsSync(SERVER_CERT) && existsSync(SERVER_KEY)) return;
  mkdirSync(CERT_DIR, { recursive: true });
  const r = spawnSync(
    'openssl',
    [
      'req', '-x509', '-newkey', 'rsa:2048',
      '-keyout', SERVER_KEY,
      '-out', SERVER_CERT,
      '-days', '30', '-nodes',
      '-subj', '/CN=127.0.0.1',
      '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost',
    ],
    { stdio: 'pipe' },
  );
  if (r.status !== 0) {
    throw new Error(`openssl failed: ${r.stderr}`);
  }
}

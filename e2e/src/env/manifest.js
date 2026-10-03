// Manifest (.artifacts/env.json) — the contract tests and agents consume.
// See TESTING.md for the schema.
import { readFileSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// e2e/ root (this file lives in e2e/src/env/).
export const E2E_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const ARTIFACTS = path.join(E2E_ROOT, '.artifacts');
export const MANIFEST = path.join(ARTIFACTS, 'env.json');

export function read() {
  try {
    return JSON.parse(readFileSync(MANIFEST, 'utf8'));
  } catch {
    return null;
  }
}

export function write(manifest) {
  mkdirSync(ARTIFACTS, { recursive: true });
  writeFileSync(MANIFEST, JSON.stringify(manifest, null, 2) + '\n');
}

export function clear() {
  try {
    rmSync(MANIFEST);
  } catch {}
}

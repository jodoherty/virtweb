// Tier-2 "remote libvirt host": a rootless podman container emulating
// the remote KVM host (sshd + libvirtd + QEMU on TCG). See
// fixtures/container/ and TESTING.md.
//
// Rootless-host restrictions (all worked around; keep in sync with
// entrypoint.sh and vw-xattr-shim.c):
//   - /proc/sys is read-only → the entrypoint overlays a populated
//     tmpfs (needs --privileged) so the NAT bridge can start.
//   - /dev/kvm is not openable by any container process (host-side
//     permission checks use the real host groups) → guests run TCG.
//   - libvirt's security driver needs trusted.* xattrs / device
//     chown+chmod+ACL → the LD_PRELOAD shim no-ops exactly those.
//
// The guest disk is cached in the named podman volume vw-e2e-images
// (survives container recreation); `env:down --wipe` removes it.

import { spawnSync } from 'node:child_process';
import { existsSync, statSync, mkdirSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { E2E_ROOT, ARTIFACTS } from './manifest.js';
import { pickPort } from './virtweb.js';

export const CONTAINER = 'vw-e2e';
export const IMAGE = 'virtweb-e2e-host:dev';
export const VOLUME = 'vw-e2e-images';
export const GUEST = 'vw-e2e-guest';

const FIXTURES = path.join(E2E_ROOT, 'fixtures', 'container');
const SHIM_SRC = path.join(FIXTURES, 'vw-xattr-shim.c');
export const SHIM_SO = path.join(ARTIFACTS, 'vw-xattr-shim.so');

function podman(args, opts = {}) {
  const r = spawnSync('podman', args, { stdio: 'inherit', ...opts });
  if (r.status !== 0) {
    throw new Error(`podman ${args.join(' ')} failed (exit ${r.status})`);
  }
}

function sshOpts(sshPort) {
  return [
    '-p', String(sshPort),
    '-o', 'BatchMode=yes',
    '-o', 'ConnectTimeout=5',
    '-o', 'StrictHostKeyChecking=no',
    '-o', 'UserKnownHostsFile=/dev/null',
  ];
}

// buildShim compiles the xattr/chown/chmod shim with the host gcc
// (cached: rebuilt only when the source is newer than the .so).
export function buildShim() {
  mkdirSync(ARTIFACTS, { recursive: true });
  if (
    existsSync(SHIM_SO) &&
    statSync(SHIM_SO).mtimeMs >= statSync(SHIM_SRC).mtimeMs
  ) {
    return;
  }
  const r = spawnSync(
    'gcc',
    ['-shared', '-fPIC', '-O2', '-o', SHIM_SO, SHIM_SRC, '-ldl'],
    { stdio: 'inherit' },
  );
  if (r.status !== 0 || !existsSync(SHIM_SO)) {
    throw new Error('shim build failed (host gcc required)');
  }
}

// ensureImage (re)builds the container image on every env:up. Podman's
// layer cache makes an unchanged rebuild nearly instant, while a
// rebuild is the only reliable way to pick up fixture changes
// (Dockerfile, entrypoint.sh, vm.xml) — checking the image's
// existence alone silently reused stale images.
export function ensureImage() {
  console.log(`building container image ${IMAGE} (cached layers, fast if unchanged)...`);
  podman(['build', '-t', IMAGE, FIXTURES]);
}

export function containerRunning() {
  const r = spawnSync('podman', ['container', 'inspect', CONTAINER], {
    stdio: 'pipe',
  });
  return r.status === 0;
}

// startContainer creates the "remote host" container. The ssh port is
// chosen per env (recorded in the manifest); the guest disk volume is
// reused across recreates.
export async function startContainer() {
  buildShim();
  ensureImage();
  if (containerRunning()) {
    // Recreate: a stale container may hold a stale shim/mount.
    podman(['rm', '-f', CONTAINER]);
  }
  const sshPort = await pickPort();
  const key = path.join(os.homedir(), '.ssh', 'id_ed25519.pub');
  if (!existsSync(key)) {
    throw new Error(
      `no host ssh public key at ${key} (rootless podman + key auth ` +
        'requires the standard key)',
    );
  }
  podman([
    'run', '-d',
    '--name', CONTAINER,
    '--privileged', // /proc/sys tmpfs overlay for the NAT bridge
    // the shim no-ops libvirt's impossible security-driver calls:
    '-v', `${SHIM_SO}:/vw/vw-xattr-shim.so:ro`,
    '-v', `${key}:/root/.ssh/authorized_keys:Z`,
    '-v', `${VOLUME}:/var/lib/libvirt/images`,
    '-p', `127.0.0.1:${sshPort}:22`,
    IMAGE,
  ]);
  return { sshPort };
}

// waitHostReady polls until sshd + libvirtd answer and the guest is
// defined (entrypoint finished).
export async function waitHostReady(sshPort, timeoutMs = 180_000) {
  const deadline = Date.now() + timeoutMs;
  let lastErr = '';
  while (Date.now() < deadline) {
    const r = spawnSync(
      'ssh',
      [...sshOpts(sshPort), 'root@127.0.0.1', `virsh list --all | grep -c ${GUEST}`],
      { stdio: 'pipe', timeout: 15_000 },
    );
    if (r.status === 0 && r.stdout.toString().trim() === '1') return;
    lastErr = r.stderr?.toString().trim() || `exit ${r.status}`;
    await new Promise((r2) => setTimeout(r2, 2_000));
  }
  throw new Error(`libvirt host not ready (ssh ${sshPort}): ${lastErr}`);
}

// guestState returns the guest's libvirt state ("running"/"shut off")
// or "" before first boot bookkeeping.
export function guestState(sshPort) {
  const r = spawnSync(
    'ssh',
    [...sshOpts(sshPort), 'root@127.0.0.1',
      `virsh dominfo ${GUEST} 2>/dev/null | awk -F': ' '/State/{print $2}'`],
    { stdio: 'pipe', timeout: 20_000 },
  );
  return r.status === 0 ? r.stdout.toString().trim() : '';
}

export function containerDown() {
  const r = spawnSync('podman', ['container', 'inspect', CONTAINER], {
    stdio: 'pipe',
  });
  if (r.status !== 0) return false;
  podman(['rm', '-f', CONTAINER]);
  return true;
}

export function wipe() {
  podman(['volume', 'rm', VOLUME]);
}

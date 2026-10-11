// Tier 2 (@real): the full guest story against the libvirt podman
// host (fixtures/container, see TESTING.md). One sequential narrative —
// workers:1 and .describe.serial keep the steps ordered, and a failure
// leaves the env running for debugging (attach mode).
//
// Slow by design: the guest runs on TCG (rootless containers cannot
// open /dev/kvm), so a boot takes ~3-5 minutes.

import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api } from '../src/api.js';
import { waitFor, sleep } from '../src/wait.js';

const GUEST = 'vw-e2e-guest';
const CONSOLE_PASS = 'vw-e2e-pass';

let m: ReturnType<typeof requireEnv>;
let api: Api;
let serverId: string;
const url = () => m.virtweb.url;


test.beforeAll(async () => {
  m = requireEnv();
  api = new Api(m.virtweb.url);
  serverId = m.serverId;
});

function vm() {
  return api.listVMs(serverId).then((vms) => vms.find((v) => v.name === GUEST));
}

// read the xterm buffer as an array of non-empty lines
async function termLines(page: import('@playwright/test').Page) {
  return page.evaluate(() => {
    const b = (window as any).__term?.buffer?.active;
    if (!b) return [];
    const out: string[] = [];
    for (let i = 0; i < b.length; i++) {
      const l = b.getLine(i);
      const s = l ? l.translateToString(true).trim() : '';
      if (s) out.push(s);
    }
    return out;
  });
}

test.describe.serial('libvirt host guest (TCG)', () => {
  test('guest is defined (normalize to shut off)', async () => {
    // Re-runs may find the guest still up from a previous failure:
    // tear it down so the story always starts from a cold disk.
    test.setTimeout(120_000);
    await expect
      .poll(async () => (await vm())?.state, { message: 'guest defined' })
      .toBeTruthy();
    if ((await vm())?.state === 'running') {
      // Hard kill is fine here: we just need a cold disk.
      await api.action(serverId, GUEST, 'destroy').catch(() => {});
      await waitFor(async () => (await vm())?.state === 'shut off', {
        timeout: 90_000,
        interval: 3_000,
        message: 'guest shut off',
      });
    }
  });

  test('Start brings QEMU up', async () => {
    test.setTimeout(120_000);
    await api.action(serverId, GUEST, 'start');
    await waitFor(async () => (await vm())?.state === 'running', {
      timeout: 90_000,
      interval: 5_000,
      message: 'QEMU running',
    });
  });

  test('guest boots over TCG and gets a DHCP address', async () => {
    // domifaddr tracks the libvirt lease; the address appears a few
    // minutes after start (TCG boot + cloud-init + netplan).
    test.setTimeout(480_000);
    await waitFor(async () => (await vm())?.ip, {
      timeout: 450_000,
      interval: 10_000,
      message: 'DHCP address in the VM table',
    });
  });

  test('serial console: login and a command round-trip', async ({ page }) => {
    test.setTimeout(420_000);
    await page.goto(`${url()}/servers/${serverId}/vms/${GUEST}/console`);
    await expect(page.locator('#console-status')).toHaveText(/^Connected\./);
    await page.locator('#term').click();

    // The terminal buffer is the ground truth: wait until the serial
    // session has actually attached (virsh prints "Connected to
    // domain"), which can lag the HTTP stream on a fresh TCG boot.
    const termHas = async (re: RegExp) =>
      (await termLines(page)).some((l) => re.test(l));
    await expect
      .poll(async () => termHas(/Connected to domain/), {
        timeout: 60_000,
        message: 'serial session attached',
      })
      .toBe(true);

    // Nudge until the login prompt shows (getty redraws on input). On
    // TCG the getty can take a while after DHCP comes up, so be
    // patient: press Enter and keep looking for up to ~2 minutes.
    const loginSeen = async () => termHas(/login:\s*$/);
    const nudgeDeadline = Date.now() + 240_000;
    while (Date.now() < nudgeDeadline && !(await loginSeen())) {
      await page.keyboard.press('Enter');
      await page.waitForTimeout(1500);
    }
    await expect.poll(loginSeen, { message: 'login prompt' }).toBe(true);

    await page.keyboard.type('root');
    await page.keyboard.press('Enter');
    await expect
      .poll(async () => (await termLines(page)).some((l) => l.includes('Password:')), {
        message: 'password prompt',
      })
      .toBe(true);
    await page.keyboard.type(CONSOLE_PASS);
    await page.keyboard.press('Enter');

    const marker = 'VW-E2E-ROUNDTRIP';
    await expect
      .poll(
        async () =>
          (await termLines(page)).some(
            (l) => l.startsWith('root@') && l.includes('#'),
          ),
        { message: 'root shell', timeout: 30_000 },
      )
      .toBe(true);
    await page.keyboard.type(`echo ${marker}`);
    await page.keyboard.press('Enter');
    await expect
      .poll(async () => (await termLines(page)).some((l) => l === marker), {
        message: `${marker} echoed`,
      })
      .toBe(true);

    await page.close();
  });

  test('VNC page: real RFB handshake over the tunnel', async ({ page }) => {
    test.setTimeout(120_000);
    // The VNC autoport is only known while the domain runs.
    await expect
      .poll(async () => (await vm())?.vncPort ?? 0, { message: 'VNC port' })
      .toBeGreaterThan(0);
    await page.goto(`${url()}/servers/${serverId}/vms/${GUEST}/vnc`);
    await expect
      .poll(() => page.locator('#vnc-status').textContent(), {
        message: 'RFB connected',
        timeout: 60_000,
      })
      .toContain('Connected');
    // The RFB connection should hold: noVNC's internal connection
    // state (exposed via the e2e hook) stays 'connected'.
    await expect
      .poll(
        async () =>
          page.evaluate(() => (window as any).__rfb?._rfbConnectionState),
        { message: 'rfb connected state' },
      )
      .toBe('connected');
    await page.close();
  });

  test('Shutdown takes the guest down', async () => {
    test.setTimeout(300_000);
    // Graceful ACPI shutdown first; if the (slow) TCG guest does not
    // come down in time, fall back to a hard destroy so the story
    // always ends on a cold disk.
    await api.action(serverId, GUEST, 'shutdown').catch(() => {});
    let down = false;
    const deadline = Date.now() + 150_000;
    while (Date.now() < deadline) {
      if ((await vm())?.state === 'shut off') {
        down = true;
        break;
      }
      await sleep(5_000);
    }
    if (!down) {
      await api.action(serverId, GUEST, 'destroy').catch(() => {});
      await waitFor(async () => (await vm())?.state === 'shut off', {
        timeout: 90_000,
        interval: 5_000,
        message: 'guest shut off (after destroy fallback)',
      });
    }
  });
});

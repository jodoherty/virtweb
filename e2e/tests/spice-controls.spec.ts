// SPICE viewer controls: audio / guest-resize toggles and the zoom
// dropdown. The stub's SPICE service never completes a full
// handshake (it rejects the ticket), so these specs test the
// page-side machinery (defaults, persistence, zoom geometry) rather
// than a live guest; the zoom engine is exercised against a
// synthetic canvas injected into #spice.
import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';

let m: ReturnType<typeof requireEnv>;
let serverId: string;
const url = () => m.virtweb.url;

test.beforeAll(async () => {
  m = requireEnv();
  serverId = m.serverId;
});

test('defaults: audio off, guest resize off, zoom 100%', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // Audio is a per-session opt-in: off on every fresh page open.
  await expect(page.locator('#spice-audio')).not.toBeChecked();
  await expect(page.locator('#spice-guestresize')).not.toBeChecked();
  await expect(page.locator('#spice-zoom')).toHaveValue('100');
});

test('zoom engine: canvas sized in logical px, relative to the fit', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // Synthetic bitmap standing in for the fork's canvas. The fitted size
  // is min(aw/bw, ah/bh) of the display area (the area is the window
  // below the bar; its exact height varies with the browser's form
  // control metrics, so derive expectations from the live area).
  await page.evaluate(() => {
    const c = document.createElement('canvas');
    c.width = 1280;
    c.height = 800;
    document.getElementById('spice')!.appendChild(c);
  });
  await page.waitForTimeout(100); // observers settle
  const geom = async () =>
    page.evaluate(() => {
      const c = document.querySelector('#spice canvas') as HTMLCanvasElement;
      const s = document.getElementById('spice')!;
      return { cw: c.clientWidth, ch: c.clientHeight, aw: s.clientWidth, ah: s.clientHeight };
    });
  const at = (zoom: number) =>
    page.locator('#spice-zoom').selectOption(String(zoom));

  const fitted = async (zoom: number) => {
    await at(zoom);
    const g = await geom();
    const fit = Math.min(g.aw / 1280, g.ah / 800);
    expect(g.cw).toBe(Math.round(1280 * fit * (zoom / 100)));
    expect(g.ch).toBe(Math.round(800 * fit * (zoom / 100)));
    // The area itself never changes with zoom.
    expect([g.aw, g.ah]).toEqual([1280, g.ah]);
    return g;
  };
  const g100 = await fitted(100);
  await fitted(200);
  await fitted(25);
  await fitted(50);
  // zoom out of the full range returns to the fitted size
  await at(100);
  const g100b = await geom();
  expect(g100b.cw).toBe(g100.cw);
  expect(g100b.ch).toBe(g100.ch);
});

test('settings persist per browser (except audio)', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  await page.locator('#spice-audio').check();
  await page.locator('#spice-guestresize').check();
  await page.locator('#spice-zoom').selectOption('150');
  const ls = await page.evaluate(() =>
    JSON.stringify([
      localStorage.getItem('vw.spice.audio'),
      localStorage.getItem('vw.spice.resize'),
      localStorage.getItem('vw.spice.zoom'),
    ]),
  );
  // Audio is a per-session opt-in: it is never persisted, so the key
  // stays null even after the toggle is checked.
  expect(ls).toBe(JSON.stringify([null, '1', '150']));
  await page.reload();
  // Audio back to off; resize and zoom remembered.
  await expect(page.locator('#spice-audio')).not.toBeChecked();
  await expect(page.locator('#spice-guestresize')).toBeChecked();
  await expect(page.locator('#spice-zoom')).toHaveValue('150');
});

test('zoom is not applied twice when the guest follows the resize', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // Spy the MonConfig requests the page sends.
  await page.evaluate(() => {
    (window as any).__spice = {
      calls: [] as number[][],
      resize_window: (_f: number, w: number, h: number) =>
        (window as any).__spice.calls.push([w, h]),
    };
  });
  // 50% zoom with guest resize on: the page requests the area scaled
  // 2x along each axis.
  await page.locator('#spice-zoom').selectOption('50');
  await page.locator('#spice-guestresize').check();
  await page.waitForTimeout(100);
  const req = await page.evaluate(() =>
    (window as any).__spice.calls.at(-1) as [number, number],
  );
  // The guest "follows": its bitmap becomes the requested size.
  await page.evaluate((r) => {
    const c = document.createElement('canvas');
    c.width = r[0];
    c.height = r[1];
    document.getElementById('spice')!.appendChild(c);
  }, req);
  await page.waitForTimeout(150); // observers settle
  const g = await page.evaluate(() => {
    const c = document.querySelector('#spice canvas') as HTMLCanvasElement;
    const s = document.getElementById('spice')!;
    return {
      cw: c.clientWidth,
      ch: c.clientHeight,
      aw: s.clientWidth,
      ah: s.clientHeight,
      bw: c.width,
      bh: c.height,
    };
  });
  // Each guest px is 0.5 CSS px: the canvas FILLS the area (fitted at
  // density, not fitted-then-shrunk by the zoom again — that would be
  // a quarter of the area).
  const fit = Math.min(g.aw / g.bw, g.ah / g.bh);
  expect(g.cw).toBe(Math.round(g.bw * fit));
  expect(g.ch).toBe(Math.round(g.bh * fit));
  expect(g.cw).toBeGreaterThanOrEqual(g.aw * 0.98);
});

test('guest resize request is gated on the toggle', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // No connect is attempted in this spec (the stub's SPICE service
  // rejects the ticket anyway), so window.__spice stays undefined;
  // toggling must not throw.
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.locator('#spice-guestresize').check();
  await page.waitForTimeout(300);
  await page.locator('#spice-guestresize').uncheck();
  expect(errors).toEqual([]);
  await expect(page.locator('#spice-guestresize')).not.toBeChecked();
});

test('guest resize resolution scales with the zoom', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // Spy on the connection the page would use: capture the MonConfig
  // requests. (No live SPICE session exists against the stub, so this
  // stands in for window.__spice.)
  await page.evaluate(() => {
    (window as any).__spice = {
      calls: [] as number[][],
      resize_window: (_f: number, w: number, h: number) =>
        (window as any).__spice.calls.push([w, h]),
    };
  });
  await page.locator('#spice-guestresize').check();
  await page.waitForTimeout(100);
  const req = async () =>
    page.evaluate(() => (window as any).__spice.calls.at(-1) as [number, number]);
  const area = async () =>
    page.evaluate(() => {
      const s = document.getElementById('spice')!;
      return [s.clientWidth, s.clientHeight] as [number, number];
    });
  const expectReq = async (zoom: number) => {
    const [aw, ah] = await area();
    const [w, h] = await req();
    const z = zoom / 100;
    // logical pixels available, scaled by the zoom, rounded down to a
    // multiple of 8 on each axis
    expect(w).toBe(Math.floor(aw / z / 8) * 8);
    expect(h).toBe(Math.floor(ah / z / 8) * 8);
  };
  await expectReq(100);
  const [w100, h100] = await req();
  // 50% zoom: the resolution is 2x the area along each axis (modulo the
  // multiple-of-8 rounding)
  await page.locator('#spice-zoom').selectOption('50');
  await page.waitForTimeout(700); // debounced
  const [aw, ah] = await area();
  const [w50, h50] = await req();
  expect(w50).toBe(Math.floor(aw / 0.5 / 8) * 8);
  expect(h50).toBe(Math.floor(ah / 0.5 / 8) * 8);
  expect(Math.abs(w50 - 2 * w100)).toBeLessThanOrEqual(8);
  expect(Math.abs(h50 - 2 * h100)).toBeLessThanOrEqual(8);
  await expectReq(50);
  // back to 100%: the original (area-sized) request
  await page.locator('#spice-zoom').selectOption('100');
  await page.waitForTimeout(700);
  await expectReq(100);
});

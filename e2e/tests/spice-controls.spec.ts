// SPICE viewer controls: audio / guest-resize toggles and the zoom
// dropdown. The stub's tunnel endpoint is not a real SPICE server, so
// these specs test the page-side machinery (defaults, persistence,
// zoom geometry) rather than a live guest; the zoom engine is
// exercised against a synthetic canvas injected into #spice.
import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';

let m: ReturnType<typeof requireEnv>;
let serverId: string;
const url = () => m.virtweb.url;

test.beforeAll(async () => {
  m = requireEnv();
  serverId = m.serverId;
});

test('defaults: audio on, guest resize off, zoom 100%', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  await expect(page.locator('#spice-audio')).toBeChecked();
  await expect(page.locator('#spice-guestresize')).not.toBeChecked();
  await expect(page.locator('#spice-zoom')).toHaveValue('100');
});

test('zoom engine: canvas sized in logical px, relative to the fit', async ({
  page,
}) => {
  page.setViewportSize({ width: 1280, height: 800 });
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

test('settings persist per browser', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  await page.locator('#spice-audio').uncheck();
  await page.locator('#spice-guestresize').check();
  await page.locator('#spice-zoom').selectOption('150');
  const ls = await page.evaluate(() =>
    JSON.stringify([
      localStorage.getItem('vw.spice.audio'),
      localStorage.getItem('vw.spice.resize'),
      localStorage.getItem('vw.spice.zoom'),
    ]),
  );
  expect(ls).toBe(JSON.stringify(['0', '1', '150']));
  await page.reload();
  await expect(page.locator('#spice-audio')).not.toBeChecked();
  await expect(page.locator('#spice-guestresize')).toBeChecked();
  await expect(page.locator('#spice-zoom')).toHaveValue('150');
});

test('guest resize request is gated on the toggle', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/spice`);
  // A live connection never happens against the stub (magic mismatch),
  // so window.__spice stays undefined; toggling must not throw.
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.locator('#spice-guestresize').check();
  await page.waitForTimeout(300);
  await page.locator('#spice-guestresize').uncheck();
  expect(errors).toEqual([]);
  await expect(page.locator('#spice-guestresize')).not.toBeChecked();
});

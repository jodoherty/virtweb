// Reverse-proxy specs. The env proxy (if any — `npm run env:up --
// --proxy <kind>`) is a TCP (L4) pass-through: it forwards the raw
// TLS stream, and every other spec is transparent to it (they all use
// the manifest URL). This file carries the proxy-SPECIFIC assertions:
//
//   1. incremental arrival — the "console drip" VM emits one line
//      every 300 ms; through a real proxy the arrivals must stay
//      spread in time, not be coalesced into one batch (runs with or
//      without a proxy — raw localhost is the baseline);
//   2. idle survival — the env proxy's idle timeouts are 30 s; an
//      idle session must still be alive at 35 s. The client is
//      byte-silent, so this is the server's h1 keepalive pings (and
//      the browser's h2 PINGs on the browser leg) keeping the raw
//      stream busy;
//   3. negative control — a throwaway proxy with 4 s idle timeouts in
//      front of the same virtweb process MUST cut the idle session
//      (~4-6 s): proves the proxies in these tests actually cut idle
//      connections, so spec 2 is not vacuous.

import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api } from '../src/api.js';
import { StreamSession } from '../src/ws.js';
import {
  startNegativeControl,
  negativeControlDown,
} from '../src/env/proxy.js';
import { waitReady } from '../src/env/virtweb.js';
import { waitFor } from '../src/wait.js';
import { waitForNoSession } from '../src/console.js';

// startDrip boots the drip VM (the stub's start is async — a console
// attach before it is running fails with "not running").
async function startDrip() {
  // A pre-existing session (e.g. left by the previous spec) would 409
  // the next attach.
  await waitForNoSession(m.virtweb.url, sid, 'drip');
  await api.action(sid, 'drip', 'destroy').catch(() => {});
  await api.action(sid, 'drip', 'start');
  await waitFor(async () => {
    const vms = await api.listVMs(sid);
    return vms.find((v) => v.name === 'drip')?.state === 'running';
  }, { message: 'drip running' });
}

const m = requireEnv();
const kind = m.proxy?.kind ?? 'none';
const api = new Api(m.virtweb.url); // the proxy URL, when a proxy is up
const sid = m.serverId;

test.describe('reverse proxy (env proxy: ' + kind + ')', () => {
  // -----------------------------------------------------------------
  // Incremental arrival: the drip VM emits "tick N" every 300 ms
  // (8 ticks, ~2.1 s total). A proxy that buffers the stream would
  // deliver the whole drip in one batch (all ticks with ~0 ms
  // spread); an unbuffered one keeps the arrivals spread in time.
  // -----------------------------------------------------------------
  test('console session data arrives incrementally', async () => {
    await startDrip();

    const ticks = new Map<number, number>(); // tick no -> arrival ms
    let t0 = -1;
    let firstDataAt = -1;
    let gotPrompt = false;

    const s = await new StreamSession().open({
      baseUrl: m.virtweb.url,
      path: `/api/servers/${sid}/vms/drip/console/stream`,
      onChunk: (chunk) => {
        if (t0 < 0) t0 = Date.now();
        if (firstDataAt < 0) firstDataAt = Date.now() - t0;
        const text = Buffer.from(chunk).toString('utf8');
        for (const line of text.split('\n')) {
          const mm = /^tick (\d+)$/.exec(line);
          if (mm) ticks.set(Number(mm[1]), Date.now() - t0);
        }
        if (text.includes('drip:~#')) gotPrompt = true;
      },
    });
    expect(s.status).toBe(200);
    try {
      const deadline = Date.now() + 20_000;
      while (ticks.size < 8 || !gotPrompt) {
        if (Date.now() > deadline) break;
        if (s.terminal) break; // session died early
        await new Promise((r) => setTimeout(r, 100));
      }
    } finally {
      await s.close();
      await waitForNoSession(m.virtweb.url, sid, 'drip', { timeout: 15_000 });
    }

    // All eight ticks, none lost through the proxy.
    expect(ticks.size, `ticks seen: ${[...ticks.keys()].join(',')}`).toBe(8);
    // Nothing buffered at the start: the first DATA arrived promptly.
    expect(firstDataAt, `first DATA at ${firstDataAt} ms`).toBeGreaterThanOrEqual(0);
    expect(firstDataAt).toBeLessThan(3_000);
    // The spread: ticks are 300 ms apart at the source. A buffering
    // proxy collapses this to ~0 (or to a few coarse batches).
    const spread =
      Math.max(...ticks.values()) - Math.min(...ticks.values());
    expect(
      spread,
      `tick spread ${spread} ms (want > 1000; arrivals: ` +
        Array.from(ticks.entries()).map(([n, t]) => `${n}@${t}`).join(' '),
    ).toBeGreaterThan(1_000);
    // And most ticks arrived in separate bursts, not one.
    const bursts = new Set([...ticks.values()].map((t) => Math.floor(t / 100)));
    expect(bursts.size, 'distinct arrival bursts').toBeGreaterThanOrEqual(5);
  });

  // -----------------------------------------------------------------
  // Idle survival through the env proxy (30 s idle timeouts): open the
  // session, let the drip pass, then stay byte-silent and require the
  // session to still be alive 35 s after the last data. The client
  // sends nothing, so only server-side keepalives (h1 pings) can keep
  // the raw stream busy.
  // -----------------------------------------------------------------
  test('idle console session survives the proxy idle timeout', async () => {
    test.skip(kind === 'none', 'no reverse proxy in this env');
    test.setTimeout(180_000);

    await startDrip();

    const s = await new StreamSession().open({
      baseUrl: m.virtweb.url,
      path: `/api/servers/${sid}/vms/drip/console/stream`,
    });
    expect(s.status).toBe(200);

    // Let the drip pass (data is flowing), then go quiet: the session
    // must still be open 35 s later (no close event).
    const deadline = Date.now() + 40_000;
    await new Promise((r) => setTimeout(r, 4_000)); // the drip
    while (Date.now() < deadline && !s.terminal) {
      await new Promise((r) => setTimeout(r, 500));
    }

    await s.close();
    await waitForNoSession(m.virtweb.url, sid, 'drip', { timeout: 15_000 });

    // If the session was cut while idle, the terminal arrived before
    // our own close (a transport death: code 1006). Our own close is
    // a clean 1000. Either way, the terminal must be ours.
    expect(
      s.terminal,
      `session terminal ${JSON.stringify(s.terminal)} — an idle session must survive 35 s behind the ${kind} proxy (its 30 s timeouts lose to the keepalives)`,
    ).toMatchObject({ code: 1000 });
  });

  // -----------------------------------------------------------------
  // Negative control: a throwaway proxy with 4 s idle timeouts in
  // front of the SAME virtweb process. The 15 s keepalive window
  // cannot beat 4 s, so the idle session MUST be cut within ~4-6 s.
  // If this fails (session survives), the survival spec above is
  // vacuous.
  // -----------------------------------------------------------------
  test('proxy with too-short timeouts cuts the idle session (negative control)', async () => {
    test.skip(kind === 'none', 'no reverse proxy in this env');
    test.setTimeout(180_000);

    const inst = await startNegativeControl(
      m.backendPort,
      (url) => waitReady(url, 20_000),
    );
    try {
      await startDrip();

      const s = await new StreamSession().open({
        baseUrl: `https://127.0.0.1:${inst.port}`,
        path: `/api/servers/${sid}/vms/drip/console/stream`,
      });
      expect(s.status).toBe(200);

      // Let the drip pass (data is flowing), then go quiet: the
      // session must end (the proxy cut the connection).
      await new Promise((r) => setTimeout(r, 4_000));
      const t0 = Date.now();
      const terminal = await Promise.race([
        s.closedP,
        new Promise((res) => setTimeout(() => res(null), 20_000)),
      ]);
      expect(
        terminal,
        `the 4 s-timeout proxy should have cut the idle session within 20 s (the keepalive window cannot keep it alive)`,
      ).not.toBeNull();
      expect(terminal.code).toBe(1006); // transport death, not a clean close
      expect(Date.now() - t0).toBeLessThan(15_000);
      await waitForNoSession(m.virtweb.url, sid, 'drip', { timeout: 15_000 });
    } finally {
      negativeControlDown();
    }
  });
});

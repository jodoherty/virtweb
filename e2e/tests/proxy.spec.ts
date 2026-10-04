// Reverse-proxy specs. The env proxy (if any — `npm run env:up --
// --proxy <kind>`) is transparent to every other spec (they all use
// the manifest URL), so this file carries the proxy-SPECIFIC
// assertions:
//
//   1. incremental arrival — the "console drip" VM emits one line
//      every 300 ms; through a real proxy the arrivals must stay
//      spread in time, not be coalesced into one batch (runs with or
//      without a proxy — raw localhost is the baseline);
//   2. idle survival — the env proxy's idle timeouts are 30 s; an
//      idle console stream must still be alive at 35 s, with
//      KEEPALIVE frames observed (virtweb's 15 s keepalives are what
//      keep it busy);
//   3. negative control — a throwaway proxy with 4 s idle timeouts in
//      front of the same virtweb process MUST cut the idle stream
//      (~4-6 s): proves the proxies in these tests actually cut idle
//      streams, so spec 2 is not vacuous.

import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api } from '../src/api.js';
import { FrameDecoder } from '../src/frames.js';
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
  // A pre-existing session (e.g. left by the previous spec; through an
  // L7 proxy a graceful FIN settles late) would 409 the next attach.
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
  // (8 ticks, ~2.1 s total). A proxy that buffers the response would
  // deliver the whole drip in one batch (all ticks with ~0 ms
  // spread); an unbuffered one keeps the arrivals spread in time.
  // -----------------------------------------------------------------
  test('console stream data arrives incrementally', async () => {
    await startDrip();

    const res = await fetch(
      `${m.virtweb.url}/api/servers/${sid}/vms/drip/console/stream`,
    );
    expect(res.status).toBe(200);
    const reader = res.body!.getReader();
    const decoder = new FrameDecoder();

    const t0 = Date.now();
    const tickTimes = new Map<number, number>(); // tick no -> arrival ms
    let firstDataAt = -1;
    let gotPrompt = false;

    try {
      while (Date.now() - t0 < 20_000) {
        let value;
        let done = false;
        try {
          ({ value, done } = await reader.read());
        } catch {
          break; // stream cut (the assertions below will say why)
        }
        if (done) break;
        for (const payload of decoder.feed(value)) {
          if (firstDataAt < 0) firstDataAt = Date.now() - t0;
          const text = Buffer.from(payload).toString('utf8');
          for (const line of text.split('\n')) {
            const mm = /^tick (\d+)$/.exec(line);
            if (mm) tickTimes.set(Number(mm[1]), Date.now() - t0);
          }
          if (text.includes('drip:~#')) gotPrompt = true;
        }
        if (tickTimes.size >= 8 && gotPrompt) break;
      }
    } finally {
      await reader.cancel().catch(() => {});
    }

    // All eight ticks, none lost through the proxy.
    expect(tickTimes.size, `ticks seen: ${[...tickTimes.keys()].join(',')}`).toBe(8);
    // Nothing buffered at the start: the first DATA arrived promptly.
    expect(firstDataAt, `first DATA at ${firstDataAt} ms`).toBeGreaterThanOrEqual(0);
    expect(firstDataAt).toBeLessThan(3_000);
    // The spread: ticks are 300 ms apart at the source. A buffering
    // proxy collapses this to ~0 (or to a few coarse batches).
    const spread =
      Math.max(...tickTimes.values()) - Math.min(...tickTimes.values());
    expect(
      spread,
      `tick spread ${spread} ms (want > 1000; arrivals: ` +
        Array.from(tickTimes.entries()).map(([n, t]) => `${n}@${t}`).join(' '),
    ).toBeGreaterThan(1_000);
    // And most ticks arrived in separate bursts, not one.
    const bursts = new Set([...tickTimes.values()].map((t) => Math.floor(t / 100)));
    expect(bursts.size, 'distinct arrival bursts').toBeGreaterThanOrEqual(5);
  });

  // -----------------------------------------------------------------
  // Idle survival through the env proxy (30 s idle timeouts): read
  // the stream directly, watch for KEEPALIVE frames, and require the
  // stream to still be open 35 s after the last data.
  // -----------------------------------------------------------------
  test('idle console stream survives the proxy idle timeout', async () => {
    test.skip(kind === 'none', 'no reverse proxy in this env');
    test.setTimeout(180_000);

    await startDrip();

    const res = await fetch(
      `${m.virtweb.url}/api/servers/${sid}/vms/drip/console/stream`,
    );
    expect(res.status).toBe(200);
    const reader = res.body!.getReader();
    let keepalives = 0;
    const decoder = new FrameDecoder(() => { keepalives++; });

    // Let the drip pass (data is flowing), then go quiet.
    const t0 = Date.now();
    let lastDataAt = t0;
    let eofAt = -1;
    while (Date.now() - t0 < 40_000) {
      let value;
      let done = false;
      try {
        ({ value, done } = await reader.read());
      } catch {
        eofAt = Date.now();
        break;
      }
      if (done) {
        eofAt = Date.now();
        break;
      }
      for (const payload of decoder.feed(value)) lastDataAt = Date.now();
    }
    await reader.cancel().catch(() => {});

    const idleFor = 35_000;
    if (eofAt > 0) {
      const idleAtEof = eofAt - lastDataAt;
      expect(
        idleAtEof,
        `stream EOF ${idleAtEof} ms after the last data (proxy ${kind} cut the idle stream; 15 s keepalives should beat its 30 s timeouts)`,
      ).toBeGreaterThan(idleFor);
    }
    // The keepalives are what kept it busy: at least one must have
    // crossed (15 s interval; the stream lived ~40 s).
    expect(keepalives, 'KEEPALIVE frames observed').toBeGreaterThanOrEqual(1);
  });

  // -----------------------------------------------------------------
  // Negative control: a throwaway HAProxy with 4 s idle timeouts in
  // front of the SAME virtweb process. The 15 s keepalives cannot
  // beat 4 s, so the idle stream must die within ~4-6 s. If this
  // fails (stream survives), the survival spec above is vacuous.
  // -----------------------------------------------------------------
  test('proxy with too-short timeouts cuts the idle stream (negative control)', async () => {
    test.skip(kind === 'none', 'no reverse proxy in this env');
    test.setTimeout(180_000);

    const inst = await startNegativeControl(
      m.backendPort,
      (url) => waitReady(url, 20_000),
    );
    try {
      await startDrip();

      const res = await fetch(
        `http://127.0.0.1:${inst.port}/api/servers/${sid}/vms/drip/console/stream`,
      );
      expect(res.status).toBe(200);
      const reader = res.body!.getReader();
      // Let the drip pass (data is flowing), then go quiet: the
      // reader must end (the proxy cut the connection).
      await new Promise((r) => setTimeout(r, 4_000));
      const t0 = Date.now();
      let ended = false;
      for (;;) {
        let done = false;
        try {
          ({ done } = await reader.read());
        } catch {
          // The proxy cut the connection hard (RST): the body stream
          // rejects rather than reporting done — that still counts.
          ended = true;
          break;
        }
        if (done) {
          ended = true;
          break;
        }
        if (Date.now() - t0 > 20_000) break; // too long: proxy didn't cut
      }
      expect(
        ended,
        `the 4 s-timeout proxy should have cut the idle stream within 20 s (keepalives at 15 s cannot keep it alive)`,
      ).toBe(true);
      if (ended) expect(Date.now() - t0).toBeLessThan(15_000);
    } finally {
      negativeControlDown();
    }
  });
});

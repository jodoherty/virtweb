// Poll-with-deadline helpers (never fixed sleeps in specs).
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * Polls fn until it returns a truthy value (or throws, which is
 * propagated).
 * @returns the first truthy value
 */
export async function waitFor(fn, { timeout = 15000, interval = 100, message = 'condition' } = {}) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > deadline) {
      throw new Error(`timed out after ${timeout}ms waiting for ${message}`);
    }
    await sleep(interval);
  }
}

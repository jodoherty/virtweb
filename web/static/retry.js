// retry.js — bounded auto-retry for the console/VNC/SPICE pages.
//
// When a session fails or is lost, the page reloads itself a few
// times (location.reload, the same affordance as the Reconnect
// button). The count lives in sessionStorage, so it survives the
// reloads but dies with the tab — a dead server stops the loop after
// MAX_RETRIES instead of spinning forever. A successful connection
// clears the counter (clearRetries).

export const MAX_RETRIES = 5;
export const RETRY_DELAY_MS = 3000;

function key() {
  return 'vw-retry:' + location.pathname + location.search;
}

/** How many failed attempts this page has made (across reloads). */
export function retryCount() {
  const n = parseInt(sessionStorage.getItem(key()) || '0', 10);
  return Number.isFinite(n) ? n : 0;
}

/**
 * Record a failed attempt and reload after RETRY_DELAY_MS if the
 * budget is left. Returns true if a retry will happen (the caller
 * should say so to the user), false if the budget is exhausted
 * (fall back to the manual Reconnect button).
 */
export function scheduleRetry({ delayMs = RETRY_DELAY_MS, max = MAX_RETRIES } = {}) {
  if (retryCount() >= max) return false;
  sessionStorage.setItem(key(), String(retryCount() + 1));
  setTimeout(() => location.reload(), delayMs);
  return true;
}

/** Clear the budget (call on a successful connection). */
export function clearRetries() {
  sessionStorage.removeItem(key());
}

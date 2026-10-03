# Fork: noVNC 1.7.0 (MPL-2.0)

This is a **first-party fork** of noVNC, developed in this repository's
git history (no separate fork repository) — the virtweb commits touching
this directory *are* the fork's history. Like the spice-html5 fork,
files under this directory are our code: edits are allowed, but keep
them minimal and confined, and record them in the fork delta below.

- Upstream: https://github.com/kanaka/noVNC
- Fork base: release **1.7.0**
- Source: `noVNC-1.7.0/` in the repo root (pinned release tree)
- Copied: 2026-10-03
- Files kept: `core/` (the library — ES modules; `core/rfb.js` exports
  `RFB`), `vendor/` (third-party support libraries noVNC itself depends
  on, chiefly pako for zlib inflate in the zrle/tight decoders),
  `LICENSE` (MPL-2.0, as `LICENSE.txt` upstream)
- Usage: `import RFB from '/novnc/core/rfb.js'` (see `web/static/vnc.js`).
  The VNC transport is virtweb's own: the `ChannelSocket` raw-channel
  adapter bridges virtweb's `DuplexChannel` to `Websock.attach()`;
  noVNC's `WebSock` (WebSocket) is not used.

## Fork delta (keep changes minimal and confined)

*(none — byte-identical to the 1.7.0 base as of 2026-10-03; the
transport bridge lives outside this tree in `web/static/vnc.js`)*

Record any change here: file, what, why, and the virtweb commit.

## Re-basing on upstream

1. Refresh the pinned release tree at repo root `noVNC-1.7.0/` (or a
   fresh checkout of the new release).
2. Re-copy the kept file set; re-apply any fork delta listed above; run
   the e2e suite (transport smoke included).
3. Update the fork base and delta in this file.

## Audit notes (2026-10-03, virtweb security review)

Audited as first-party code. Trust boundary: a hostile VNC server
(e.g. a compromised guest/QEMU) feeds us untrusted protocol bytes;
noVNC is the parser. Findings:

**Safe (verified):**
- No `eval` / `innerHTML` / `document.write` / dynamic code anywhere
  in `core/` or `vendor/`. Server data reaches only canvas drawing
  (`display.js`, `core/decoders/*`); the cursor is a canvas bitmap
  rendered via `toDataURL()` into `style.cursor` (base64 data URL —
  no CSS breakout).
- Malformed protocol handling is contained: `decodeRect` is wrapped in
  try/catch → `_fail()` → clean disconnect; oversized typed-array
  allocations throw `RangeError` into that same catch.
- Server-declared framebuffer/cursor dimensions are u16; canvas
  allocation is bounded by the browser's canvas limits, and
  `getImageData` uses the *actual* (browser-clamped) canvas size, not
  the declared one.
- `WebSock` (WebSocket impl) is loaded by the import graph but unused
  in our wiring (`ChannelSocket` bridge instead); it has no sinks.
- `vendor/pako` (zlib inflate for zrle/tight) is pure compute; its
  upstream history is the vetting record.

**Accepted residuals (hostile-*server* DoS class, no RCE):**
- ZRLE: the 32-bit zlib stream-length field is trusted — the client
  buffers that many bytes before inflating (`zrle.js`). A malicious
  server streaming gigabytes exhausts the tab; QEMU sends real
  streams.
- tight: `w*h*4` allocation from the rect header before the browser
  throws — contained by the decode try/catch.
- Cursor: `w*h*4` bytes are waited on before processing — same class.

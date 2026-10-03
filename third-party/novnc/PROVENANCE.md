# Vendored: noVNC 1.7.0 (MPL-2.0)

These files are the **upstream source, unmodified**. Do not edit anything
under this directory. To replace: delete this directory and re-copy from a
fresh checkout of the pinned release, recording the new commit below.

- Upstream: https://github.com/kanaka/noVNC
- Version: 1.7.0
- Source: `noVNC-1.7.0/` in the repo root (pinned release tree)
- Copied: 2026-10-03
- Files kept: `core/` (the library — ES modules; `core/rfb.js` exports
  `RFB`), `vendor/` (third-party support libraries noVNC itself depends on),
  `LICENSE` (MPL-2.0, as `LICENSE.txt` upstream)
- Usage: `import RFB from '/novnc/core/rfb.js'` (see `docs/LIBRARY.md`
  upstream). The VNC transport (M6) is virtweb's own: an `ssh -W` stdio
  tunnel bridged over the same chunked-HTTP streams pattern as the serial
  console — noVNC connects to a WebSocket, so M6 adds a small bridge.

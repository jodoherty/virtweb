# Vendored: spice-html5 — FORK (LGPL-3.0-or-later)

Unlike the other vendored libraries, this one is **modified**. Upstream
spice-html5 is hardwired to WebSocket (`SpiceConn` creates
`new WebSocket(o.uri, 'binary')` and every child channel clones
`this.ws.url`); there is no transport seam. The fork replaces that
transport with virtweb's plain-HTTP streams and is developed **in this
repository's git history** (no separate fork repository) — the virtweb
commits touching this directory *are* the fork's history.

- Upstream: https://gitlab.freedesktop.org/spice/spice-html5
- Upstream base: `b372aab6e5ab3ce056f486d3bdcd3845f9e34aad`
  (2026-09-21, "Remove the ES module polyfill")
- Copied: 2026-10-03 (flattened: no `.git`, no `package.json.in`,
  no Makefile/spec/apache sample; `src/`, `spice_auto.html`,
  `spice.css`, licenses kept)
- Usage: `import { SpiceMainConn } from '/spice/src/main.js'`
  (see `web/static/spice.js`); transport endpoints are
  `GET/POST /api/servers/{id}/vms/{name}/tunnel/stream|input`

## Fork delta (keep changes confined to the transport seam)

| File | Change |
|---|---|
| `src/spicestream.js` | **new** — `SpiceStream`, the WebSocket-shaped channel over virtweb's `DuplexChannel` (`web/static/duplex.js`, chunked flushed GET + ordered POST uplink) |
| `src/spiceconn.js` | `SpiceConn` constructs `SpiceStream` instead of `WebSocket`; dropped the synchronous `readyState` connect check (the stream opens asynchronously; failures come through error/close); `send_msg` coalesces frames per animation tick (one atomic uplink chunk per tick — the spice stream accepts concatenated messages; this is the ordering + POST-rate guarantee); `cleanup` discards pending frames |
| `src/main.js` | child channels clone `inputUri: this.ws.inputUrl` alongside `uri` |
| `src/inputs.js` | `WebSocket.OPEN` → literal `1` (SpiceStream keeps the numeric readyState) |

Untouched: everything else (display, cursor, playback, filexfer,
wire reader, protocol code) is byte-identical to the upstream base.
`wire.js` needs no change: it only uses `binaryType` and
`addEventListener('message')`, which SpiceStream provides.

## Re-basing on upstream

1. Refresh the scratch checkout at repo root `spice-html5/`
   (or any upstream clone) to the new commit.
2. Re-apply the four-file delta above (it is small and confined to
   the transport seam); run the e2e suite (transport smoke included).
3. Update the upstream base commit in this file.

Do **not** edit vendored files in place except through commits that
are recognizably part of the fork delta; `git log -- third-party/spice-html5-fork`
is the delta's record.

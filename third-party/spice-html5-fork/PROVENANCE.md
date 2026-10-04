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
| `src/spicestream.js` | **new** — `SpiceStream`, the WebSocket-shaped channel over virtweb's `DuplexChannel` (`web/static/duplex.js`, chunked flushed GET + ordered POST uplink). `message` events carry **fresh ArrayBuffers** (WebSocket `binaryType: arraybuffer` semantics; the fork's wire reader builds DataView over them — the DuplexChannel's Uint8Array payloads are copied). The downlink's terminal state (wire protocol v2: a SESSION_ERROR frame or a transport failure) is reported through an `error` event before the `close`, so the page's onerror shows the server's message |
| `src/spiceconn.js` | `SpiceConn` constructs `SpiceStream` instead of `WebSocket`; dropped the synchronous `readyState` connect check (the stream opens asynchronously; failures come through error/close); `send_msg` coalesces frames per animation tick (one atomic uplink chunk per tick — the spice stream accepts concatenated messages; this is the ordering + POST-rate guarantee); `cleanup` discards pending frames; the `error` listener's `e.target` access is guarded (SpiceStream's error events are plain objects, not DOM events, and `'url' in e.target` threw uncaught) |
| `src/main.js` | child channels clone `inputUri: this.ws.inputUrl` alongside `uri`; `SpiceMainConn` wraps the page's `onerror` so a main-channel failure tears the sibling channels down first (`close_sibling_channels` — without it their tunnels, one ssh -W session each, persist until page close) |
| `src/inputs.js` | `WebSocket.OPEN` → literal `1` (SpiceStream keeps the numeric readyState) |
| `src/wire.js` | the wire reader's `message` listener is `bind(this.sc.ws)`'d. **Latent upstream bug, exposed by the transport seam**: upstream registers `wire_blob_catcher` without an explicit receiver, which only works because a real WebSocket dispatches `message` events with `this` = the WebSocket; SpiceStream dispatches with `this` = the stream, so `this.wire_reader` resolved to `undefined` and every inbound protocol message was dropped (`inbound` was never called — the connection sat at "Connecting…" until its handshake timeout). Against a real WebSocket this fix is a no-op |

Untouched: everything else (display, cursor, playback, filexfer,
protocol code) is byte-identical to the upstream base.

## Re-basing on upstream

1. Refresh the scratch checkout at repo root `spice-html5/`
   (or any upstream clone) to the new commit.
2. Re-apply the four-file delta above (it is small and confined to
   the transport seam); run the e2e suite (transport smoke included).
3. Update the upstream base commit in this file.

Do **not** edit vendored files in place except through commits that
are recognizably part of the fork delta; `git log -- third-party/spice-html5-fork`
is the delta's record.

## Audit notes (2026-10-03, virtweb security review)

Audited as first-party code (fork delta + the upstream protocol
path). Trust boundary: a hostile SPICE server (compromised
guest/QEMU) feeds untrusted protocol bytes; `wire.js` reassembles
them, `spicemsg.js` decodes them. Findings:

**Safe (verified):**
- Fork delta adds no new attack surface: `SpiceStream` is a
  WebSocket-shaped byte channel; `send_msg` coalesces per animation
  tick (order preserved, uplink stays a trickle of small POSTs);
  `cleanup`/`handle_timeout` discard pending frames and actually
  close the stream (no zombie sessions).
- `wire.js` `inbound()` is a correct streaming accumulator: it
  buffers partial blocks and slices exactly `needed` bytes, so
  arbitrary chunk splits from `DuplexChannel` are safe (slices copy,
  no aliasing).
- Server data reaches only canvas drawing (`display.js`, `bitmap.js`);
  logs use `document.createTextNode` (no `innerHTML`).
- Malformed-message handling is contained: `process_message` wraps the
  channel dispatch in try/catch so one bad message can't desync the
  wire reader.
- RSA ticket (`ticket.js`): the server-supplied public key is parsed
  from a fixed 512-byte field with an explicit "keys > 255 bytes
  unsupported" bound, so a hostile server cannot force a huge
  modulus/exponent into the jsbn/bigint math; `OAEP` padding checks
  `padlen` before allocating.
- `SpiceMsgChannels` caps parsing reads `num_of_channels` from the
  message but only reads as many `SpiceChannelId`s as the buffer
  actually contains (`from_dv` advances `at` and the DataView bounds
  the reads) — a hostile `num_of_channels` cannot over-allocate.

**Accepted residuals (hostile-*server* DoS class, no RCE):**
- A hostile server can still stream large display/cursor/pixmap
  payloads that the wire reader buffers before decoding (bounded by
  the browser's memory, not by the protocol); QEMU sends real
  payloads. `DEBUG` is 0 in the vendored tree.

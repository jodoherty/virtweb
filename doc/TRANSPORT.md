# Transport wire format (v1 + v2)

The protocol spoken by the console and display streams. Usage:
[USAGE.md](USAGE.md). Deployment (proxy buffering/idle timeouts):
[SECURITY.md](SECURITY.md#reverse-proxy-notes).

The console and display streams are not raw byte pipes: both
directions speak a small length-framed protocol, one frame per flush:

```
frame := [tag:1][len:4 big-endian][payload]
```

| tag | name | use |
|-----|------|-----|
| `0x01` | HELLO | first frame of every downlink stream; payload `[version=1][flags]`, where bit 0 of `flags` announces the v2 terminal frames. Clients refuse non-matching streams |
| `0x02` | DATA | the session's raw bytes (console output/keystrokes, RFB, SPICE per channel). Each uplink POST body is one DATA frame |
| `0x03` | KEEPALIVE | empty; emitted by *each direction* while it has been idle for the keepalive interval |
| `0x04` | SESSION_END | **v2**, downlink only; the session ended cleanly. Empty payload. Always the last frame before the stream closes |
| `0x05` | SESSION_ERROR | **v2**, downlink only; the session failed. Payload is the server's message (e.g. the ssh/virsh error). Always the last frame |

Why frames exist, given virtweb is an opaque byte tunnel:

- **Idle keepalives survive any proxy topology.** An idle serial
  console or a static display is byte-silent; a TCP reverse proxy with
  its own read/idle timeouts would cut it. Each stream pings itself
  (server pings the downlink, the browser pings the uplink), so no
  stream can go quiet for longer than ~2× the interval — under L4/h2,
  L7, or h1.1. The keepalive *cannot* live in the guest protocol
  (RFB/SPICE/pty) because virtweb doesn't parse it; it lives one layer
  up, in frames the tunnel itself emits.
- **Corruption is detectable.** Without framing, a byte dropped
  mid-stream desyncs a VNC/SPICE session silently; a torn or over-size
  frame fails the channel cleanly ("connection lost" + Reconnect)
  instead.
- **Terminal state is in-band (v2).** The session's final state
  arrives as a frame before the stream ends: the UI can show *why* a
  session died (the ssh/virsh error, verbatim) instead of guessing from
  how the stream did. The version stays at 1 — the frame shape and
  HELLO/DATA are unchanged; v2 is a capability bit, not a new version.
  (In-band client→server terminal frames are the planned v3 extension,
  announced the same way.)

The browser parses the frames in `web/static/duplex.js`
(`FrameDecoder`); the consumers (xterm, noVNC, the SPICE fork) only see
DATA payloads. A session that ends with SESSION_ERROR surfaces as
"Connection lost: <message>"; a clean SESSION_END as "Session ended.".
When the *client* goes away, no terminal frame is emitted — the client
is gone, and the stream abort is itself the signal.

## A Firefox quirk: one URL, one in-flight GET

Firefox (observed: 155) queues a second in-flight GET to the *exact
same URL* behind the first — it stays pending indefinitely, while
Chrome opens both. SPICE child channels would all fetch the master's
stream URL, so on Firefox only the master channel ever started
("Connected." with a blank display). `DuplexChannel` therefore gives
every channel a unique stream URL (an opaque `s=` parameter the
server ignores); each GET opens a fresh session anyway, so the
distinct URLs are semantically correct, not just a workaround.

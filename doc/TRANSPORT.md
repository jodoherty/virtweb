# The session transport

The protocol spoken by the serial console and the display tunnels
(VNC/SPICE). Usage: [USAGE.md](USAGE.md). Deployment (proxy timeouts,
L4 pass-through): [SECURITY.md](SECURITY.md#reverse-proxy-notes).

## What it is

One **WebSocket session** per logical stream — a serial console, or
one display channel (a SPICE session opens several: main, display,
inputs, cursor, playback). Each session is the same URL for its whole
lifetime; the browser's native `WebSocket` is the entire client
implementation. Binary messages carry the session's raw bytes in both
directions (console output / keystrokes; RFB; SPICE per channel).
Nothing is multiplexed above the transport — if h2 transport-level
multiplexing is needed, that is what it is for.

There is no custom framing and no application protocol on top of the
bytes. Terminal state rides the WebSocket close code:

| close | meaning | UI |
|-------|---------|-----|
| `1000` | the session ended cleanly (VM stopped, console detached, remote hang-up) | "Session ended." + Reconnect |
| `1011` | the session failed server-side; the close **reason** carries the server's message (the ssh/virsh error; bounded on the wire to 123 bytes, the full message stays in the server log) | "Connection lost: <message>" + auto-retry |
| `1006` | transport death (proxy cut, backend restart, keepalive timeout) | "Connection lost" + auto-retry |

When the *client* goes away, no terminal close is sent — the client is
gone and the transport dying is itself the signal; the server kills
the backend session through its pipes.

## Preflight

The browser's `WebSocket` API (and Node's) does not surface the
handshake status, so the server's pre-upgrade rejections —
`409` (a console session for this VM is already open), `429` (session
cap), `400` (bad `?display=`), `503` (the VM has no such display),
`404` (unknown server) — are read with a `HEAD` to the same URL before
the handshake. A `200` preflight means the session may open; the
handshake itself can still race (e.g. another session takes the slot
between the two).

## The two legs

Browsers negotiate HTTP/2 over ALPN on the TLS listener, so all of a
tab's sessions ride **one h2 connection** as extended-CONNECT streams
(RFC 8441) — one stream per session, no per-origin connection-budget
pressure, transport-level keepalive. Tooling (curl-adjacent clients,
the e2e harness) and h2-less browsers use the plain **HTTP/1.1
Upgrade** leg; it is a supported fallback, not a degradation.

### Extended CONNECT and the Go toolchain

Go's stdlib HTTP/2 server does not route extended CONNECT by default —
it is gated behind `GODEBUG=http2xconnect=1`, read once at process
start. h2 is the designed primary path, so the binary guarantees its
own invariant: at startup `virtweb` re-executes itself once with the
setting appended to `GODEBUG` (see
`ensureExtendedConnect` in `main.go`). The startup log says so.

### The h1 warning

A session that lands on the HTTP/1.1 leg instead of HTTP/2 is logged
with a verbose, throttled warning (per client — the mTLS certificate
CN when mTLS is on, else the peer address; repeated occurrences
suppressed for five minutes and reported on the next warning). A
browser session on h1 means ALPN was not negotiated (client
configuration or a middlebox), which changes the session's liveness
semantics and makes it count against the client's per-origin
connection budget. The page shows it as a `· h1` marker next to the
status. If this warning appears for browser traffic in production,
check ALPN end to end.

## Liveness

- **HTTP/1.1 leg:** the vendored `ws` package probes a silent session
  with a ping once the idle window is reached and considers the
  connection dead after a second window. The window is `-keepalive`
  (default 15 s). The browser answers pings automatically — there are
  no client-side keepalive timers (and nothing is throttled in hidden
  tabs).
- **HTTP/2 leg:** h2 streams expose no per-stream deadline, so the
  extended-CONNECT session runs with both deadline windows zero (the
  package refuses a nonzero window there). Liveness is the
  transport's job: the browser's h2 PINGs keep the connection busy,
  and the reverse proxy's idle timeout is a documented deployment
  constraint (a proxy that cuts an idle h2 connection cuts the
  sessions on it — the client re-establishes them; that is the
  designed behaviour, not an error).

A severed session is always re-establishable by the client; the
console page retries a failed or lost session automatically (a
bounded number of reloads — `web/static/retry.js`), and every page
offers Reconnect for clean ends.

## Why WebSocket

The former plain-HTTP transport (a chunked GET downlink plus small
ordered POSTs uplink, with a custom length-framed protocol for
keepalives, terminal state and corruption detection) is gone.
WebSocket is strictly simpler and more responsive:

- **Simplicity:** the framing, keepalive, close-handshake and
  terminal-state machinery is RFC 6455 — a stdlib-only vendored file
  plus `new WebSocket()` in the browser, instead of bespoke Go,
  bespoke browser code, and a wire-protocol document.
- **Responsiveness:** uplink bytes go straight out as frames. The old
  transport paid a per-chunk POST round-trip on the keystroke path
  (visible on serial consoles and the SPICE input channel).
- **Robustness:** close semantics are standard (the close code *is*
  the terminal state), frame integrity is the RFC's, and h2 gives the
  browser's many sessions one connection instead of N connections
  fighting the per-origin budget.

The one behaviour the custom framing provided that had no RFC
equivalent — proxy-surviving idle keepalives — now comes from the
ws package's in-library pings (h1 leg) and the browser's h2 PINGs
(h2 leg), documented under [Liveness](#liveness) and
[SECURITY.md](SECURITY.md#reverse-proxy-notes).

# Security and deployment

How virtweb is hardened and how to run it behind TLS / an mTLS reverse
proxy / an L4 pass-through with the PROXY protocol. Usage:
[USAGE.md](USAGE.md). Stream transport: [TRANSPORT.md](TRANSPORT.md).

## Security

virtweb is designed to run **behind an mTLS reverse proxy** (see
[reverse-proxy deployment](#tls-mtls-and-the-proxy-protocol-reverse-proxy-deployment));
with client-cert auth at the edge, the protections below address what an
*authenticated* client can do:

- **Cross-origin protection (CSRF)** — every response goes through
  Go's `net/http.CrossOriginProtection`: non-safe (POST/DELETE/…)
  cross-origin *browser* requests are rejected (403) via the
  `Sec-Fetch-Site`/`Origin` headers. Same-origin pages and non-browser
  clients (curl, node, API automation) are unaffected.
- **Display tunnels are server-resolved** — the client names which
  display to view (`?display=vnc|spice`); no client-supplied
  `host:port` is accepted, so the tunnel cannot be pointed at arbitrary
  services from the remote host (SSRF).
- **Session caps** — at most 64 live console sessions and 64 live
  tunnel sessions (each holds a remote ssh process); further opens get
  429.
- **Per-host ssh concurrency cap** — at most 8 concurrent ssh
  invocations per managed host (bounds the `virsh list` fan-out).
- **Request body caps** — console input 64 KiB, tunnel input 512 KiB,
  JSON server-add 1 MiB.
- **Response hardening** — `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` (server ids
  live in URL paths), and a strict CSP (`script-src 'self'`; no inline
  scripts — confirm dialogs use `data-confirm` + `web/static/ui.js`).
- **SSH host-key policy** — `-host-key-checking` (default
  `accept-new`, i.e. trust-first-contact); use `no` with a seeded
  `known_hosts` on untrusted paths to a host.
- **Per-server SSH credentials** — a server may have a `key`
  (private key material, OpenSSH/PKCS#8 PEM, pasted in the UI) and/or
  a `password`. The key is written to a 0600 temp file per connection,
  offered *alone* (`-i` + `IdentitiesOnly=yes` — the virtweb host's ssh
  agent and `~/.ssh` defaults are not used), and the temp file is
  removed afterwards. A password is handed to ssh via
  `sshpass -f <0600 temp file>` (never argv, so it is not visible in
  the process list). Both credentials live in `servers.json`
  (mode 0600) — the trade-off of letting an authenticated UI user
  register a host they can only reach with stored credentials (this
  is also what makes container deployments work: no host-side key
  files to mount). The API responses never carry the key material,
  only a `keySet` flag; the edit form (server-rendered from the store,
  not the API) pre-fills both the key and the password, so a routine
  edit doesn't wipe credentials and clearing a box removes it (trusted
  mTLS path). Both empty: no credentials are
  offered — the server is reachable only via the virtweb host's own
  ssh setup.

## TLS, mTLS, and the PROXY protocol (reverse-proxy deployment)

virtweb terminates its own TLS (and optional mTLS client-cert auth),
and can accept the
[PROXY protocol](https://www.haproxy.org/project/proxy-protocol/) that
an L4 load balancer (e.g. a Traefik TCP pass-through router) prepends to
each connection to carry the original client address across an internal
network.

The listener pipeline is fixed order:

```
TCP accept  ->  PROXY header (v1/v2, outside TLS)  ->  TLS / mTLS  ->  HTTP
```

- `-proxy-protocol` makes virtweb read **one** PROXY header per
  connection before anything else, and requires `-proxy-from` to name
  the proxy's source address(es). Connections from any other source are
  rejected *before any data is read* — this is what stops a peer on the
  internal network from forging the header or bypassing the proxy.
- `-client-ca` (with `-tls`) enforces **mTLS**: a client that does not
  present a certificate signed by one of the bundled CAs is refused at
  the handshake.

Typical flags for a behind-the-proxy deployment:

```sh
./virtweb -addr 0.0.0.0:8443 -tls \
  -tls-cert /etc/virtweb/server.pem -tls-key /etc/virtweb/server.key \
  -client-ca /etc/virtweb/client-ca.pem \
  -proxy-protocol -proxy-from 10.0.0.1
```

## Reverse proxy notes

The console/VNC/SPICE transport is a long-lived, chunked HTTP response
plus small POSTs. If you put virtweb behind a proxy (nginx, Caddy,
Traefik, …), make sure the streaming responses are **passed through
unbuffered** — virtweb sends `X-Accel-Buffering: no` and
`Cache-Control: no-cache` for this, but your proxy must honour (or
equivalent) no-buffering, or console/display updates will arrive in
batches instead of live.

**Timeouts:** the streams must also survive the proxy's read/idle
timeouts. virtweb emits idle KEEPALIVE frames on both sides of every
stream (default: after 15 s of silence; tune with `-keepalive`) — see
[TRANSPORT.md](TRANSPORT.md) — so with keepalives a stream never goes
quiet; keep the proxy's idle timeout above ~2× the keepalive interval
(or raise it on the stream routes) as defense-in-depth.

For the L4 pass-through + PROXY protocol + mTLS topology, see
[TLS, mTLS, and the PROXY protocol](#tls-mtls-and-the-proxy-protocol-reverse-proxy-deployment)
above: the proxy forwards the raw TLS stream (with the PROXY header
outside it), so the no-buffering requirement is on the TCP-forwarding
path, and virtweb terminates TLS/mTLS itself.

A complete example — docker-compose with Traefik, Let's Encrypt TLS
and basic auth (L7, plain-HTTP virtweb behind it) — lives in
[deployment-example/](deployment-example/).

# Security and deployment

How virtweb is hardened and how to run it behind TLS / an mTLS reverse
proxy / an L4 pass-through with the PROXY protocol. Usage:
[USAGE.md](USAGE.md). Stream transport: [TRANSPORT.md](TRANSPORT.md).

## Security

virtweb is designed to run **behind an mTLS reverse proxy** (see
[reverse-proxy deployment](#tls-mtls-and-the-proxy-protocol-reverse-proxy-deployment)).
Client certificate authentication is **on by default**: virtweb starts
only with `-client-ca` (which enforces mTLS) or an explicit `-no-auth`
(an unambiguous opt-out for development/testing). With client-cert auth
at the edge, the protections below address what an *authenticated*
client can do:

- **Cross-origin protection (CSRF)** — every response goes through
  Go's `net/http.CrossOriginProtection`: non-safe (POST/DELETE/…)
  cross-origin *browser* requests are rejected (403) via the
  `Sec-Fetch-Site`/`Origin` headers. Same-origin pages and non-browser
  clients (curl, node, API automation) are unaffected.
- **WebSocket origin policy (CSWSH)** — mTLS authenticates the
  *browser's* connection, not the page that opened it: a malicious
  page in the same browser would present the same client certificate.
  The session upgraders therefore run a strict same-origin check on
  the handshake `Origin` (rejected with `403` before the upgrade; no
  `Origin` header — programmatic clients — is allowed). With TLS
  terminated at virtweb (the supported topology) the check compares
  against `https://<Host>`.
- **Display tunnels are server-resolved** — the client names which
  display to view (`?display=vnc|spice`); no client-supplied
  `host:port` is accepted, so the tunnel cannot be pointed at arbitrary
  services from the remote host (SSRF).
- **Session caps** — at most 64 live console sessions and 64 live
  tunnel sessions (each holds a remote ssh process); further opens get
  429.
- **Per-host ssh concurrency cap** — at most 8 concurrent ssh
  invocations per managed host (bounds the `virsh list` fan-out).
- **Session message caps** — a single WebSocket message on either leg
  of a console or tunnel session is capped at 512 KiB (bound also the
  reassembly allocation on a hostile client).
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
- `-client-ca` enforces **mTLS** (the default posture): a client that
  does not present a certificate signed by one of the bundled CAs is
  refused at the handshake. Without `-client-ca`, virtweb refuses to
  start unless `-no-auth` is passed, which explicitly disables client
  authentication (any TLS client is then admitted — for development
  and testing only).

Typical flags for a behind-the-proxy deployment (TLS is always on;
the certificate files name the served identity):

```sh
./virtweb -addr 0.0.0.0:8443 \
  -tls-cert /etc/virtweb/server.pem -tls-key /etc/virtweb/server.key \
  -client-ca /etc/virtweb/client-ca.pem \
  -proxy-protocol -proxy-from 10.0.0.1
```

## Reverse proxy notes

The supported proxy topology is the **L4 (TCP) pass-through**: the
proxy forwards the raw TLS stream and virtweb terminates TLS (and
mTLS) itself. There is deliberately no L7 axis — an L7 proxy in front
of this app means re-terminating TLS (breaking the mTLS perimeter and
ALPN), buffering long-lived streams, and handling upgrade/CONNECT on
the proxy's own terms.

**Idle timeouts:** the sessions are long-lived and can be byte-silent
(an idle serial console, a static display). Over the L4 pass-through
the proxy sees only the raw TLS stream, so its TCP idle/read timeout
must be tuned against the transport's liveness:

- Sessions on the **HTTP/1.1 leg** never go quiet: the server probes a
  silent session with a WebSocket ping after `-keepalive` (default
  15 s) of silence, so the stream carries traffic at least every
  ~15–30 s. Keep the proxy's idle timeout comfortably above two
  keepalive windows.
- Sessions on the **HTTP/2 leg** ride the browser's h2 connection;
  keepalives there are the browser's h2 PINGs (the extended-CONNECT
  streams themselves run with no deadline — h2 streams cannot
  enforce one). A proxy that cuts the idle connection cuts the
  sessions on it; the clients re-establish them automatically (console
  auto-retry, Reconnect everywhere). That is the designed behaviour,
  but it costs a reconnect, so a long proxy idle timeout is still
  preferable.

Certificate rotation is a restart: Let's Encrypt's DNS-01 keeps
issuance inside the mTLS perimeter (no L7 exception on the edge), and
virtweb is restarted with the new certificate. Open sessions are cut
by the restart and re-establish — the client-side reconnect is part
of the transport design, so the downtime is the restart itself. The
worked procedure (self-signed default, Let's Encrypt or manual
issuance, rotation) is in the
[deployment example](deployment-example/README.md#server-certificate).

For the L4 pass-through + PROXY protocol + mTLS topology, see
[TLS, mTLS, and the PROXY protocol](#tls-mtls-and-the-proxy-protocol-reverse-proxy-deployment)
above: the proxy forwards the raw TLS stream (with the PROXY header
outside it), so all of the above is on the TCP-forwarding path, and
virtweb terminates TLS/mTLS itself.

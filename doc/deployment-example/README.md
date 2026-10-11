# Deployment example

`docker-compose.yml` in this directory: virtweb behind Traefik (v3)
as an **L4 (TCP) pass-through** — the supported topology. Traefik
forwards the raw TLS stream on `:443`; virtweb terminates TLS (and,
with `-client-ca`, mTLS) itself.

```
browser --TLS--> traefik :443 (raw TCP) -- virtweb :8443 (TLS/mTLS)
```

## Quickstart (local test, works out of the box)

The committed `certs/` is a throwaway **self-signed** pair for
`virtweb.example.test`, and the stack runs with `-no-auth` — so after
building the image, it just runs:

```sh
# 1. build the virtweb image (from the repo root)
docker build -t virtweb .

# 2. from this directory
docker compose up -d

# 3. check it (the self-signed certificate needs -k)
curl -fsSk 'https://virtweb.example.test/api/servers'
```

Browsers reject the self-signed certificate until you accept it
(Chrome: `thisisunsafe` at the warning page) or add it to the local
trust store — expected, since it is a placeholder. For anything that
other people use, install a real certificate
([Server certificate](#server-certificate)) and enable mTLS (swap
`-no-auth` for `-client-ca`; see [Authentication](#authentication)).

## Why L4, not L7

An L7 proxy (nginx, Caddy, Traefik in HTTP mode) in front of this app
would have to re-terminate TLS (breaking mTLS at the edge and ALPN),
buffer long-lived streams, and handle the WebSocket upgrade/CONNECT on
its own terms. The L4 pass-through forwards the raw stream and leaves
all of that to virtweb, which terminates TLS/mTLS and negotiates HTTP/2
over ALPN. See [../SECURITY.md](../SECURITY.md#reverse-proxy-notes)
for the idle-timeout constraints and
[../TRANSPORT.md](../TRANSPORT.md) for the transport.

## Authentication

- **mTLS** (recommended, the default posture): set `-client-ca` and
  give each browser a client certificate signed by that CA. The edge
  authenticates the *connection*; the app still checks the WebSocket
  `Origin` for cross-site hijacking (CSWSH) and cross-origin
  state-changing requests (CSRF).
- **No mTLS** (`-no-auth`): no edge auth — basic auth at the L7 edge
  was the old model and is no longer available. Client auth is the
  default, so `-no-auth` is an explicit opt-out; with it, only run the
  stack inside a network that is itself trusted (development/testing).

## Server certificate

virtweb terminates TLS itself and takes the certificate pair from
`-tls-cert` / `-tls-key` — here, `./certs/tls.crt` + `./certs/tls.key`,
mounted read-only into `/etc/virtweb/`. The committed pair is a
self-signed placeholder for `virtweb.example.test`; replace it with a
certificate for your real hostname by any of:

- **Let's Encrypt (ACME, DNS-01).** The L4 pass-through keeps the edge
  at raw TCP — there is no HTTP-01 challenge port to open, so use the
  **DNS-01** validation (certbot or lego, with your DNS provider's
  API). This keeps issuance entirely inside the mTLS perimeter (no
  L7 exception at the edge). Issue to `tls.crt`/`tls.key` in
  `./certs/` (the full chain in `tls.crt`).
- **Manually** — from any CA (or your own internal one): issue a
  certificate for the hostname (with a matching SAN) and drop the PEM
  chain as `tls.crt` and the key as `tls.key` into `./certs/`.

**Installing / rotating** (all options): certificate rotation is a
restart — copy the new pair into `./certs/` and

```sh
docker compose up -d --force-recreate virtweb
```

virtweb restarts onto the new certificate; clients re-establish their
sessions after the restart (the transport treats it as a normal
reconnect, so the downtime is the restart itself). Schedule this on
renewal (cron or a systemd timer) for Let's Encrypt certificates.

`virtweb-data` holds the server list and per-server SSH credentials;
it is the only state that must survive container recreations.

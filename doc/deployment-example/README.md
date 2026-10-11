# Deployment example

`docker-compose.yml` in this directory: virtweb behind Traefik (v3)
as an **L4 (TCP) pass-through** — the supported topology. Traefik
forwards the raw TLS stream on `:443`; virtweb terminates TLS (and,
with `-client-ca`, mTLS) itself.

```
browser --TLS--> traefik :443 (raw TCP) -- virtweb :8443 (TLS/mTLS)
```

```sh
# 1. build the virtweb image (from the repo root)
docker build -t virtweb .

# 2. put a real certificate for your hostname in doc/deployment-example/certs/
#    (tls.crt, tls.key). The committed ones are 30-day throwaway
#    placeholders so the stack runs for local testing.

# 3. edit doc/deployment-example/docker-compose.yml:
#    - real hostname (HostSNI rule)
#    - swap -no-auth for -client-ca to enable mTLS (the production auth)

# 4. from this directory
docker compose up -d

# 5. check it
curl -fsSk 'https://virtweb.example.test/api/servers'
```

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

## Certificate lifecycle

The example mounts `./certs/`. For production-grade issuance **without
opening an L7 exception** in the mTLS perimeter, use ACME **DNS-01**
(certbot or lego with your DNS provider's API). A renewal job copies
the fresh certificate into `./certs/` and runs
`docker compose up -d --force-recreate virtweb`; virtweb restarts onto
the new certificate and the clients re-establish their sessions
(the transport treats the restart as a normal reconnect).

`virtweb-data` holds the server list and per-server SSH credentials;
it is the only state that must survive container recreations.

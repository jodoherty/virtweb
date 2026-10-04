# Deployment example

`docker-compose.yml` in this directory: virtweb behind Traefik (v3)
with Let's Encrypt TLS and HTTP basic auth at the edge, for the
placeholder hostname `virtweb.example.test`.

```sh
# 1. build the virtweb image (from the repo root)
docker build -t virtweb .

# 2. edit doc/deployment-example/docker-compose.yml:
#    - real hostname (virtweb.example.test in two places) + ACME email
#    - basicAuth hash:  traefik password "your-password"
#      (or htpasswd -nbBC 10 your-user "your-password")

# 3. from this directory
docker compose up -d

# 4. check it
curl -fsS -u your-user 'https://virtweb.example.test/api/servers'
```

Notes:

- Plain-HTTP virtweb inside the compose network: Traefik terminates
  TLS, so the `-tls` / `-proxy-protocol` / mTLS flags (the L4
  pass-through topology, see [../SECURITY.md](../SECURITY.md)) are not
  used here.
- The console/VNC/SPICE transport is long-lived chunked HTTP;
  Traefik passes it through unbuffered, and the wire protocol's idle
  keepalives (15 s default) keep quiet streams alive, so no timeout
  tuning is needed on the example config.
- `virtweb-data` holds the server list and per-server SSH
  credentials; `traefik-certs` the ACME account and certificates.
  Both survive container recreations.
- `example.test` is reserved as a placeholder domain and will never
  receive a Let's Encrypt certificate — substitute a real domain with
  a public A record (HTTP-01) before starting the stack.

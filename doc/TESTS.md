# Tests and releasing

The test evidence virtweb stands on, and the release gate. Usage:
[USAGE.md](USAGE.md). The e2e environment itself (commands, the
manifest, attach vs own, debugging) is documented in
[e2e/AGENTS.md](../e2e/AGENTS.md); the design rationale in the parent
directory's `TESTING.md`.

## Unit tests

```sh
go test ./...    # no libvirtd/sshd/network; everything shimmed
```

All external effects go through interfaces (`virt.Backend`,
`sshexec.Runner`) and are faked (canned-output backends, fake `ssh`
shell scripts in `t.TempDir()`, `httptest`).

## End-to-end suite (`e2e/`)

The e2e suite is a self-contained Node/Playwright project in `e2e/`
with two backend tiers:

- **stub** — fast, offline, deterministic (the bulk of the specs).
  Runs on both Chromium and Firefox (`stub` / `stub-firefox`
  projects): Firefox is a production browser and diverges on the
  transport leg (HTTP/2 extended CONNECT vs the HTTP/1.1 Upgrade)
  and on CSS cursor handling.
- **libvirt** (`@real`) — a rootless-podman libvirt container acting as
  the remote host, exercising the real ssh/virsh/transport path.

A third, orthogonal axis puts the whole stub suite behind a **real
L4 (TCP) pass-through proxy** — HAProxy or Traefik, the production
topology — to verify the session transport survives real proxy
buffering and idle-timeout behavior (the e2e environment is over TLS
with a fixture certificate; the proxy forwards the raw stream):

```sh
npm run env:up -- --backend stub --proxy haproxy-l4   # or traefik-l4
npx playwright test --project=stub
```

(`make e2e-proxy` loops both; `e2e/tests/proxy.spec.ts` carries the
proxy-specific assertions, including a negative control that proves
the idle-survival check is not vacuous.)

Run and debug the suite (commands, the manifest, attach vs own,
raw-serial access): **`e2e/AGENTS.md`**.

## Releasing

The definition-of-done gate is encoded in the Makefile so a release is
a single command:

```sh
make check     # always run: go vet + unit tests + the fast (stub) e2e tier
make release   # before a release: `check` + the real libvirt tier + the
               # reverse-proxy tiers (HAProxy and Traefik, L4 pass-through)
```

`make dist` produces the deployable `dist/virtweb-linux-amd64` binary.
**Stub-green is not release-green** — the real libvirt tier and the
proxy tiers must actually run before a release. The real (libvirt) tier
needs podman + gcc + /dev/kvm and is slow (TCG boot); the proxy tiers
need podman and pull small images on first use — so run `make release`
rather than on every build.

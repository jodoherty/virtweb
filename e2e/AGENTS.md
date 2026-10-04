# e2e — Playwright end-to-end suite

Self-contained Node project for end-to-end tests of the `virtweb`
binary. **No nodejs artifacts (package.json, node_modules, lockfiles,
Playwright caches) live outside `e2e/`** — the rest of the project is
vanilla Go/JavaScript. The only dev dependency is `@playwright/test`.

Design rationale and the tier model live in `TESTING.md` (repo root).
This file is the *operator* cheat-sheet: commands, the manifest schema,
and how to debug a live failure.

## Quick start

```sh
cd virtweb/e2e
npm install                      # once (installs @playwright/test)
npm run env:up -- --backend stub # build virtweb + start it (stub backend)
npx playwright test --project=stub   # tier 0: fast, offline, deterministic
```

Tier 2 (real libvirt, slow — TCG boot is minutes, not seconds):

```sh
npm run env:up -- --backend libvirt
npx playwright test --project=libvirt
npm run env:down                 # remove the container; the guest-disk volume stays
```

Run a single spec / test:

```sh
npx playwright test --project=stub tests/ui.spec.ts
npx playwright test --project=stub -g "Start button"        # by title
npx playwright test --project=libvirt --ui                    # interactive UI mode (set breakpoints)
```

## The environment layer

The test *environment* is a **persistent, inspectable artifact** — a
`virtweb` process (and, for tier 2, a podman container that is the fake
remote host) — that tests *attach* to. It is not a test-runner fixture.
Manage it with the `env:*` scripts (all idempotent, non-interactive):

| command | what it does |
|---------|-------------|
| `npm run env:up -- --backend stub\|libvirt [--proxy K]` | build virtweb, start it, (libvirt: build image + start container), optionally start a reverse-proxy container in front of it (`--proxy haproxy-l7\|haproxy-l4\|traefik-l7\|traefik-l4`, default `none`), register the `e2e` server, write the manifest. No-op if already up with the same backend+proxy. |
| `npm run env:down [--wipe]` | stop virtweb, remove the libvirt + proxy containers, clear the manifest. `--wipe` also drops the guest-disk volume (forces re-download). |
| `npm run env:status [--json]` | is it up? prints `url`, `pid`, `backend`, `serverId`. |
| `npm run env:logs` | tail the last 100 lines of the virtweb log. |
| `npm run env:reset` | `env:down` + wipe the data dir (fresh `servers.json` / VM state on next up). |
| `npm run env:restart-api` | rebuild + restart virtweb on the *same* port (after changing Go code); re-registers the server. |
| `npm run env:doctor [--json]` | checklist: node ≥ 18, playwright, go, binary, gcc (tier 2 shim), podman (tier 2). |

### Two lifetime modes

- **attach** (default, `E2E_ENV` unset): tests read the manifest and
  attach to the running env; they **never tear it down**. A failure
  leaves the env live so you can poke at the failed state. This is the
  mode to develop in.
- **own** (`E2E_ENV=own`): `globalSetup` brings up a disposable env and
  `globalTeardown` tears it down after the run. Used by CI; the env does
  *not* survive a failure.

The Playwright config wires this in: `globalSetup`/`globalTeardown` are
only active when `E2E_ENV=own`. In attach mode there is no setup — if
the manifest is missing, tests fail with "run `env:up`".

## The two backends (tiers)

- **`stub`** — tier 0. virtweb runs with `-backend stub` (the in-process
  `internal/stub` fake). Offline, deterministic, instant. The bulk of
  the specs (REST, console protocol, transport, UI) run here. It doubles
  as an offline demo mode.
- **`libvirt`** — tier 2. virtweb runs with `-backend ssh` and reaches a
  rootless **podman container** (name `vw-e2e`) that *is* the remote
  libvirt host: real `sshd` + `libvirtd` + QEMU (TCG) + one Debian
  trixie guest (`vw-e2e-guest`). This exercises the real
  ssh/virsh/transport path end-to-end. Slow by design (TCG boot ~3–5 min,
  graceful shutdown can be 40–90 s). Only the `@real` specs run here.

The container's root cause of slowness and its rootless workarounds
(`/proc/sys` tmpfs overlay, LD_PRELOAD xattr shim, dbus+polkit, TCG
instead of KVM) are documented in `TESTING.md` §"Tier 2".

## The manifest (`.artifacts/env.json`)

The single source of truth tests and agents read:

```jsonc
{
  "backend": "stub | libvirt",
  "created": "<iso8601>",
  "virtweb": { "url": "http://127.0.0.1:<port>", "pid": 0, "log": ".artifacts/virtweb.log" },
  "sshPort": 12345,          // libvirt backend only
  "serverId": "<the 'e2e' server id>"
}
```

Tests get `serverId` and the base URL from this (`src/envinfo.js`).
Ports are ephemeral (chosen at `env:up`); never hardcode them.

### Reach the tier-2 "remote host" directly (debugging)

The manifest's `sshPort` is the container's sshd. It uses the *host's*
key (`~/.ssh/id_ed25519`, mounted into the container's `authorized_keys`):

```sh
ssh -p <sshPort> -o BatchMode=yes root@127.0.0.1 'virsh list --all'   # the "remote host"
podman exec -it vw-e2e bash                                           # inside the container
```

**Raw serial to the guest, bypassing virtweb** — the tool that tells you
whether virtweb corrupted the stream or the guest misbehaved when a
console test fails. The guest serial is a pty (port 0) inside the
container:

```sh
podman exec vw-e2e cat /dev/pts/0                                   # dump what the guest wrote
ssh -tt -p <sshPort> root@127.0.0.1 'virsh console vw-e2e-guest'    # interactive
```

Guest login: `root` / `vw-e2e-pass` (set via the config drive).

## The two Playwright projects

`playwright.config.ts` defines two projects; `workers:1`,
`fullyParallel:false` (all specs mutate the same VMs, so they must run
one at a time):

- `stub` — every spec **except** `*.real.spec.ts`.
- `libvirt` — **only** `*.real.spec.ts`.

Spec files:

| file | tier | covers |
|------|------|--------|
| `tests/api.spec.ts` | stub | REST: servers CRUD, ping, list, actions |
| `tests/console-protocol.spec.ts` | stub | streaming, 409 concurrent, disconnect cleanup, input-after-close, idle KEEPALIVE, wire-protocol-v2 terminal frames, `STUB_SCENARIO` error paths |
| `tests/transport.spec.ts` | stub | console/VNC/SPICE transport smoke over the tunnel, tunnel terminal frames, auto-retry |
| `tests/proxy.spec.ts` | stub | reverse-proxy axis: incremental arrival of the `drip` VM's console stream through the env proxy, idle-stream survival (L7), negative control (4 s-timeout proxy must cut the stream) — the L7/L4 specs skip on `--proxy none` envs |
| `tests/ui.spec.ts` | stub | dashboard, add-server form, VM list, console page, start |
| `tests/guest.real.spec.ts` | libvirt | one sequential story: lifecycle, TCG boot + real IP, real serial-console login + `echo` round-trip, VNC RFB handshake over the tunnel, shutdown |

`guest.real.spec.ts` is a single `.describe.serial` so a failure leaves
the guest in a known, inspectable state.

### The reverse-proxy axis

`--proxy` is orthogonal to `--backend`: the manifest URL becomes the
proxy's URL and *every* stub spec runs through a real HAProxy/Traefik
container (`src/env/proxy.js`, fixtures in `fixtures/proxy/` —
`{{PROXY_PORT}}`/`{{BACKEND_PORT}}` templates rendered at env:up,
mounted read-only into the podman container, `--network host`). The
manifest gains `proxy: {kind, port, container, image}` and
`backendPort` (the direct virtweb port, used by the negative-control
spec). `src/env/proxy.js isL7(kind)` drives the adaptive deadlines in
`playwright.config.ts` (L7 envs get a 90 s test timeout) and in
`waitForNoSession`/`settle` (45 s settles). `make e2e-proxy` (repo
root) loops the four kinds; the release gate includes it. Two quirks
are handled there, not in the specs: the Traefik cold-start 404
window (`waitProxyReady`) and the negative control's own short-lived
proxy (`startNegativeControl`/`negativeControlDown`, container
`vw-e2e-proxy-negctl`).

## Test helpers (`src/`)

- `src/envinfo.js` — `requireEnv()` (loads the manifest), `envUrl()`.
- `src/api.js` — `Api` (fetch wrappers for the REST API).
- `src/frames.js` — wire protocol (mirrors
  `web/static/duplex.js` / `internal/api/frame.go`): `encodeFrame`,
  `TAG`, `HELLO_FLAG_TERMINAL`, `FrameDecoder` (reassembly + HELLO
  check + optional keepalive/terminal observers). The session helpers
  below speak frames, not raw bytes, and surface terminal frames on
  `session.terminal` ({kind: 'end'|'error', message}).
- `src/console.js` — `openConsole()` / `ConsoleSession`: consumes the
  console stream (DATA payloads) and records **every byte** (both
  directions) to `test-results/` for the failing spec.
- `src/tunnel.js` — `openTunnel()` / `TunnelSession`: same, for the
  generic TCP tunnel (VNC/SPICE); `chunkCount` counts DATA frames.
- `src/wait.js` — `waitFor(fn, {timeout, interval})` (poll-with-deadline;
  never fixed sleeps), `sleep`.

Failures land in `test-results/` (Playwright reports + `.console.log`
byte captures). Both `.artifacts/` and `test-results/` are gitignored.

## Debug workflow (the point of the design)

When a spec fails in attach mode, the custom reporter
(`src/cheatsheet.js`) prints the env cheat-sheet. Then:

1. `npm run env:status` / `env:logs` — is virtweb alive, what did it log?
2. Read the failing spec's `test-results/*.console.log` — the exact
   bytes both directions. For console failures this usually tells you
   whether the corruption is in virtweb or the guest.
3. Reach the guest directly (raw serial / `podman exec` above) to confirm
   the guest side is sane.
4. Set a breakpoint in the spec (Playwright UI mode) and re-run just that
   test against the still-live env.

## Ground rules

- **Stub is a simulation; tier 2 is the authority.** Stub output strings
  must track real virsh/libvirt; when they diverge, fix the stub.
- **Stub-green ≠ release-green.** The `libvirt` project must actually run
  (before each release, at minimum).
- **The container is not the production host.** "Passes in e2e" is not
  "works on 192.168.1.4" — the manual smoke test against the real host
  stays part of the definition of done.
- **Keep nodejs out of the Go tree.** Everything Node lives in `e2e/`.
  Home-dir caches from standard tools (`~/.cache/ms-playwright`) are fine.

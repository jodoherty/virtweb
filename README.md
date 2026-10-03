# virtweb

A web-based management dashboard for remote libvirt (KVM/QEMU) hosts.
Point it at one or more hosts over SSH and manage their virtual machines
— lifecycle, serial console, VNC, and SPICE — from a browser.

Everything (lifecycle, console, VNC, SPICE) is reached through the
system `ssh` client on the server; virtweb never links an SSH library and
never opens a direct socket to a hypervisor.

- Add/remove libvirt servers (each reached over SSH)
- View and manage virtual machines: start, shutdown, reboot, destroy
- Use a VM:
  - **serial console** (xterm.js) over the HTTP stream transport
  - **VNC** (noVNC fork) over the same transport
  - **SPICE** (spice-html5 fork) over the same transport
- No WebSocket anywhere: the transport is a chunked, flushed GET (browser
  `ReadableStream`) for server→client bytes plus small, ordered POSTs for
  client→server bytes, one session per (server, VM).

## Tech stack

- **Backend:** Go, standard library only (`net/http`, `embed`, …). No
  third-party dependencies.
- **Frontend:** vanilla JavaScript, no frameworks, no build step.
- **Deployment:** a single binary — the web frontend is embedded with
  `go:embed`.

## Build

The whole project is one dependency-free Go binary (the web frontend is
embedded), so the only build prerequisite is the Go toolchain. There is
no cgo, so cross-compiling for a Linux deploy target is a plain
env-var thing.

```sh
make build      # -> ./virtweb        (native binary for the host you're on)
make dist       # -> dist/virtweb-linux-amd64 (static, relocatable Linux x86-64)
make test       # go test ./...
make vet        # go vet ./...
make clean      # remove ./virtweb and dist/
```

Or without make:

```sh
go build -o virtweb .                                  # native
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o virtweb-linux-amd64 .
```

`make dist` stamps the version from `git describe` (e.g. `virtweb v0.1-3-gabc123`);
`go build` alone reports `virtweb dev`. Check either with
`./virtweb -version`.

## Run

```sh
./virtweb [-addr 127.0.0.1:8080] [-data-dir ~/.virtweb] [-backend ssh|stub]
./virtweb -version     # print the version and exit
```

| flag | default | meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8080` | address to listen on |
| `-data-dir` | `~/.virtweb` | directory for `servers.json` (auto-created) |
| `-backend` | `ssh` | `ssh` (production: virsh over SSH) or `stub` (offline demo/test) |
| `-version` | — | print the version and exit |
| `-tls` | off | serve HTTPS (requires `-tls-cert` and `-tls-key`) |
| `-tls-cert` / `-tls-key` | — | PEM certificate / private key for `-tls` |
| `-client-ca` | — | PEM CA bundle; with `-tls` enables **mTLS** (clients must present a cert signed by it) |
| `-proxy-protocol` | off | expect a PROXY protocol (v1/v2) header on each connection |
| `-proxy-from` | — | with `-proxy-protocol`: comma-separated proxy source IPs/CIDRs; any other source is rejected |
| `-host-key-checking` | `accept-new` | ssh `StrictHostKeyChecking`: `accept-new` (trust first contact, verify afterwards), `yes`, `no` (known_hosts only), or `off` |

The server binds to `127.0.0.1` by default. To use it from another
machine, run it behind a reverse proxy (or listen on a non-loopback
addr) — the console/VNC/SPICE streams must reach the browser
**unbuffered**; see "Reverse proxy notes" and "TLS / mTLS / PROXY
protocol" below.

## Security

virtweb is designed to run **behind an mTLS reverse proxy** (see the
previous section); with client-cert auth at the edge, the protections
below address what an *authenticated* client can do:

- **Cross-origin protection (CSRF)** — every response goes through
  Go's `net/http.CrossOriginProtection`: non-safe (POST/DELETE/…) 
  cross-origin *browser* requests are rejected (403) via the
  `Sec-Fetch-Site`/`Origin` headers. Same-origin pages and non-browser
  clients (curl, node, API automation) are unaffected.
- **Display tunnels are server-resolved** — the client names which
  display to view (`?display=vnc|spice`); no client-supplied `host:port`
  is accepted, so the tunnel cannot be pointed at arbitrary services
  from the remote host (SSRF).
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
- **SSH host-key policy** — `-host-key-checking` (default `accept-new`,
  i.e. trust-first-contact); use `no` with a seeded `known_hosts` on
  untrusted paths to a host.

## TLS, mTLS, and the PROXY protocol (reverse-proxy deployment)

virtweb terminates its own TLS (and optional mTLS client-cert auth),
and can accept the [PROXY protocol](https://www.haproxy.org/project/proxy-protocol/) that
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

## Requirements

**To build:** the Go toolchain (any recent version; the module declares
its own minimum). That's it.

**To run in production (`-backend ssh`), on the host that runs virtweb:**
- Linux, with the OpenSSH client (`ssh`) in the default path. virtweb
  reaches each managed host by running `virsh` over SSH — it never opens
  a direct socket to a hypervisor and never links an SSH library.
  Fixed ssh options: `BatchMode=yes`, `StrictHostKeyChecking=accept-new`,
  `ConnectTimeout=10`.
- **Key-based SSH auth** to every managed host — virtweb is
  non-interactive, so interactive password auth is not supported.
- The remote host must have `virsh` on its PATH (i.e. libvirt installed).

## Setup and configuration

There is no config file to hand-edit for normal use — you add and manage
servers from the web UI, and everything is persisted to
`<data-dir>/servers.json` (default `~/.virtweb/servers.json`). That file
is a plain JSON array; you can also edit it directly while virtweb is
stopped.

1. **Build and start.**
   ```sh
   make build
   ./virtweb            # -> http://127.0.0.1:8080
   ```
2. **Open the URL** and click **Add server**. You'll be prompted for:
   - **name** — your label for this host (unique).
   - **user** — the SSH user on the remote host.
   - **host** — the remote host's address (name or IP).
   - **port** — the SSH port (leave blank for the default 22).
   - **uri** — the libvirt URI. Use **`qemu:///system`** (the default) so
     virtweb talks to the *system* libvirt instance, where VMs live.
     (`qemu:///session` is the per-user session and is usually empty.)
3. **SSH access** must already work from the virtweb host as that user —
   e.g. `ssh -p <port> <user>@<host> virsh -c qemu:///system list` works
   non-interactively. virtweb uses your existing key and known_hosts
   behaviour (new host keys are accepted automatically via
   `accept-new`).

The server list, its persisted `servers.json`, and each server's shape
(`name`, `user`, `host`, `port`, `uri`) are what virtweb keeps — nothing
else is stored. A server you can't reach simply shows an error on its
page until the SSH access is fixed.

### Reverse proxy notes

The console/VNC/SPICE transport is a long-lived, chunked HTTP response
plus small POSTs. If you put virtweb behind a proxy (nginx, Caddy,
Traefik, …), make sure the streaming responses are **passed through
unbuffered** — virtweb sends `X-Accel-Buffering: no` and
`Cache-Control: no-cache` for this, but your proxy must honour (or
equivalent) no-buffering, or console/display updates will arrive in
batches instead of live. For the L4 pass-through + PROXY protocol +
mTLS topology, see "TLS, mTLS, and the PROXY protocol" above: the proxy
forwards the raw TLS stream (with the PROXY header outside it), so the
no-buffering requirement is on the TCP-forwarding path, and virtweb
terminates TLS/mTLS itself.

## Managing VMs

Once a server is added, its page lists every domain with state, vCPUs,
memory, IP (from `domifaddr`), and — for a running domain — its live
display address. Use the row actions (Start / Shutdown / Reboot /
Destroy), or open a VM's console / VNC / SPICE page.

### Console / VNC / SPICE

- **Console** — the serial console (`ssh -tt` → `virsh console`). The page
  reports a distinct status for every terminal state: *Connected*,
  *Session ended*, *Connection lost*, *Connection timed out*, *A session
  is already open*, *Could not connect* — and offers a **Reconnect**
  button once the session is no longer live. One console session per
  (server, VM); a second concurrent open is refused (409).
- **VNC** — noVNC over the SSH stdio TCP tunnel (`ssh -W`). The VNC port
  is read from the domain (auto-port resolved from the live XML while the
  domain is running).
- **SPICE** — the spice-html5 fork. Each SPICE channel opens its own
  concurrent tunnel (the same topology as websockify's
  WebSocket-per-channel). The display address comes from `virsh
  domdisplay` (SPICE ports are auto-assigned at boot and the display is
  often loopback-bound).

Both display tunnels are **server-resolved**: the client names which
display to view (`?display=vnc|spice`) and virtweb resolves the
destination from the VM's live display info. No client-supplied
`host:port` is accepted on the tunnel endpoint — that would let an
authenticated user point the remote SSH user at an arbitrary TCP
service (SSRF).

All three use the same plain-HTTP duplex transport — see
`web/static/duplex.js` for the design notes (ordering, h2, teardown).

## Offline demo mode (`-backend stub`)

`virtweb -backend stub` runs against a deterministic in-process fake
libvirt host instead of a real SSH connection: a fixed VM table
(`debian-dev`, `win10`, …) with a real state machine (start → *starting*
→ running, shutdown/destroy/reboot), virsh-shaped states, and a scripted
shell on the console (echo, `uname -a`, `hostname`, `command not found`).
Useful to explore the UI before a real host is configured, and it is the
backend the e2e suite's fast tier runs against.

Failure injection for tests: `STUB_SCENARIO=fail-ping | slow-boot |
console-drop | tunnel-drop | ssh-down`.

## Tests

```sh
go test ./...    # unit tests — no libvirtd/sshd/network; everything shimmed
```

The end-to-end suite lives in `e2e/` (a self-contained Node/Playwright
project) and has two tiers:

- **stub** — fast, offline, deterministic (the bulk of the specs).
- **libvirt** (`@real`) — a rootless-podman libvirt container acting as
  the remote host, exercising the real ssh/virsh/transport path.

Run and debug it (commands, the manifest, attach vs own, raw-serial
access): see **`e2e/AGENTS.md`**. Design rationale: `TESTING.md` in the
parent directory.

## Releasing

The definition-of-done gate is encoded in the Makefile so a release is a
single command:

```sh
make check     # always run: go vet + unit tests + the fast (stub) e2e tier
make release   # before a release: `check` + the slow, real (libvirt) e2e tier
```

`make dist` produces the deployable `dist/virtweb-linux-amd64` binary.
**Stub-green is not release-green** — the real libvirt tier must actually
run before a release. The real (libvirt) tier needs podman + gcc and is
slow (TCG boot), so run `make release` rather than on every build.

## Layout

```
main.go                    entrypoint (embeds web/, wires store + API, flags)
internal/virt              Server/VM types + the Backend interface (the seam)
internal/sshexec           Runner interface + system-ssh implementation
internal/virsh             Backend impl: virsh over sshexec
internal/stub              deterministic stub Backend (-backend stub)
internal/store             JSON-file server store (atomic, concurrency-safe)
internal/api               REST + console/tunnel stream & input handlers
internal/webui             server-rendered HTML pages
web/                       embedded frontend (templates/ + static/)
third-party/               first-party forks: novnc, xtermjs,
                               spice-html5-fork (PROVENANCE.md each)
e2e/                       Playwright e2e suite (stub + libvirt tiers)
```

## Status

Feature-complete through the console, VNC, and SPICE transports (all over
plain-HTTP streams, no WebSocket). See `ROADMAP.md`/`TODO.md` in the
parent directory for the polish roadmap.

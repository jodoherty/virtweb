# Usage

Building, running, configuring, and using virtweb. Security and
deployment hardening: [SECURITY.md](SECURITY.md). The stream transport:
[TRANSPORT.md](TRANSPORT.md). Tests and release gates: [TESTS.md](TESTS.md).

## What it is

A web-based management dashboard for remote libvirt (KVM/QEMU) hosts.
Point it at one or more hosts over SSH and manage their virtual machines
— lifecycle, serial console, VNC, and SPICE — from a browser.

Everything (lifecycle, console, VNC, SPICE) is reached through the
system `ssh` client on the server; virtweb never links an SSH library and
never opens a direct socket to a hypervisor.

- Add/remove libvirt servers (each reached over SSH)
- View and manage virtual machines: start, shutdown, reboot, destroy
- Use a VM:
  - **serial console** (xterm.js) over the WebSocket session transport
  - **VNC** (noVNC fork) over the same transport
  - **SPICE** (spice-html5 fork) over the same transport
- The transport is one WebSocket session per logical stream — a serial
  console, or one display channel (a SPICE session opens several):
  binary messages both ways, terminal state in the close code, HTTP/2
  extended CONNECT for browsers, HTTP/1.1 Upgrade for tooling. See
  [TRANSPORT.md](TRANSPORT.md).

virtweb always serves TLS (the session transport requires it: browsers
negotiate HTTP/2 over ALPN on TLS listeners only, and the deployment
model is mTLS behind an L4 pass-through). Without `-tls-cert`/`-tls-key`
it generates an ephemeral self-signed development certificate and says
so loudly; production always supplies real certificates. Client
certificate authentication is the default: virtweb starts only with
`-client-ca` (mTLS) or an explicit `-no-auth`.

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

`make dist` stamps the version from `git describe` (e.g.
`virtweb v0.1-3-gabc123`); `go build` alone reports `virtweb dev`.
Check either with `./virtweb -version`.

## Run

```sh
# production (mTLS is the default; -client-ca is required):
./virtweb -addr 127.0.0.1:8080 -client-ca /etc/virtweb/client-ca.pem
# offline demo (explicitly without client auth):
./virtweb -backend stub -no-auth
./virtweb -version     # print the version and exit
```

| flag | default | meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8080` | address to listen on |
| `-data-dir` | `~/.virtweb` | directory for `servers.json` (auto-created) |
| `-backend` | `ssh` | `ssh` (production: virsh over SSH) or `stub` (offline demo/test) |
| `-version` | — | print the version and exit |
| `-tls-cert` / `-tls-key` | — | PEM certificate / private key. TLS is always served; when these are omitted virtweb generates an ephemeral self-signed development certificate (with a loud warning) — for production, always supply real certificates |
| `-client-ca` | — | PEM CA bundle; enables **mTLS** (clients must present a cert signed by it). Client auth is the default: this flag, or `-no-auth`, must be given |
| `-no-auth` | off | Run **without** client certificate authentication. An explicit opt-out — the default refuses to start without `-client-ca`, so this is how you deliberately serve unauthenticated (development / testing) |
| `-proxy-protocol` | off | expect a PROXY protocol (v1/v2) header on each connection |
| `-proxy-from` | — | with `-proxy-protocol`: comma-separated proxy source IPs/CIDRs; any other source is rejected |
| `-host-key-checking` | `accept-new` | ssh `StrictHostKeyChecking`: `accept-new` (trust first contact, verify afterwards), `yes`, `no` (known_hosts only), or `off` |
| `-keepalive` | `15s` | keepalive window for sessions on the HTTP/1.1 leg: a silent session is probed with a ping after this much silence (0 disables). Keep it well under your reverse proxy's idle/read timeout. HTTP/2 sessions run with the window zero (h2 streams cannot enforce deadlines); liveness there is the transport's job (see [TRANSPORT.md](TRANSPORT.md#liveness)) |

The server binds to `127.0.0.1` by default. To use it from another
machine, run it behind an L4 (TCP) pass-through proxy (or listen on a
non-loopback addr); see [SECURITY.md](SECURITY.md#reverse-proxy-notes).

Note: virtweb re-executes itself once at startup to enable the Go
stdlib's HTTP/2 extended-CONNECT routing (`GODEBUG=http2xconnect=1`) —
the startup log line says so; it needs no operator action.

## Requirements

**To build:** the Go toolchain (any recent version; the module declares
its own minimum). That's it.

**To run in production (`-backend ssh`), on the host that runs virtweb
(or in its container):**
- Linux, with the OpenSSH client (`ssh`) in the default path. virtweb
  reaches each managed host by running `virsh` over SSH — it never opens
  a direct socket to a hypervisor and never links an SSH library.
  Default ssh options: `StrictHostKeyChecking=accept-new`,
  `ConnectTimeout=10`; `BatchMode=yes` unless the server has a
  password.
- **Credentials per server** — paste the server's SSH private key
  and/or set its password in the UI. They are stored with the server
  record (see below) and need nothing else on the virtweb host, which
  is why virtweb is happy to run in a container with only a data-dir
  volume. `sshpass` is only needed if any server uses password auth.
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
   - **SSH private key** *(optional)* — paste the key itself (OpenSSH
     or PKCS#8 PEM). It is stored with the server record in
     `servers.json` (file mode 0600); on each connection virtweb writes
     it to a 0600 temp file, offers it to ssh via `-i
     IdentitiesOnly=yes` (the virtweb host's own ssh agent and
     identities are excluded), and deletes the temp file afterwards.
   - **ssh password** *(optional)* — SSH password for this server.
     Requires `sshpass` where virtweb runs; stored in `servers.json`
     (0600) and handed to ssh through a 0600 temp file (never the
     process list). Key and password can be combined (key first,
     password as fallback).
3. **SSH access** must work as that user with the stored credentials:
   virtweb offers exactly the stored key (alone) and/or password for
   each server. Verify by hand that
   `ssh -p <port> <user>@<host> virsh -c qemu:///system list` works.
   New host keys are accepted automatically (`accept-new`). If no
   credentials are stored for a server, none are offered — that server
   is only reachable if the virtweb host's own ssh setup (agent,
   `~/.ssh/config`, default identities) gets it there.

The server list, its persisted `servers.json` (mode 0600), and each
server's shape (`name`, `user`, `host`, `port`, `uri`, and optionally
`key`, `password`) are what virtweb keeps — nothing else is stored.
`servers.json` holds the credentials, so keep the data dir private;
the API never returns the key material (only a `keySet` flag). The
server edit form does show the stored credentials (key and password,
pre-filled): edit them to replace, clear them to remove. A
server you can't reach simply shows an error on its page until the
SSH access is fixed.

## Managing VMs

Once a server is added, its page lists every domain with state, vCPUs,
memory, IP (from `domifaddr`), and — for a running domain — its live
display address. Click a VM's name for its **detail page**: live state,
resource info, display addresses, the usual actions, and the current
domain XML (`virsh dumpxml`), rendered as a collapsible tree inside a
scrollable, height-bounded box (the root element open, everything
else collapsed; the "Raw" toggle shows the byte-exact text, which is
what the copy button copies). Row actions on the
The server page's VM table is a *view*: state, resources, IP, and
links to open a VM's console / VNC / SPICE page — no actions in the
table. All actions live on the VM's detail page, where only the
transitions valid from the current state are offered: a shut-off VM
shows **Start** (unconfirmed); a running VM shows **Shutdown /
Destroy / Reboot** (each guarded by a confirmation dialog). On that
page the display links (Console/VNC/SPICE) sit at the top, just
under the info table — they are the frequent, safe path — while the
state control is a boxed panel at the very bottom, deliberately far
from them: the state actions are rare and one is destructive, so
distance (plus the confirms) keeps a habitual "open the console"
click away from "Destroy". The server is edited in place from
its page as well (host/port/user, name, SSH credentials). Defining
or undefining domains is not offered in the UI (virtweb manages
existing VMs); the REST API below still exposes it for automation.

### HTTP API

The UI is a thin form over a small JSON API, also usable directly
(every request needs the server's id in the URL — see the server store):

```
GET    /api/servers                         list servers
POST   /api/servers                         create server  {name, host, port, user, displays}
PUT    /api/servers/{id}                    update server in place
DELETE /api/servers/{id}                    delete server
GET    /api/servers/{id}/vms                list domains
POST   /api/servers/{id}/vms                define domain  {xml, start?}
DELETE /api/servers/{id}/vms/{name}         undefine domain
GET    /api/servers/{id}/vms/{name}/xml     current domain XML (dumpxml)
GET    /api/servers/{id}/vms/{name}/console/status   {"open":bool}
POST   /api/servers/{id}/vms/{name}/{start|shutdown|destroy|reboot}
GET    /api/servers/{id}/vms/{name}/tunnel/status    {"open":N} (all open tunnel sessions)

# Session endpoints (WebSocket; GET is the HTTP/1.1 Upgrade leg, CONNECT
# the HTTP/2 extended-CONNECT leg — the browser picks via ALPN). A HEAD
# to the same URL is the preflight: pre-upgrade status without opening
# a session (200 openable / 409 in use / 429 capped / 400, 503 display
# problems / 404 unknown server).
GET      /api/servers/{id}/vms/{name}/console/stream
GET|CONNECT  /api/servers/{id}/vms/{name}/tunnel/stream?display=vnc|spice
```

### Console / VNC / SPICE

All three pages share one layout: the remote display **never moves**.
The page is a single viewport — a header bar (title, live status,
Reconnect, **Fullscreen**, and on the SPICE page a **Log** toggle)
and one display area that fills the rest. The display is fitted to
the available space with CSS (aspect-preserving), Fullscreen pins it
to the screen (Esc leaves), the SPICE log is an overlay panel that
does not participate in layout, and the SPICE connect form is a
centered overlay card that disappears once the session is live.
Status, errors, logs, and the form can therefore never reposition
the remote display.

- **Console** — the serial console (`ssh -tt` → `virsh console`). The
  page reports a distinct status for every terminal state: *Connected*,
  *Session ended* (a clean 1000 close), *Connection lost* (with the
  server's reason when the session failed — the 1011 close reason),
  *Connection timed out*, *A session is already open*, *Could not
  connect* — and offers a **Reconnect** button once the session is no
  longer live. One console session per (server, VM); a second
  concurrent open is refused (409), and the VM detail page shows the
  console as *open* while one is live.
- **VNC** — noVNC over the SSH stdio TCP tunnel (`ssh -W`). The VNC
  port is read from the domain (auto-port resolved from the live XML
  while the domain is running).
- **SPICE** — the spice-html5 fork. Each SPICE channel opens its own
  concurrent tunnel session (one WebSocket each — the same topology as
  websockify's WebSocket-per-channel); when the main channel fails, the
  sibling channels are torn down with it (their tunnels close too). The
  display address comes from `virsh domdisplay` (SPICE ports are
  auto-assigned at boot and the display is often loopback-bound). Behind
  the mTLS reverse proxy the sessions ride HTTP/2 (one stream per
  session on the tab's h2 connection), so the browser's per-origin
  connection budget is not a constraint. The fork opens only the
  channels it implements (main, display, inputs, cursor, playback, port)
  and skips the rest (usbredir, record) — a domain with two `spicevmc`
  devices advertises eight channels, and the extras would both confuse
  the spice-server and hold ssh processes for nothing.

**Auto-retry.** A console/VNC/SPICE page whose session fails or is lost
reloads itself automatically — up to 5 times (the count survives the
reloads in `sessionStorage`, so a dead server stops the loop instead of
spinning). A *clean* session end is never retried: the session is over,
and re-attaching is the user's call (Reconnect).

Both display tunnels are **server-resolved**: the client names which
display to view (`?display=vnc|spice`) and virtweb resolves the
destination from the VM's live display info. No client-supplied
`host:port` is accepted on the tunnel endpoint — that would let an
authenticated user point the remote SSH user at an arbitrary TCP service
(SSRF).

All three use the same WebSocket session transport — see
[TRANSPORT.md](TRANSPORT.md) and `web/static/duplex.js` for the design
notes (the two legs, terminal close codes, liveness, teardown).

## Running in a container

The intended production shape is: virtweb in a container, behind the
mTLS reverse proxy (see SECURITY.md). Per-server credentials are
stored in the data dir rather than on the host, so the container only
needs a **data-dir volume** — no key files to mount.

```sh
docker build -t virtweb .      # golang multi-stage build -> debian:trixie + openssh-client + sshpass
docker run -d --name virtweb \
  -v $(pwd)/virtweb-data:/home/virtweb/.virtweb \
  -v $(pwd)/client-ca.pem:/home/virtweb/client-ca.pem \
  virtweb -addr 0.0.0.0:8080 -client-ca /home/virtweb/client-ca.pem
```

Flags for the proxy deployment: `-tls-cert -tls-key -client-ca`
(TLS/mTLS at virtweb — TLS is always served; `-client-ca` is required
unless you pass `-no-auth` to explicitly disable client auth),
`-proxy-protocol
-proxy-from <cidr>` (PROXY header from an L4 pass-through router).
`sshpass` in the image covers password auth; key auth needs nothing
extra.

## Offline demo mode (`-backend stub`)

`virtweb -backend stub -no-auth` runs against a deterministic in-process fake
libvirt host instead of a real SSH connection: a fixed VM table
(`debian-dev`, `win10`, …) with a real state machine (start →
*starting* → running, shutdown/destroy/reboot), virsh-shaped states, and
a scripted shell on the console (echo, `uname -a`, `hostname`,
`command not found`). Useful to explore the UI before a real host is
configured, and it is the backend the e2e suite's fast tier runs
against.

Failure injection for tests: `STUB_SCENARIO=fail-ping | slow-boot |
console-drop | tunnel-drop | ssh-down`.

## Layout

```
doc/                       documentation (this file + SECURITY/TRANSPORT/TESTS)
main.go                    entrypoint (embeds web/, wires store + API, flags)
internal/virt              Server/VM types + the Backend interface (the seam)
internal/sshexec           Runner interface + system-ssh implementation
internal/virsh             Backend impl: virsh over sshexec
internal/stub              deterministic stub Backend (-backend stub)
internal/store             JSON-file server store (atomic, concurrency-safe)
internal/api               REST + the WebSocket session endpoints
internal/ws                vendored single-file stdlib-only WebSocket
                               package (RFC 6455 + h2 extended CONNECT)
internal/webui             server-rendered HTML pages
web/                       embedded frontend (templates/ + static/)
third-party/               first-party forks: novnc, xtermjs,
                               spice-html5-fork (PROVENANCE.md each)
e2e/                       Playwright e2e suite (stub + libvirt tiers)
```

## Status

Feature-complete through the console, VNC, and SPICE transports (all
over WebSocket sessions: HTTP/2 extended CONNECT for browsers,
HTTP/1.1 Upgrade for tooling).

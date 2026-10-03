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
  - **VNC** (noVNC, unmodified) over the same transport
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

```sh
go build -o virtweb .
```

## Run

```sh
./virtweb [-addr 127.0.0.1:8080] [-data-dir ~/.virtweb] [-backend ssh|stub]
```

| flag | default | meaning |
|------|---------|---------|
| `-addr` | `127.0.0.1:8080` | address to listen on |
| `-data-dir` | `~/.virtweb` | where `servers.json` is persisted |
| `-backend` | `ssh` | `ssh` (production: virsh over SSH) or `stub` (offline demo/test) |

## Requirements

- Linux
- The OpenSSH client (`ssh`) in the default path — virtweb runs `virsh`
  on the remote host over SSH (`BatchMode=yes`,
  `StrictHostKeyChecking=accept-new`, `ConnectTimeout=10`).
- SSH access to each managed host. **Key-based auth is expected;
  interactive passwords are not supported.**

## Using it

1. Start the binary and open the URL (default `http://127.0.0.1:8080`).
2. **Add a server**: name, user, host, optional port, and the libvirt
   URI (default `qemu:///system` — the *system* instance, where VMs live,
   as opposed to `qemu:///session`).
3. On the server page, the VM table lists each domain with state, vCPUs,
   memory, IP (from `domifaddr`), and — for a running domain — its live
   display address. Use the row actions (Start / Shutdown / Reboot /
   Destroy) or open a VM's console/VNC/SPICE page.

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
  often loopback-bound, so the page takes `host:port`).

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
third-party/               vendored: novnc, xtermjs, spice-html5-fork
e2e/                       Playwright e2e suite (stub + libvirt tiers)
```

## Status

Feature-complete through the console, VNC, and SPICE transports (all over
plain-HTTP streams, no WebSocket). See `ROADMAP.md`/`TODO.md` in the
parent directory for the polish roadmap.

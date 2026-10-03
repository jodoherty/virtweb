# virtweb

A web-based management dashboard for remote libvirt (KVM/QEMU) hosts.

> **Status:** M1–M4 done (server management, VM list, VM actions, serial
> console over plain HTTP streams — chunked GET `ReadableStream` + POST
> input, no WebSocket). VNC viewer (M6) not started.
> See `TODO.md`/`ROADMAP.md` in the parent directory for exact state.

- Add/remove libvirt servers (reached over SSH using the system `ssh` client)
- View and manage virtual machines (start, shutdown, reboot, destroy)
- Use virtual machines: serial console (HTTP stream → `ssh -tt` → `virsh console`)
  and VNC (SSH stdio tunnel + in-browser RFB client)

## Tech stack

- **Backend:** Go, standard library only
- **Frontend:** vanilla JavaScript, no frameworks, no build step
- **Deployment:** single binary — the web frontend is embedded with `go:embed`

## Build

```sh
go build -o virtweb .
```

## Run

```sh
./virtweb [-addr 127.0.0.1:8080] [-data-dir ~/.virtweb]
```

- `-addr` — address to listen on (default `127.0.0.1:8080`)
- `-data-dir` — where server definitions are persisted (default `~/.virtweb`)

## Requirements

- Linux
- OpenSSH client (`ssh`) in the default path — servers are managed by running
  `virsh` on the remote host over SSH
- SSH access to each managed host (key-based auth is expected; passwords are
  not supported)

## Tests

```sh
go test ./...          # unit tests (no libvirtd needed, everything is shimmed)
go test -tags integration ./itest/   # integration tests (uses qemu-system)
```

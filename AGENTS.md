# AGENTS.md

Instructions for AI coding agents (and humans) working in this
repository.

## README.md is owner-maintained

`README.md` is reserved for the project owner's hand-written editing. Do
not create, edit, or reformat it — not for "improvements", not for sync
with the code, not ever. User-facing documentation lives in `doc/`
instead; when the README's former content changes, update the `doc/`
files, never the README:

- `doc/USAGE.md` — what it is, building, running and configuring,
  managing VMs, the HTTP API, console/VNC/SPICE, offline demo mode
- `doc/SECURITY.md` — the security model, TLS/mTLS and the PROXY
  protocol, reverse-proxy deployment notes
- `doc/TRANSPORT.md` — the framed stream transport (wire format v1 +
  v2) and why it exists
- `doc/TESTS.md` — the test evidence and the release gate

## What this repository is

virtweb is a web-based management dashboard for remote libvirt
(KVM/QEMU) hosts, reached over the system `ssh` client (virsh over SSH;
never a linked SSH library, never a direct hypervisor socket).

Hard constraints (keep every change compliant):

- **Backend:** Go, standard library only. No third-party dependencies,
  no cgo.
- **Frontend:** vanilla JavaScript, no frameworks, no build step, no npm
  packages outside `e2e/`.
- **Single embedded binary:** the web frontend is baked in via
  `go:embed` (`web/`).
- **No WebSocket:** the console/VNC/SPICE transport is plain-HTTP
  streams — a chunked, flushed GET (browser `ReadableStream`) plus
  small, ordered POSTs — speaking the framed wire protocol documented
  in `doc/TRANSPORT.md`.
- **SSH layer:** always exec the system `ssh` (OpenSSH ≥ 9.x).
- **`e2e/`** is a self-contained Node/Playwright project; nodejs
  artifacts stay inside it. Its own rules live in `e2e/AGENTS.md`.

Architecture package map, environment facts, workflow, and the broader
project context (remote-host details, milestones) are documented in the
parent directory's `AGENTS.md` and `TESTING.md` (outside this git
repository; the host environment may not have them — treat them as
absent and rely on this file plus `doc/` and `e2e/AGENTS.md`).

## Definition of done

`go build ./...`, `go vet ./...`, and `go test ./...` all pass; the
e2e tiers relevant to the change are green (`make check` covers the
fast path; `make release` is the pre-release gate — see
`doc/TESTS.md`).

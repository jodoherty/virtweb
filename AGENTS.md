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
- `doc/TRANSPORT.md` — the WebSocket session transport (the two legs,
  terminal close codes, keepalives/liveness) and why it exists
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
- **Transport:** the console/VNC/SPICE streams are WebSocket sessions
  (RFC 6455), one per logical session — no app-level multiplexing,
  no custom framing. The server speaks them over the vendored, stdlib-only
  `internal/ws` package (a single-file drop-in, see its license); the
  browser drives them with the native `WebSocket`. HTTP/2 extended
  CONNECT (RFC 8441) is the primary leg, the HTTP/1.1 Upgrade the
  fallback. Details: `doc/TRANSPORT.md`.
- **TLS is always on.** virtweb serves TLS unconditionally (the
  WebSocket transport requires it; browsers negotiate h2 over ALPN on
  TLS listeners only). When no certificate is supplied it generates an
  ephemeral self-signed dev certificate (loud warning) — production
  always supplies real certificates.
- **SSH layer:** always exec the system `ssh` (OpenSSH ≥ 9.x).
- **`e2e/`** is a self-contained Node/Playwright project; nodejs
  artifacts stay inside it. Its own rules live in `e2e/AGENTS.md`.

The e2e suite's design rationale (tiers, the persistent environment,
the reverse-proxy axis) is documented in the parent directory's
`TESTING.md` (outside this git repository; the host environment may not
have it — treat it as absent and rely on this file, `doc/`, and
`e2e/AGENTS.md`).

## Definition of done

`go build ./...`, `go vet ./...`, and `go test ./...` all pass; the
e2e tiers relevant to the change are green (`make check` covers the
fast path; `make release` is the pre-release gate — see
`doc/TESTS.md`).

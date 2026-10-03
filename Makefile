# virtweb — build, test, release gate
#
# virtweb is a single, dependency-free Go binary (the web frontend is
# embedded), so building needs nothing but the Go toolchain. There is no
# cgo, so cross-compilation is a plain env-var thing.
#
# Release gate (see README "Releasing"):
#   make check    — the always-run gate: vet + unit tests + the fast
#                   (stub) e2e tier.
#   make release  — the before-release gate: everything in `check` plus
#                   the slow, real (libvirt container) e2e tier.

BINARY  := virtweb
DIST    := dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w

.PHONY: all build dist test vet e2e-stub e2e-libvirt check release clean

# Default: build the binary for the host you're on.
all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

# A static, relocatable build for a Linux x86-64 deploy target,
# regardless of what you're building on (macOS/Windows/etc).
dist:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS) -X main.version=$(VERSION)" \
		-o $(DIST)/$(BINARY)-linux-amd64 .

test:
	go test ./...

vet:
	go vet ./...

# e2e tiers (Node/Playwright, self-contained in e2e/; see e2e/AGENTS.md).
# `env:up` is idempotent; the stub tier is fast/offline, the libvirt tier
# spins up a rootless-podman libvirt host (needs podman + gcc) and is slow.
e2e-stub:
	cd e2e && npm run env:up -- --backend stub && npx playwright test --project=stub

e2e-libvirt:
	cd e2e && npm run env:up -- --backend libvirt && npx playwright test --project=libvirt

# Always-run gate: static checks, unit tests, fast e2e tier.
check: vet test e2e-stub

# Before-release gate: `check` plus the real libvirt tier. Stub-green is
# NOT release-green — the real tier must actually run.
release: check e2e-libvirt

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

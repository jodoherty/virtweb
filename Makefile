# virtweb — build + test
#
# virtweb is a single, dependency-free Go binary (the web frontend is
# embedded), so building needs nothing but the Go toolchain. There is no
# cgo, so cross-compilation is a plain env-var thing.

BINARY  := virtweb
DIST    := dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w

.PHONY: all build dist test vet clean e2e

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

# The e2e suite (Node/Playwright) lives in e2e/; see e2e/AGENTS.md.
e2e:
	cd e2e && npm run env:up -- --backend stub && npx playwright test --project=stub

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

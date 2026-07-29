.PHONY: build test install install-local clean

# Version is injected at build time via -ldflags. Override with VERSION=x,
# otherwise derive from git (tag or commit), falling back to "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/pm ./cmd/pm

test:
	go test -race ./...

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/pm

# install-local builds pm and copies it to ~/bin/pm, the binary currently on
# PATH and run by the daemon. Prefer this over install (go install -> GOBIN).
# Replace the binary atomically so a running daemon keeps its old inode while
# new invocations use a complete binary with a valid macOS code-signing cache.
install-local: build
	@tmp=$$(mktemp "$(HOME)/bin/.pm.XXXXXX"); \
	trap 'rm -f "$$tmp"' EXIT; \
	cp bin/pm "$$tmp"; \
	chmod 755 "$$tmp"; \
	mv -f "$$tmp" "$(HOME)/bin/pm"; \
	trap - EXIT

clean:
	rm -rf bin

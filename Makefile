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
# Copying over a running binary is safe: the daemon keeps the old code in
# memory until you restart it with `pm down && pm up -d`.
install-local: build
	cp bin/pm $(HOME)/bin/pm

clean:
	rm -rf bin

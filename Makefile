.PHONY: build test install clean

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

clean:
	rm -rf bin

.PHONY: build test install clean

build:
	go build -o bin/pm ./cmd/pm

test:
	go test -race ./...

install:
	go install ./cmd/pm

clean:
	rm -rf bin

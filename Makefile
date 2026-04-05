VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test clean

build:
	CGO_ENABLED=1 go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

test:
	CGO_ENABLED=1 go test -race -cover ./...

clean:
	rm -rf bin/

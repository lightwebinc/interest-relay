BIN      := interest-relay
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.Version=$(VERSION)

.PHONY: all build tools test test-netns lint fmt tidy clean help

all: build tools

build:                 ## build the relay daemon into build/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o build/$(BIN) .

tools:                 ## build the test tools (announcer, terminus, lane feed and sink) into build/
	CGO_ENABLED=0 go build -trimpath -o build/ ./cmd/...

test:                  ## unit and loopback tests, with the race detector
	go test -race -count=1 $(PKG)

test-netns:            ## multi-hop and distribution tests in network namespaces (needs user namespaces or root)
	test/netns/chain.sh
	test/netns/distribute.sh

lint:                  ## golangci-lint
	golangci-lint run $(PKG)

fmt:                   ## gofmt -w .
	gofmt -w .

tidy:                  ## go mod tidy
	go mod tidy

clean:
	rm -rf build

help:                  ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/'

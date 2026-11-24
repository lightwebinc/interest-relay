BIN      := interest-relay
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE    ?= ghcr.io/lightwebinc/$(BIN)
LDFLAGS  := -s -w -X main.Version=$(VERSION)

# Every target runs with the Go workspace off, so a local build resolves
# sibling modules from go.mod, as CI and the Dockerfile do.
export GOWORK = off

.PHONY: all build tools test test-netns verify fmt-check vet licences licences-update figures image lint fmt tidy clean help

all: build tools

build:                 ## build the relay daemon into build/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o build/$(BIN) .

tools:                 ## build the test tools (announcer, terminus, lane feed and sink) into build/
	CGO_ENABLED=0 go build -trimpath -o build/ ./cmd/...

test:                  ## unit and loopback tests, with the race detector
	go test -race -count=1 $(PKG)

test-netns:            ## multi-hop, distribution and publishing tests in network namespaces (needs user namespaces or root)
	test/netns/chain.sh
	test/netns/distribute.sh
	test/netns/publish.sh

verify: fmt-check vet test licences   ## what CI runs: formatting, vet, tests under -race, license freshness

fmt-check:             ## fail if anything is unformatted
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

vet:                   ## go vet ./...
	go vet $(PKG)

licences:              ## fail if LICENSE-THIRD-PARTY is stale
	python3 scripts/gen-third-party-licenses.py . --check

licences-update:       ## regenerate LICENSE-THIRD-PARTY from what the binaries link
	python3 scripts/gen-third-party-licenses.py .

figures:               ## regenerate docs/figures/*.svg (needs Pillow and Liberation Sans)
	python3 docs/figures/figs.py

image:                 ## build the container image locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

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

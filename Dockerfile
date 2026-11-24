# syntax=docker/dockerfile:1.29.0@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
#
# Multi-stage Dockerfile for interest-relay. Produces static binaries in
# /usr/local/bin on a distroless nonroot base: the daemon, and
# interest-announce to ask a relay by hand.
#
# The builder is pinned by digest and is a LATER toolchain than go.mod's floor.
# The floor is held project-wide on purpose, while the stdlib actually shipped
# comes from here, so scanning sees what we ship rather than what we compile
# against.
#
# No ENV defaults are baked in: pass flags as the container command, or set
# the environment variables docs/configuration.md lists.

FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    mkdir -p /out; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X main.Version=${VERSION}" \
        -o /out/interest-relay .; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -buildvcs=false -ldflags "-s -w" \
        -o /out/interest-announce ./cmd/interest-announce

FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
USER nonroot:nonroot
COPY --from=builder /out/ /usr/local/bin/

# The licenses travel with the image: a static binary contains every module
# it links, and a published image is a copy of each of them.
COPY LICENSE NOTICE LICENSE-THIRD-PARTY /usr/share/doc/interest-relay/
# Interest 8729 (UDP), then the metrics and health listener 9600, which also
# serves /healthz, /readyz and /state. The lane and publish listeners are off
# unless configured, on ports the operator chooses.
EXPOSE 8729/udp 9600
ENTRYPOINT ["/usr/local/bin/interest-relay"]

# popfleet agent image: static Go binary on busybox:musl (lean, per the RDD).
# Run: podman run -d -e POPFLEET_URL=... -e POPFLEET_TOKEN=... ghcr.io/ejoliet/popfleet-agent
# --platform=$BUILDPLATFORM keeps the build stage native and cross-compiles via
# TARGETOS/TARGETARCH. Without it, buildx runs the whole Go toolchain under QEMU
# for the arm64 leg of the multi-arch build — minutes instead of seconds.
# Both args are supplied by buildx; the defaults keep a plain `podman build` working.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION is stamped by the release workflow; local builds report "dev".
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/ejoliet/popfleet/internal/agent.Version=$VERSION" -o /popfleet .

# busybox:musl (~4 MB), not scratch. The RDD's resolved open question picked
# "lean scratch/Go" over python-slim, but scratch has no shell at all, so a
# terminal into the container — the entire point of the product — returns 127.
# busybox keeps the image lean and static while making Gate 1 actually demoable.
# Swap to FROM scratch if you only ever want enroll/heartbeat/drop-off.
FROM docker.io/library/busybox:musl
COPY --from=build /popfleet /popfleet
ENV SHELL=/bin/sh
ENTRYPOINT ["/popfleet", "agent"]

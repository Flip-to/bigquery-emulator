ARG DEBIAN_VERSION=bookworm

FROM --platform=$BUILDPLATFORM golang:${DEBIAN_VERSION} AS builder

# Provided by buildx for every target platform.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build
# Copy the dependency manifests first to leverage the Docker layer cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . ./

ARG VERSION
ARG REVISION

# The SQL backend is pure Go; build a fully static binary without cgo.
# The builder stage runs on the native build platform and cross-compiles
# for the requested target, so multi-arch image builds need no QEMU.
# Cache mounts keep the module and build caches across builds; CI persists
# them between runs with buildkit-cache-dance (see flipto-image.yml).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /go/bin/bigquery-emulator \
    -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    ./cmd/bigquery-emulator

FROM debian:${DEBIAN_VERSION}

COPY --from=builder /go/bin/bigquery-emulator /bin/bigquery-emulator

WORKDIR /work

ENTRYPOINT ["/bin/bigquery-emulator"]

# --- Builder stage (Debian bookworm) ---
FROM golang:1.25-bookworm AS builder

# Build dependencies. No libolm: the build uses the `goolm` tag, mautrix-go's
# pure-Go Olm implementation, so the deprecated C library is neither compiled
# against nor shipped. build-essential stays because go-sqlite3 is cgo.
RUN apt-get update -y \
    && apt-get install -y --no-install-recommends git ca-certificates build-essential \
    && rm -rf /var/lib/apt/lists/*

# TARGETARCH is provided by buildx for multi-platform builds. We use it to
# scope the Go build cache per-arch so cross-arch builds (linux/amd64 +
# linux/arm64 in release.yml) don't corrupt each other's compile artifacts.
# Single-arch builds (docker.yml) just get a stable, arch-specific cache key.
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
# BuildKit cache mount on the module cache: persists across builds via the
# GHA cache (cache-to: type=gha,mode=max in the workflows). go.mod/go.sum
# changes still bust the layer because the COPY above is what triggers
# re-execution; the mount only kicks in when this RUN actually runs.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# CGO stays enabled for go-sqlite3, which is what the default database needs;
# the `goolm` tag is what removes the libolm dependency. Cache mounts:
#   - /root/.cache/go-build: Go's compile cache; arch-scoped because compiled
#     object files are platform-specific (sharing across arm64+amd64 corrupts).
#   - /go/pkg/mod: module source cache; arch-agnostic.
# These persist across builds (combined with cache-to: type=gha,mode=max),
# turning cold ~5-8 min builds into ~30-90s incremental ones.
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 go build -tags goolm -o matrimail ./cmd/matrimail

# Prepare a data directory we can chown in final image via COPY --chown
RUN mkdir -p /runtime-data

# --- Runtime dependencies stage (Debian bookworm-slim) ---
# Stages certificates and timezone data into a known prefix, then COPYs that
# prefix into the distroless final stage. This used to also carry libolm, and
# had to hunt for it under an arch-specific multiarch path; the `goolm` build
# removed that.
FROM debian:bookworm-slim AS runtime-deps
RUN apt-get update -y \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /matrimail-runtime/etc/ssl/certs /matrimail-runtime/usr/share \
    && cp /etc/ssl/certs/ca-certificates.crt /matrimail-runtime/etc/ssl/certs/ca-certificates.crt \
    && cp -r /usr/share/zoneinfo /matrimail-runtime/usr/share/zoneinfo

# --- Final minimal runtime (Distroless) ---
# Distroless base matching Debian 12
FROM gcr.io/distroless/cc-debian12:nonroot

# Copy the compiled binary
COPY --from=builder /build/matrimail /usr/bin/matrimail
# Certificates and timezone data. The base image supplies the C runtime that
# the cgo SQLite driver needs; nothing else has to be carried over.
COPY --from=runtime-deps /matrimail-runtime/etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=runtime-deps /matrimail-runtime/usr/share/zoneinfo /usr/share/zoneinfo

# Use a path owned by the nonroot user in distroless
WORKDIR /home/nonroot/app
# Ensure a writable data directory owned by the nonroot user exists
COPY --from=builder --chown=nonroot:nonroot /runtime-data /home/nonroot/app/data
# Expose a writable volume for data (mount a host volume here)
VOLUME ["/home/nonroot/app/data"]

EXPOSE 29319
ENTRYPOINT ["/usr/bin/matrimail"]

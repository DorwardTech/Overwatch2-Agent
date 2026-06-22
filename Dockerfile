# Overwatch site agent — static binary in a distroless, non-root image.
# Build context is THIS directory (the agent repo root).
#
# Multi-arch: the build stage is pinned to the native build platform ($BUILDPLATFORM)
# and Go cross-compiles to the requested target arch (TARGETARCH/TARGETVARIANT,
# injected by buildx). amd64/arm64/armv7 images therefore build without QEMU
# emulation, and the final distroless stage only copies the static binary.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG AGENT_VERSION=dev
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GOARM is derived from the buildx variant (arm/v7 -> 7); it is ignored for
# non-ARM and arm64 targets.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    GOARM=$(printf '%s' "${TARGETVARIANT}" | tr -d 'v') \
    go build -trimpath \
    -ldflags="-s -w -X overwatch/agent/internal/version.Value=${AGENT_VERSION}" \
    -o /out/agent ./cmd/agent
# Stage an empty cache dir so the final image can own it as the non-root user.
RUN mkdir -p /cache

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
# Pre-create the cache mountpoint owned by the non-root runtime user (65532) so a
# freshly-created CACHE_DIR volume inherits writable ownership (Docker copies the
# mountpoint's ownership to a new, empty volume). For an EXISTING volume or a bind
# mount, chown it to 65532:65532 — see the README.
COPY --from=build --chown=65532:65532 /cache /data/cache
USER nonroot:nonroot
EXPOSE 8088
ENTRYPOINT ["/agent"]

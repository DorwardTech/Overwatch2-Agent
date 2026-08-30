# Overwatch site agent — static binary in a distroless, non-root image.
# Build context is THIS directory (the agent repo root).
# Multi-arch: pin the build stage to the native build platform and let Go
# cross-compile to the buildx target arch (TARGETARCH/TARGETVARIANT), so
# amd64/arm64/armv7 all build without QEMU — matches docker-image.yml, which
# sets up buildx only (no setup-qemu).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
# Default to the current release so an un-tagged build (e.g. main -> :latest)
# still reports a real version, not "dev". Tagged releases override this via a
# build-arg (see docker-image.yml). Keep in sync with internal/version.Value.
ARG AGENT_VERSION=1.1.2
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GOARM is derived from the buildx variant (arm/v7 -> 7); ignored for non-ARM.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    GOARM=$(printf '%s' "${TARGETVARIANT}" | tr -d 'v') go build -trimpath \
    -ldflags="-s -w -X overwatch/agent/internal/version.Value=${AGENT_VERSION}" \
    -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
USER nonroot:nonroot
EXPOSE 8088
ENTRYPOINT ["/agent"]

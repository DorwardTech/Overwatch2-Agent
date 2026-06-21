# Overwatch site agent — static binary in a distroless, non-root image.
# Build context is THIS directory (the agent repo root).
FROM golang:1.24-alpine AS build
ARG AGENT_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
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

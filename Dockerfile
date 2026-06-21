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

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
USER nonroot:nonroot
EXPOSE 8088
ENTRYPOINT ["/agent"]

# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
# CGO is off: SQLite + sqlite-vec run as a wasm module inside the Go binary,
# so the result is a single static executable.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ragmux ./cmd/ragmux

# Pre-create the data directory with the runtime user's ownership so a fresh
# named volume is writable without any chown at start.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---- runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ENV DATA_DIR=/app/data \
    PORT=8080

COPY --from=build /out/ragmux /app/ragmux
COPY --from=build --chown=65532:65532 /out/data /app/data

WORKDIR /app
VOLUME ["/app/data"]
EXPOSE 8080
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/ragmux", "-healthcheck"]

ENTRYPOINT ["/app/ragmux"]

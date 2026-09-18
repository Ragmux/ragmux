# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
# Pure Go (pgx, no cgo): the result is a single static executable.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ragmux ./cmd/ragmux

# DATA_DIR only backs the secret.key fallback used when SECRET_KEY is unset;
# pre-create it writable for the runtime user so that fallback works.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---- runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ENV DATA_DIR=/app/data \
    PORT=8765

COPY --from=build /out/ragmux /app/ragmux
COPY --from=build --chown=65532:65532 /out/data /app/data

WORKDIR /app
EXPOSE 8765
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/ragmux", "-healthcheck"]

ENTRYPOINT ["/app/ragmux"]

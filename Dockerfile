# Builds ruler from this checkout. The release image is built by goreleaser
# from an already-compiled binary; see Dockerfile.goreleaser.
FROM golang:1.26.8-alpine3.24 AS build

# Version is the one symbol worth stamping. The commit and build time normally
# come from the toolchain's vcs record, which a docker build cannot produce:
# the build context carries no .git, so `ruler version` reports an unknown
# commit in an image built this way. A release image is built from a binary
# compiled inside the checkout and does carry a real commit.
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/dennisme/clickhouse-ruler/internal/buildinfo.Version=${VERSION}" \
    -o /out/ruler ./cmd/ruler

FROM alpine:3.24
# ClickHouse and Alertmanager are both reachable over TLS, and alpine ships no
# root certificates, so a verified connection fails without this.
RUN apk add --no-cache ca-certificates \
    && addgroup -S ruler \
    && adduser -S -G ruler ruler
COPY --from=build /out/ruler /usr/local/bin/ruler
USER ruler
ENTRYPOINT ["/usr/local/bin/ruler"]

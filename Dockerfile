# syntax=docker/dockerfile:1

# Cross-compile on the build host's architecture; the binary is pure Go
# (CGO-free SQLite), so no emulation is needed for the arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/tendarr ./cmd/tendarr \
 && mkdir -p /out/data

# distroless: no shell, no package manager. It starts as root only so the
# binary can hand /data to PUID:PGID; it then drops to that user before
# opening the database or the port (see cmd/tendarr/privileges_linux.go).
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2

COPY --from=build /out/tendarr /usr/local/bin/tendarr
COPY --from=build --chown=65532:65532 /out/data /data

LABEL org.opencontainers.image.licenses="MIT"

ENV TENDARR_DATA_DIR=/data \
    TENDARR_LISTEN_ADDR=:8484

VOLUME /data
EXPOSE 8484

HEALTHCHECK --interval=60s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/tendarr", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tendarr"]

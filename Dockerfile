# syntax=docker/dockerfile:1

# Cross-compile on the build host's architecture; the binary is pure Go
# (CGO-free SQLite), so no emulation is needed for the arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build

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

# distroless: no shell, no package manager, runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/tendarr /usr/local/bin/tendarr
COPY --from=build --chown=65532:65532 /out/data /data

LABEL org.opencontainers.image.licenses="MIT"

ENV TENDARR_DATA_DIR=/data \
    TENDARR_LISTEN_ADDR=:8484

VOLUME /data
EXPOSE 8484
USER 65532:65532

HEALTHCHECK --interval=60s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/tendarr", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tendarr"]

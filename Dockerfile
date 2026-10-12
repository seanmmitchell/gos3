# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# gos3 container image: the web UI is built with pnpm, embedded into a static
# Go binary, and shipped on distroless (non-root, no shell).
#
#   docker build -t gos3 .
#   docker build -t gos3 --build-arg VERSION=v1.2.3 --build-arg COMMIT=$(git rev-parse --short=12 HEAD) .
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/OWNER/gos3 .
#   docker run -p 9000:9000 -v gos3-data:/data gos3
#
# `make docker` passes VERSION, COMMIT and DATE. The build stages run on the
# build platform and cross-compile, so multi-arch builds need no emulation.
#
# Base images are pinned by digest (multi-arch index) so a retagged image
# cannot change a release build; Dependabot (.github/dependabot.yml) proposes
# tag and digest updates. To build on another Go or Node version, edit the tag
# and the digest of the FROM line together.

# ---------------------------------------------------------------------------
# Web UI: static SPA with base /_gos3/ui/ (web/.output/public)
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:25-bookworm-slim@sha256:81db02c4b671288a03915da9534dbd54f96d0e7c24d80ccc54f5b36b2e684370 AS ui
ENV PNPM_HOME=/pnpm \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    NUXT_TELEMETRY_DISABLED=1 \
    CI=true
ENV PATH=$PNPM_HOME:$PATH
RUN corepack enable
WORKDIR /src/web
# Dependencies first (cached until the lockfile changes). `pnpm fetch` only
# needs the lockfile; the offline install below links from the store.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm fetch --frozen-lockfile
COPY web/ ./
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile --offline && \
    pnpm generate:embed && \
    test -f .output/public/index.html -o -f .output/public/200.html

# ---------------------------------------------------------------------------
# Go binary with the UI embedded (internal/webui/dist)
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# Same steps as `make ui`: replace the placeholder with the UI build and make
# sure index.html exists (the SPA build may only produce 200.html).
COPY --from=ui /src/web/.output/public/ /tmp/ui/
RUN find internal/webui/dist -mindepth 1 -maxdepth 1 ! -name .keep -exec rm -rf {} + && \
    cp -R /tmp/ui/. internal/webui/dist/ && \
    { test -f internal/webui/dist/index.html || cp internal/webui/dist/200.html internal/webui/dist/index.html; }
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/seanmmitchell/gos3/internal/version.Version=${VERSION} \
        -X github.com/seanmmitchell/gos3/internal/version.Commit=${COMMIT} \
        -X github.com/seanmmitchell/gos3/internal/version.Date=${DATE}" \
      -o /out/gos3 ./cmd/gos3 && \
    mkdir -p /out/data

# ---------------------------------------------------------------------------
# Runtime: distroless static, uid/gid 65532 (nonroot)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="gos3" \
      org.opencontainers.image.description="S3-compatible object storage server with an embedded web UI" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/seanmmitchell/gos3"
COPY --from=build /out/gos3 /usr/local/bin/gos3
COPY --from=build --chown=65532:65532 /out/data /data
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/doc/gos3/
# Every config key has a GOS3_* variable (`gos3 config env`). Root keys:
# set GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY, or let gos3 generate a
# pair on first start (printed in the log, stored in /data/.gos3).
ENV GOS3_ADDRESS=:9000 \
    GOS3_DATA=/data
USER 65532:65532
VOLUME ["/data"]
# 9000/tcp: S3 API, admin API, web UI, metrics. 7373/udp: discovery beacon;
# 5353/udp: mDNS (both multicast, only useful on a shared L2 network).
EXPOSE 9000/tcp 7373/udp 5353/udp
# `gos3 healthcheck` derives the URL from GOS3_* variables (and GOS3_CONFIG),
# not from the flags given to `serve`: set the listen address and TLS
# through the environment (GOS3_ADDRESS, GOS3_TLS_CERT, GOS3_TLS_KEY).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/gos3", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/gos3"]
CMD ["serve"]

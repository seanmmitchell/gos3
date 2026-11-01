# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# gos3 container image: the web UI is built with pnpm, embedded into a static
# Go binary, and shipped on distroless (non-root, no shell) together with the
# restic executable that restic-encrypted buckets use (docs/encryption.md).
# Select --target headless to omit UI assets and the Node/pnpm build entirely.
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
# and the digest of the FROM line together (the build stage checks that the
# installed Go is at least go.mod's go line).

# ---------------------------------------------------------------------------
# Web UI: static SPA with base /_gos3/ui/ (web/.output/public)
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:25-bookworm-slim@sha256:81db02c4b671288a03915da9534dbd54f96d0e7c24d80ccc54f5b36b2e684370 AS ui
ENV PNPM_HOME=/pnpm \
    NUXT_TELEMETRY_DISABLED=1 \
    CI=true
ENV PATH=$PNPM_HOME:$PATH
WORKDIR /src/web
# Dependencies first (cached until the lockfile changes). `pnpm fetch` only
# needs the lockfile; the offline install below links from the store. Only
# production dependencies are installed: `nuxt generate` needs none of the
# devDependencies (test tools), so their code never runs in the image build.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
# Install the project's pinned package manager directly for repeatable builds.
RUN npm install --global --ignore-scripts "$(node -p "require('./package.json').packageManager")"
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm fetch --frozen-lockfile
COPY web/ ./
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile --offline --prod && \
    pnpm generate:embed && \
    test -f .output/public/index.html -o -f .output/public/200.html

# ---------------------------------------------------------------------------
# Go binary with the UI embedded (internal/webui/dist)
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS go-base
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
WORKDIR /src
COPY go.mod go.sum ./
# The golang image sets GOTOOLCHAIN=local. Fail if the installed toolchain is
# older than go.mod's minimum, so a future Go bump cannot silently ship an
# older standard library. A newer image (a Dependabot update) passes.
RUN want="$(tr -d '\r' <go.mod | sed -n 's/^go //p')"; \
    have="$(cd / && go env GOVERSION)"; have="${have#go}"; \
    if [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n 1)" != "$want" ]; then \
      echo "build toolchain has go$have but go.mod requires go >= $want: update the pinned golang FROM line" >&2; \
      exit 1; \
    fi
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/

# This target has no dependency on the Node/pnpm stage or UI assets.
FROM go-base AS build-headless
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -tags noui -trimpath \
      -ldflags "-s -w \
        -X github.com/seanmmitchell/gos3/internal/version.Version=${VERSION} \
        -X github.com/seanmmitchell/gos3/internal/version.Commit=${COMMIT} \
        -X github.com/seanmmitchell/gos3/internal/version.Date=${DATE}" \
      -o /out/gos3 ./cmd/gos3 && \
    mkdir -p /out/data

FROM go-base AS build
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
# restic for the target architecture: the official release executable
# (static Go binary), verified against the SHA-256 sums published in the
# release's SHA256SUMS (signed by the restic maintainers). To update restic,
# change RESTIC_VERSION and both sums together, the same pins in
# .github/actions/setup-restic, and the license URL in internal/tools/notices
# (whose tests check that it names this version). The golang image is reused
# for curl and to decompress the .bz2 with Go's compress/bzip2.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS restic
ARG TARGETOS
ARG TARGETARCH
ARG RESTIC_VERSION=0.19.1
RUN --mount=type=cache,target=/root/.cache/go-build <<'EOF'
set -eu
case "$TARGETOS/$TARGETARCH" in
  linux/amd64) sum=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c ;;
  linux/arm64) sum=a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465 ;;
  *) echo "no pinned restic $RESTIC_VERSION checksum for $TARGETOS/$TARGETARCH: add it to the Dockerfile" >&2; exit 1 ;;
esac
asset="restic_${RESTIC_VERSION}_${TARGETOS}_${TARGETARCH}.bz2"
curl -fsSL --retry 3 -o "/tmp/$asset" "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/$asset"
echo "$sum  /tmp/$asset" | sha256sum -c -
cat >/tmp/bunzip2.go <<'GO'
// bunzip2 copies the bzip2-decompressed standard input to standard output.
package main

import (
	"compress/bzip2"
	"io"
	"log"
	"os"
)

func main() {
	if _, err := io.Copy(os.Stdout, bzip2.NewReader(os.Stdin)); err != nil {
		log.Fatalf("bunzip2: %v", err)
	}
}
GO
mkdir -p /out
go run /tmp/bunzip2.go <"/tmp/$asset" >/out/restic
chmod 0755 /out/restic
test -s /out/restic
EOF

# ---------------------------------------------------------------------------
# Runtime: distroless static, uid/gid 65532 (nonroot)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
ARG VERSION=dev
LABEL org.opencontainers.image.title="gos3" \
      org.opencontainers.image.description="S3-compatible object storage server with an embedded web UI" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/seanmmitchell/gos3"
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/doc/gos3/
# restic's license is in THIRD_PARTY_NOTICES (artifact gos3-image). gos3
# finds it on PATH and runs it only for buckets whose encryption provider is
# restic.
COPY --from=restic /out/restic /usr/local/bin/restic
# Every config key has a GOS3_* variable (`gos3 config env`). Root keys:
# set GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY, or let gos3 generate a
# pair on first start (stored in /data/.gos3/root-credentials.json).
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

# docker build --target headless -t gos3:headless .
# Management API is enabled; use --no-admin for S3-only operation.
FROM runtime AS headless
LABEL org.opencontainers.image.description="S3-compatible object storage server without embedded web UI assets"
COPY --from=build-headless /out/gos3 /usr/local/bin/gos3
COPY --from=build-headless --chown=65532:65532 /out/data /data

# The default image continues to include and enable the embedded UI.
FROM runtime AS embedded
COPY --from=build /out/gos3 /usr/local/bin/gos3
COPY --from=build --chown=65532:65532 /out/data /data

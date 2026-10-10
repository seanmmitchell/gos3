# syntax=docker/dockerfile:1
#
# Standalone gos3 web UI (Nuxt node-server build). It manages any number of
# gos3 / S3 servers and adds local discovery routes (mDNS, beacon, scan) that
# run on the UI host. Build from the repository root:
#
#   docker build -f deploy/docker/web.Dockerfile -t gos3-ui .
#   docker run -p 3000:3000 \
#     -e NUXT_PUBLIC_SERVERS='[{"name":"NAS","endpoint":"http://10.0.0.5:9000"}]' gos3-ui
#
# Every server the browser talks to must allow the UI origin via CORS
# (gos3: cors.allowed_origins / GOS3_CORS_ORIGINS). The Node server has no
# authentication; set GOS3_UI_LOCAL_DISCOVERY=false to disable its probe and
# scan routes when it is reachable by untrusted clients. Those routes answer
# only for requests to an IP address or localhost: add the DNS names the UI
# is opened with to GOS3_UI_ALLOWED_HOSTS (comma separated), e.g.
#   -e GOS3_UI_ALLOWED_HOSTS=console.example.com
# See docs/web-ui.md.

ARG NODE_VERSION=22

FROM node:${NODE_VERSION}-bookworm-slim AS build
ENV PNPM_HOME=/pnpm \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    NUXT_TELEMETRY_DISABLED=1 \
    CI=true
ENV PATH=$PNPM_HOME:$PATH
RUN corepack enable
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm fetch --frozen-lockfile
COPY web/ ./
RUN --mount=type=cache,id=gos3-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile --offline && \
    pnpm build

FROM node:${NODE_VERSION}-bookworm-slim
ENV NODE_ENV=production \
    HOST=0.0.0.0 \
    PORT=3000 \
    NUXT_TELEMETRY_DISABLED=1
WORKDIR /app
# The Nitro output is self-contained (traced node_modules included).
COPY --from=build --chown=node:node /src/web/.output ./.output
USER node
EXPOSE 3000/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/api/discovery/capabilities').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
CMD ["node", ".output/server/index.mjs"]

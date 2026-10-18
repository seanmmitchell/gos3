# gos3 web UI

Nuxt 4 SPA (TypeScript strict, Nuxt UI v4, Pinia) that manages one or more S3
servers — gos3 or any S3-compatible service — with gos3-only extras (IAM,
settings, metrics, discovery) through the admin API.

The web UI is part of the public, MIT-licensed
[gos3 repository](https://github.com/seanmmitchell/gos3). Report bugs in the
[public issue tracker](https://github.com/seanmmitchell/gos3/issues) and target
pull requests at `dev`; see the [development guide](../README.md#development).

It ships in two modes from one codebase:

| Mode | Build | Served by | Notes |
|---|---|---|---|
| **Embedded** | `pnpm generate:embed` → `.output/public/` | the gos3 binary at `/_gos3/ui/` (copied to `internal/webui/dist/` by the integration build) | fully static, offline-safe; auto-detects the hosting server (same origin, no CORS) |
| **Standalone** | `pnpm build` → `.output/` | `node .output/server/index.mjs` | adds Nitro server routes in `server/` for *local* network discovery (mDNS / beacon / scan; off unless `GOS3_UI_LOCAL_DISCOVERY=true`) |

## Screenshots

| | |
|---|---|
| ![Dashboard](../docs/screenshots/dashboard.png) | ![Object browser](../docs/screenshots/browser.png) |
| Dashboard: every server with live status, identity and storage totals | Object browser: folders, uploads (drag & drop, multipart), bulk actions |
| ![Object details](../docs/screenshots/object-details.png) | ![Bucket settings](../docs/screenshots/bucket-settings.png) |
| Object details: metadata, checksums, preview, tags, versions, share links | Bucket settings: versioning, policy, CORS, lifecycle, tags, encryption, object lock |
| ![Server overview](../docs/screenshots/overview.png) | ![Users](../docs/screenshots/users.png) |
| Server overview: storage, request metrics and per-operation latency | IAM users, access keys, policies and a permission simulator |
| ![Discovery](../docs/screenshots/discovery.png) | ![Dark mode](../docs/screenshots/browser-dark.png) |
| Discovery: peers found via mDNS, beacon, seeds, DNS SRV and scans | Dark mode follows the system (also [dashboard](../docs/screenshots/dashboard-dark.png)) |

The images are produced by `pnpm screenshots` (Playwright project
`screenshots`, `test/e2e/screenshots/readme.shots.ts`): it builds a gos3
binary with the current embedded UI (into `.e2e-cache/`, never touching
`.output` or `internal/webui/dist`), starts two instances, fills one with demo
data (buckets, folders, varied file types, a versioned bucket with lifecycle
rules, users), keeps S3 traffic flowing for `SHOTS_TRAFFIC_MINUTES` (default
5) so the metrics chart has data, and writes 1440×900 PNGs (each checked to
stay under 400 KB) to `docs/screenshots/`. `SHOTS_ONLY=dashboard,users`
re-captures a subset.

## Commands

```bash
pnpm install          # also runs `nuxt prepare`
pnpm dev              # http://localhost:3000 (standalone, hot reload)
pnpm typecheck        # vue-tsc via `nuxt typecheck`
pnpm test             # Vitest unit tests (test/unit)
pnpm build            # standalone Node server (Nitro preset node-server)
pnpm generate         # static SPA at base '/'
pnpm generate:embed   # static SPA at base '/_gos3/ui/' (NUXT_PUBLIC_EMBEDDED=true)
pnpm preview          # preview the last build
pnpm test:e2e         # Playwright tests against mocked APIs (builds the embed bundle first)
pnpm test:e2e:real    # Playwright end-to-end tests against real gos3 binaries (see below)
pnpm test:e2e:standalone  # Playwright: standalone Node UI + two real gos3 binaries, cross-origin
pnpm screenshots      # regenerate docs/screenshots/*.png (see Screenshots)
```

Use the pnpm version pinned in `packageManager` (corepack or pnpm's own
version switching selects it). `pnpm-workspace.yaml` holds the pnpm settings:
a 14-day release cooldown (`minimumReleaseAge`), overrides that force patched
transitive packages or remove unused WebAssembly fallbacks, and the allowlist of packages that may run install
scripts. When an install fails with `ERR_PNPM_NO_MATURE_MATCHING_VERSION`, lower
the dependency's version range instead of excluding it from the cooldown;
exclusions are for security fixes only (see `docs/security.md`).

Builds normally write `.nuxt/` and `.output/`. `NUXT_BUILD_DIR` and
`NUXT_OUTPUT_DIR` redirect them, so a second build can run next to the usual
one without overwriting it (the standalone e2e setup builds into
`.e2e-cache/standalone-*` this way).

`pnpm test:e2e` normally builds the embedded UI first. With
`E2E_SKIP_UI_BUILD=1`, it serves an existing embedded build; `make e2e-web`
uses this after `make build`. CI transfers its web job's embedded and
standalone builds by immutable artifact ID, so browser tests exercise those
outputs without rebuilding them. The standalone preset variant still builds
separately because it includes a different build-time `servers.json`.

`pnpm test:e2e` never downloads browsers: `playwright.config.ts` uses
`PW_CHROMIUM_PATH` or the newest `chromium-*` under `PLAYWRIGHT_BROWSERS_PATH`
(default `/opt/pw-browsers`). `test/e2e/serve.mjs` serves `.output/public`
under `/_gos3/ui/` and mocks `GET /_gos3/info`.

During `pnpm dev` against a local gos3, the gos3 server must allow the dev
origin via CORS (`cors.allowed_origins: ["http://localhost:3000"]`, or
`gos3 serve --cors-origin http://localhost:3000`; `make dev` does this) and
expose `ETag`, `x-amz-version-id`, … (`cors.expose_headers`).

## Standalone mode

```bash
pnpm build
PORT=3000 HOST=0.0.0.0 node .output/server/index.mjs     # Nitro node-server (also NITRO_PORT / NITRO_HOST)
NUXT_PUBLIC_SERVERS='[{"name":"NAS","endpoint":"http://10.0.0.5:9000"}]' node .output/server/index.mjs
GOS3_UI_LOCAL_DISCOVERY=true HOST=127.0.0.1 node .output/server/index.mjs   # + local discovery (trusted clients only)
```

The browser talks to every S3 server **directly** (requests are signed in the
browser), so each server must allow the UI's origin via CORS and expose the
response headers the UI reads:

| Header | Needed for |
|---|---|
| `ETag` | multipart uploads (else `S3Error` `MissingETag`), object details |
| `x-amz-meta-*`, `Content-Disposition`, `Content-Encoding`, `Date` | the metadata editor — when a cross-origin HeadObject hides `Date` (not every header exposed), details show "Some headers are hidden" and the editor refuses to save rather than wipe metadata it cannot see |
| `x-amz-version-id`, `x-amz-delete-marker`, `x-amz-checksum-*`, `x-amz-object-lock-*`, `Content-*`, `Last-Modified` | versions, details, checksums, retention |

gos3's default `cors.expose_headers` starts with `"*"` and lists these names
explicitly, so only `cors.allowed_origins` needs the UI origin. For other S3
services, configure the equivalent (`ExposeHeaders` in the bucket CORS rules
for AWS; `x-amz-meta-*` wildcards are not supported there, so list `Date`
and every metadata name to enable the metadata editor). The same applies to
gos3 buckets with their own CORS rules, which reject `ExposeHeader`
wildcards. See [docs/web-ui.md](../docs/web-ui.md#cors).
`GET /_gos3/info` always sends `Access-Control-Allow-Origin: *`, so the UI
can detect a gos3 server (and tell "CORS blocked" from "unreachable") before
CORS is configured.

### Local discovery routes (Nitro, standalone only)

The Node server adds routes that discover gos3 servers **from the UI host**
(the browser cannot do mDNS, multicast or port scans). They are **off unless
`GOS3_UI_LOCAL_DISCOVERY=true`** (also `1`, `on`, `yes`, `enabled`): the
server has no authentication. The static/embedded build has no server
routes; the UI detects that (`capabilities` → 404 or HTML) and hides the
feature.

| Route | Body / query | Response |
|---|---|---|
| `GET /api/discovery/capabilities` | | `{localDiscovery, mdns, beacon, scan, maxScanHosts}` |
| `GET /api/discovery/local?wait=<ms>` | `wait` 500–10000 (default 3000) | mDNS browse (`_gos3._tcp`) + beacon query (`239.255.73.83:7373`), then `/_gos3/info` verification: `{peers, errors, candidates, elapsedMs}` |
| `POST /api/discovery/scan` | admin API `ScanRequest` (`timeout` in ns) | `ScanResult`; ≤ 1024 hosts, ≤ 64 ports, ≤ 4096 probes, concurrency ≤ 64 |
| `POST /api/discovery/probe` | `{"url": "http://host:9000"}` | the server's `Info`, or 502 `ProbeFailed` with a coarse reason (`unreachable`, `timed out`, `HTTP <status>`, `authentication required`, `not a gos3 server`, `address not allowed`) |

- Errors use the admin envelope `{"error": {"code", "message"}}`.
- POSTs require `Content-Type: application/json` (415 otherwise). A
  cross-site page cannot send that without a CORS preflight, which this
  server never approves.
- Against DNS rebinding (a site whose name later resolves to the UI host
  becomes same-origin), every route answers 403 unless the `Host` header is
  an IP address, `localhost` / `*.localhost`, or a name listed in
  **`GOS3_UI_ALLOWED_HOSTS`** (comma separated; `*` turns the check off).
  Requests with a foreign `Origin` or `Sec-Fetch-Site: cross-site` /
  `same-site` are refused too. An `Origin` counts as foreign unless it
  matches `Host` or its host name is listed in `GOS3_UI_ALLOWED_HOSTS`
  (`*` does not count). Behind a reverse proxy that rewrites `Host` (nginx's
  default `proxy_pass`), list the public name or forward `Host`
  (`proxy_set_header Host $host;`), otherwise probes and scans get 403.
  Opening the console via another host name without listing it hides local
  discovery.
- At most 2 scans, 4 local discoveries and 8 probes run at once per UI
  server; more get 429 `TooManyRequests` (with `Retry-After`). Scans and
  probes take their slot before reading the request body.
- Request bodies are capped (probe 8 KiB, scan 64 KiB): 413 by
  `Content-Length` or as soon as a streamed body passes the cap (the
  connection is then closed), 408 when the body takes over 10 s, 400 on
  invalid JSON.
- Cloud metadata (`169.254.169.254`, `169.254.170.2`, `169.254.170.23`,
  `fd00:ec2::254`, `100.100.100.200`), unspecified, multicast and broadcast
  addresses (IPv4-mapped IPv6 forms included) are never probed: literals are
  refused up front (400; skipped inside CIDRs), and host names are checked
  against every resolved address at connect time
  (`server/utils/discovery/netguard.ts`), so DNS rebinding cannot bypass it.
- Probes read at most 64 KiB of an identity document (with or without
  `Content-Length`) and never follow redirects; local discovery keeps at
  most 2048 mDNS records and 1024 beacon candidates, and verifies at most
  256 URLs.
- The Node server has **no authentication**: with local discovery on,
  anyone who can reach it can make it probe and scan (bounded as above).
  Enable it only on a trusted interface. While it is off (the default),
  `local`, `scan` and `probe` answer 403, `capabilities` reports
  `localDiscovery: false` and the UI shows local discovery as turned off.
- mDNS uses `multicast-dns` when it can be loaded; the beacon needs UDP
  multicast on the host's network (often unavailable in containers — scans
  and probes still work).

### Connect page parameters

| URL | Shows |
|---|---|
| `/connect?server=<id>&redirect=<path>` | sign-in for an existing connection, then `redirect` (internal paths only; `//…`, schemes and `/connect` are ignored) |
| `/connect` | the hosting server's sign-in when embedded and not signed in, else the new-server form |
| `/connect?new=1` | always the new-server form (`appRoutes.newServer()`; used by every "Add server" entry) |
| `/connect?new=1&endpoint=<url>&name=<name>` | new-server form, prefilled (`appRoutes.newServer({ endpoint, name })`; `endpoint` alone implies `new`) |

### Configuration

| Runtime config | Env | Meaning |
|---|---|---|
| `app.baseURL` | `NUXT_APP_BASE_URL` | base path (`/` or `/_gos3/ui/`) — build time |
| `public.servers` | `NUXT_PUBLIC_SERVERS` | JSON array of preset connections |
| `public.embedded` | `NUXT_PUBLIC_EMBEDDED` | force embedded detection (also implied by a `/_gos3/ui` base) |
| `public.appName` | `NUXT_PUBLIC_APP_NAME` | display name (default `gos3`) |

Presets can also be shipped as `public/servers.json` (served as
`${baseURL}servers.json`; a missing file or an SPA HTML fallback is ignored).
`public/servers.example.json` is a documented template (shipped, never
loaded): copy it to `public/servers.json` **before** building. Both builds
bundle `public/` at build time — the standalone Node server serves public
files from a manifest made by `nuxt build`, so a `servers.json` dropped into
`.output/public` afterwards is not served. For runtime configuration of the
standalone server use `NUXT_PUBLIC_SERVERS`; the embedded build is static, so
there both mechanisms are fixed at build time.

```json
{ "servers": [
  { "id": "nas", "name": "NAS", "endpoint": "http://10.0.0.5:9000", "region": "us-east-1", "kind": "gos3" },
  { "name": "AWS eu", "endpoint": "https://s3.eu-west-1.amazonaws.com", "region": "eu-west-1", "kind": "s3", "forcePathStyle": false }
] }
```

Accepted keys: `id, name, endpoint|url, region, accessKeyId|accessKey,
secretAccessKey|secretKey, kind ('gos3'|'s3'), forcePathStyle|pathStyle,
color`. Presets without `id` get `preset-<endpoint-slug>`. Preset definitions
are read-only in the UI (only credentials can be entered); removing a preset
hides it in that browser. Avoid shipping secret keys: anyone who can load the
UI can read them.

### Offline / no remote assets

- `ui.fonts: false` (no `@nuxt/fonts`, no Google Fonts); system font stacks in `app/assets/css/main.css`.
- Icons: `@iconify-json/lucide` is bundled at build time (`icon.clientBundle.scan`
  over `app/**/*.{vue,ts}`, `provider: 'none'`, `fallbackToApi: false`).
  **Icon names must appear literally in source** (`'i-lucide-trash-2'`); names
  assembled at runtime are not bundled and render empty. Only use the `lucide`
  collection (plus the icons Nuxt UI itself uses).

## Architecture

```
app/
  app.vue                 UApp (toasts, overlays, tooltips) + NuxtLayout
  app.config.ts           Nuxt UI colors (primary teal, neutral zinc)
  error.vue               404 / error page
  layouts/default.vue     dashboard shell: sidebar (logo, server switcher, server nav, global nav,
                          identity menu), navbar (breadcrumbs, color mode), <slot/> in UDashboardPanel body
  layouts/blank.vue       centered layout (connect page)
  middleware/server.global.ts   /servers/:id/** guard (unknown → /servers, no credentials → /connect)
  plugins/connections.client.ts store.init(): hydrate, presets, embedded detection (awaited before routing)
  stores/connections.ts   Pinia store (connections, active, statuses, persistence, import/export)
  types/admin.ts          admin API + discovery types (mirror docs/admin-api.md; durations = ns)
  types/s3.ts             S3 types
  types/connection.ts     Connection, ConnectionStatus, ConnectionInput, export format
  types/ui.ts             KeyValuePair
  utils/s3/               S3 client (client.ts), XML (xml.ts), parsers, builders, errors, url, hash (MD5/CRC32)
  utils/admin/client.ts   AdminClient, AdminError, fetchInfo, probeServer
  utils/clients.ts        getS3Client / getAdminClient (memoised per connection)   [auto-imported]
  utils/connections.ts    connection helpers, EMBEDDED_CONNECTION_ID                 [auto-imported]
  utils/format.ts         formatBytes, formatNumber, formatDate, formatRelativeTime,
                          formatDuration (ns), formatSeconds, formatPercent, …       [auto-imported]
  utils/paths.ts          prefixBreadcrumbs, joinKey, parentPrefix, baseName, extName,
                          validateBucketName, validateObjectKey, routeParamToKey     [auto-imported]
  utils/routes.ts         appRoutes.* canonical links, routeParam()                  [auto-imported]
  utils/errors.ts         describeError, errorMessage, isAuthError, isAbortedError  [auto-imported]
  composables/            useConnection, useWhoAmI, useServerInfo, useAsyncAction, useLoader,
                          useConfirm, useBreadcrumbs
  components/common/      shared building blocks (prefix `Common`)
  components/layout/      AppLogo, ServerSwitcher, IdentityMenu (prefix `Layout`)
modules/third-party-notices.ts  records the npm packages each production build ships in
                          .notices/ (input of `make notices`, see docs/security.md)
server/                   Nitro routes for standalone local discovery (servers & discovery agent)
test/unit/                Vitest "unit" project (node env + DOMParser/Storage from test/setup.ts)
test/nuxt/                Vitest "nuxt" project (@nuxt/test-utils, components & composables)
test/e2e/                 Playwright: mocked smoke/feature specs + static server (serve.mjs),
                          real/ (embedded UI in real gos3), standalone/ (Node UI + real gos3),
                          screenshots/ (README screenshots, `pnpm screenshots`)
```

Nuxt auto-imports only top-level `app/utils/*.ts`; the clients in
`app/utils/s3/` and `app/utils/admin/` are imported explicitly:

```ts
import { S3Error } from '~/utils/s3/errors'
import type { S3Client } from '~/utils/s3/client'
import { AdminError, probeServer } from '~/utils/admin/client'
import { useConnectionsStore } from '~/stores/connections'
```

## File ownership (UI work items)

| Owner | Files |
|---|---|
| **ui-scaffold** | everything not listed below: config, `app.vue`, `app.config.ts`, `error.vue`, layouts, middleware, plugins, stores, types, utils, composables, `components/common/`, `components/layout/`, tests of those, this README |
| **servers & discovery** | `app/pages/index.vue`, `app/pages/servers/index.vue`, `app/pages/connect.vue`, `app/pages/discover.vue`, `app/pages/servers/[id]/discovery.vue`, `app/components/servers/`, `app/components/discovery/`, `server/` (Nitro routes) |
| **object browser** | `app/pages/servers/[id]/buckets/index.vue`, `app/pages/servers/[id]/buckets/[bucket]/index.vue`, `app/pages/servers/[id]/buckets/[bucket]/objects/[...path].vue`, `app/components/browser/` |
| **admin** | `app/pages/servers/[id]/index.vue`, `app/pages/servers/[id]/users/index.vue`, `app/pages/servers/[id]/users/[name].vue`, `app/pages/servers/[id]/policies/index.vue`, `app/pages/servers/[id]/policies/[name].vue`, `app/pages/servers/[id]/settings.vue`, `app/pages/servers/[id]/buckets/[bucket]/settings.vue`, `app/components/admin/`, `app/components/bucket-settings/` |

The pages listed above currently are placeholders (`// PLACEHOLDER`) — replace
them wholesale. Feature agents may add tests under `test/unit/<area>-*.test.ts`,
`test/nuxt/<area>-*.test.ts` and `test/e2e/<area>.spec.ts`. If you need a change in a scaffold file, keep it
minimal and mention it in your report.

## Conventions

### Getting clients

```ts
// In any page under /servers/:id — the id comes from the route.
const { connection, connected, isGos3, s3, admin, requireS3, requireAdmin } = useConnection()
const buckets = await requireS3().listBuckets()
const users = await requireAdmin().listUsers()   // throws for generic S3 servers
```

`S3Client` methods take the bucket explicitly (`putObject(bucket, key, body, opts)`,
`presignGetUrl(bucket, key, opts)` …). Highlights:

- listing: `listObjectsV2`, `listObjectsV2Pages` / `listAllObjects` (async iterators),
  `listObjectVersions`, `listObjectVersionsPages`, `listVersionsOfKey`, `listMultipartUploads`
- objects: `headObject` (metadata, checksums, lock info), `getObjectText` (size-capped
  preview, default 256 KiB), `getObjectRange`, `getObjectBlob`, `presignGetUrl`
  (downloads/share links, ≤ 7 days), `presignPutUrl`
- upload: `upload(bucket, key, file, { onProgress, signal })` picks PutObject
  (≤ 16 MiB) or `uploadLarge` (multipart, 4 parallel parts, retries, aborts on
  failure/cancel). Progress uses XHR; cancel with an `AbortController` →
  rejects with `S3Error` code `Aborted` (`isAbortedError(e)`).
- delete: `deleteObject(bucket, key, versionId?)`, `deleteObjects` (auto-batches
  1000, returns per-key `errors`), `deletePrefix(bucket, prefix, { allVersions })`
- `copyObject(src, dst, { metadataDirective: 'REPLACE', metadata })`, `moveObject`
- bucket config get/put/delete: tagging, versioning, policy (+`getBucketPolicyStatus`),
  CORS, lifecycle, encryption, object lock, ACL (canned); object tagging,
  retention, legal hold, ACL. "Not configured" returns `null` / `[]` instead of throwing.

Errors: `S3Error { code, status, message, requestId }` and
`AdminError { code, status, message }`. `NetworkError` (status 0) usually means
the server is down **or CORS blocked the request**.

The browser can only read response headers the server exposes via CORS — the
multipart upload needs `ETag` (else `S3Error` code `MissingETag`).

### Connections & capabilities

- `useConnectionsStore()` — `connections`, `sorted`, `ordered` (default server
  first), `activeId`, `active`, `defaultId`, `defaultConnection`, `setDefault`,
  `byId`, `add`, `update`, `remove`, `setActive`, `setCredentials`,
  `clearCredentials`, `exportJson({ includeSecrets })`, `importJson(text)`,
  `statusOf(id)`, `refreshStatus(id)`, `ensureStatus(id, maxAgeMs)`, `refreshAll()`,
  `findByEndpoint`, `findByServerId`, `embeddedInfo`.
- Secrets: `secretAccessKey` goes to localStorage only when `remember` is true,
  otherwise to sessionStorage. `accessKeyId` and the definition always persist.
- Status (`ConnectionStatus.state`): `unknown | checking | online | offline |
  unauthorized | error`, plus `info` (public `/_gos3/info`) and `whoami` (gos3 +
  credentials). For `kind: 's3'` connections the store probes `/_gos3/info` too, so
  `status.info` reveals a gos3 server configured as generic S3.
- Capability gating: `const { can, isAdmin, isGos3 } = useWhoAmI()` →
  `can('manageUsers')` etc. (`Capabilities` in `types/admin.ts`). Generic S3 →
  every capability false. The sidebar shows Users / Policies / Settings /
  Storage / Discovery only for `manageUsers` / `managePolicies` /
  `manageConfig` / `viewStorage` / `discovery`; the bucket settings
  Placement tab needs `viewStorage` (`bucketSettingsTabs()`) and its actions
  `manageStorage`. Pages should still handle `AccessDenied` gracefully.
- `EMBEDDED_CONNECTION_ID` (`'this-server'`) is the connection for the hosting gos3.
- Default server: `store.defaultId` (persisted with the connections; the
  former `gos3.defaultServer.v1` key is migrated) orders the dashboard, the
  servers table and the sidebar switcher, and is the sidebar's connection when
  no server is active (`useConnection()` falls back to active, then default).
- Route guard: every `/servers/:id/**` page may assume the connection exists
  and has credentials; `/connect?server=<id>&redirect=<path>` is where users
  enter credentials (then `router.replace(redirect)`).

### Routes

Always build links with `appRoutes` (`appRoutes.bucket(id, bucket, prefix)`,
`appRoutes.object(id, bucket, key, versionId)`, `appRoutes.connect(id, redirect)`,
`appRoutes.newServer({ endpoint?, name? })` …). Never link "Add server" to a bare
`appRoutes.connect()`: when embedded it opens the hosting server's sign-in.
Folder browsing uses `?prefix=` on the bucket page; object details use the
catch-all `objects/[...path]` (`routeParamToKey(route.params.path)`). The
layout derives breadcrumbs from these routes; override with
`useBreadcrumbs(() => [...])` (cleared on unmount).

### UI patterns

- Page skeleton: `<CommonPageHeader title icon description><template #actions>…</template></CommonPageHeader>` then content. The layout already provides padding, scrolling and max width.
- Async actions with toasts:
  ```ts
  const del = useAsyncAction((name: string) => requireS3().deleteBucket(name), {
    successMessage: name => `Bucket ${name} deleted`, errorTitle: 'Could not delete bucket'
  })
  ```
  Data loading: `const { data, loading, error, refresh } = useLoader(() => requireS3().listBuckets(), { watch: [() => route.params.id] })`.
- Destructive actions are always confirmed:
  ```ts
  const confirm = useConfirm()
  if (!(await confirm({ title: `Delete bucket ${name}?`, description: 'All objects will be lost.', confirmLabel: 'Delete', requireText: name }))) return
  ```
- States: loading (`USkeleton` / button `loading`), empty (`<CommonEmptyState>`),
  error (`<CommonErrorAlert :error retryable @retry="refresh" />`).
- Shared components: `CommonCopyButton`, `CommonByteSize`, `CommonRelativeTime`,
  `CommonStatusDot`, `CommonJsonEditor` (v-model string, `@validity`),
  `CommonKeyValueEditor` (v-model `KeyValuePair[]`, `max-items`, `validate-key`).
- Toasts: `useToast().add({ title, description, color: 'success' | 'error', icon })`.
- Accessibility: label every input (`UFormField` / `<label for>`), give icon-only
  buttons an `aria-label`, keep keyboard focus visible.
- Durations from the admin API are nanoseconds: `formatDuration(ns)`,
  `secondsToNs()` / `nsToSeconds()` for form fields.

### Testing

`pnpm test` runs two Vitest projects (`vitest.config.ts`):

| Project | Files | Environment | Use for |
|---|---|---|---|
| `unit` | `test/unit/**/*.test.ts` | Node + `test/setup.ts` (DOMParser from happy-dom, in-memory Web Storage) | plain TS modules: clients, parsers, store, utils — fast |
| `nuxt` | `test/nuxt/**/*.test.ts` | `@nuxt/test-utils` (`environment: 'nuxt'`, happy-dom) | components & composables that need auto-imports / Nuxt UI / Pinia |

- Unit tests use Node's native `fetch`/`Headers`/WebCrypto (browser-like);
  happy-dom's own `Headers` keeps header-name case and breaks SigV4. Inject a
  `fetch` into `S3Client`/`AdminClient` (`{ fetch }` option) or
  `vi.stubGlobal('fetch', …)`. Unit tests import modules explicitly (no auto-imports).
- Nuxt tests mount components with `mountSuspended(Component, { props, slots })`
  from `@nuxt/test-utils/runtime` and may call composables directly (see
  `test/nuxt/common-components.test.ts`, `test/nuxt/composables.test.ts`).
  Overlays/modals are teleported; prefer testing their logic in plain modules.
- Run one project: `pnpm vitest run --project unit` (or `nuxt`).
- Real-server E2E: `pnpm test:e2e:real` runs the Playwright project
  `real-server` (`test/e2e/real/*.spec.ts`) against the UI embedded in a real
  gos3. Its setup project (`real/global.setup.ts`) rebuilds the embed bundle
  when `app/` changed, builds `web/.e2e-cache/gos3` with `go build -overlay`
  (the build replaces `internal/webui/dist`, which stays untouched in the
  tree) when Go sources or the bundle are newer, and starts two instances on
  free loopback ports: *primary* (fs backend in a temp dir, root keys
  `e2eroot` / `e2eroot-secret-key`, discovery with scans, mDNS/beacon off,
  CORS for `http://localhost:3000`) and *peer*, each seeded with the other.
  The teardown project stops them (a watchdog also stops them if the run is
  aborted). Specs use the fixtures in `real/fixtures.ts` (`signIn`, `ui()`,
  a root-signed `api` client to verify state). Files run in parallel against
  the shared servers, so use `uniqueName()` for buckets/users and do not
  assert the server name. Env: `GOS3_BIN=<path>` uses a prebuilt binary,
  `E2E_SKIP_UI_BUILD=1` skips the bundle check, `E2E_KEEP_DATA=1` keeps data
  dirs and logs (`/tmp/gos3-e2e-*`).
- Standalone E2E: `pnpm test:e2e:standalone` runs the Playwright project
  `standalone` (`test/e2e/standalone/*.spec.ts`): the standalone UI as a real
  Node server against two real gos3 instances, so every API call is
  cross-origin. Its setup project (`standalone/global.setup.ts`):
  - runs `pnpm build` into `.e2e-cache/standalone-output` (via
    `NUXT_BUILD_DIR` / `NUXT_OUTPUT_DIR`, so `.output` stays untouched) when
    `app/`, `server/`, `public/` or the config changed, plus a second build of
    a temp copy of the sources (outside `web/`) that adds
    `public/servers.json` — presets never touch `web/public`;
  - `go build`s `.e2e-cache/gos3-standalone` when Go sources changed
    (`bin/gos3` is the fallback if that fails; `GOS3_BIN=<path>` overrides);
  - starts gos3 *alpha* and *beta* (memory backend, root keys `saroot` /
    `saroot-secret-key`, discovery with scans but no multicast and no seeds,
    CORS for both UI origins) and two UI servers: the plain one on
    `http://localhost:3000` (a free port if 3000 is taken) with
    `GOS3_UI_LOCAL_DISCOVERY=true`, and a preset one (`servers.json` +
    `NUXT_PUBLIC_SERVERS` at runtime) with local discovery off (the
    default). Processes run under
    `real/watchdog.mjs`, so an aborted run does not leave them behind.
  Specs cover adding a server (type detection, test connection, credentials),
  import/export, presets, browsing/uploads (multipart reads `ETag`) and the
  metadata editor reading `x-amz-meta-*` cross-origin, and the Discover page
  (UI-host scan of `127.0.0.1` with both ports, browser/UI-host probes, peer
  tables of connected servers, the off-by-default routes, metadata targets
  and body caps). Env: `E2E_SKIP_UI_BUILD=1`, `E2E_KEEP_DATA=1`
  (keeps `/tmp/gos3-standalone-*` with the logs).
- E2E: `pnpm test:e2e` (Playwright, `test/e2e/*.spec.ts`) builds the embedded
  bundle and serves it via `test/e2e/serve.mjs`, which mocks `GET /_gos3/info`.
  Extend the mock server there if your feature needs canned API responses.
- All test files are type-checked by `pnpm typecheck` (`test/tsconfig.json`).

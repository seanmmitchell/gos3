# gos3 web UI

Nuxt 4 SPA (TypeScript strict, Nuxt UI v4, Pinia) that manages one or more S3
servers — gos3 or any S3-compatible service — with gos3-only extras (IAM,
settings, metrics, discovery) through the admin API.

It ships in two modes from one codebase:

| Mode | Build | Served by | Notes |
|---|---|---|---|
| **Embedded** | `pnpm generate:embed` → `.output/public/` | the gos3 binary at `/_gos3/ui/` (copied to `internal/webui/dist/` by the integration build) | fully static, offline-safe; auto-detects the hosting server (same origin, no CORS) |
| **Standalone** | `pnpm build` → `.output/` | `node .output/server/index.mjs` | adds Nitro server routes in `server/` for *local* network discovery (mDNS / beacon / scan) |

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
pnpm test:e2e         # Playwright smoke tests (builds the embed bundle first)
```

`pnpm test:e2e` never downloads browsers: `playwright.config.ts` uses
`PW_CHROMIUM_PATH` or the newest `chromium-*` under `PLAYWRIGHT_BROWSERS_PATH`
(default `/opt/pw-browsers`). `test/e2e/serve.mjs` serves `.output/public`
under `/_gos3/ui/` and mocks `GET /_gos3/info`.

During `pnpm dev` against a local gos3, the gos3 server must allow the dev
origin via CORS (`cors.allowed_origins: ["http://localhost:3000"]`) and expose
`ETag`, `x-amz-version-id`, … (`cors.expose_headers`).

### Configuration

| Runtime config | Env | Meaning |
|---|---|---|
| `app.baseURL` | `NUXT_APP_BASE_URL` | base path (`/` or `/_gos3/ui/`) — build time |
| `public.servers` | `NUXT_PUBLIC_SERVERS` | JSON array of preset connections |
| `public.embedded` | `NUXT_PUBLIC_EMBEDDED` | force embedded detection (also implied by a `/_gos3/ui` base) |
| `public.appName` | `NUXT_PUBLIC_APP_NAME` | display name (default `gos3`) |

Presets can also be shipped as `public/servers.json` (served as
`${baseURL}servers.json`; a missing file or an SPA HTML fallback is ignored):

```json
{ "servers": [
  { "id": "nas", "name": "NAS", "endpoint": "http://10.0.0.5:9000", "region": "us-east-1", "kind": "gos3" },
  { "name": "AWS eu", "endpoint": "https://s3.eu-west-1.amazonaws.com", "region": "eu-west-1", "kind": "s3", "forcePathStyle": false }
] }
```

Accepted keys: `id, name, endpoint|url, region, accessKeyId|accessKey,
secretAccessKey|secretKey, kind ('gos3'|'s3'), forcePathStyle|pathStyle,
color`. Presets without `id` get `preset-<endpoint-slug>`.

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
server/                   Nitro routes for standalone local discovery (servers & discovery agent)
test/unit/                Vitest (node env + DOMParser/Storage from test/setup.ts)
test/e2e/                 Playwright smoke tests + static server
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
them wholesale. Feature agents may add tests under `test/unit/<area>-*.test.ts`
and `test/e2e/<area>.spec.ts`. If you need a change in a scaffold file, keep it
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

- `useConnectionsStore()` — `connections`, `sorted`, `activeId`, `active`,
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
  Discovery only for `manageUsers` / `managePolicies` / `manageConfig` /
  `discovery`. Pages should still handle `AccessDenied` gracefully.
- `EMBEDDED_CONNECTION_ID` (`'this-server'`) is the connection for the hosting gos3.
- Route guard: every `/servers/:id/**` page may assume the connection exists
  and has credentials; `/connect?server=<id>&redirect=<path>` is where users
  enter credentials (then `router.replace(redirect)`).

### Routes

Always build links with `appRoutes` (`appRoutes.bucket(id, bucket, prefix)`,
`appRoutes.object(id, bucket, key, versionId)`, `appRoutes.connect(id, redirect)` …).
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

Unit tests run in Node (native `fetch`/`Headers`/WebCrypto, which behave like
browsers) with `DOMParser` and Web Storage installed by `test/setup.ts` —
happy-dom's own `Headers` keeps header-name case and breaks SigV4. Inject a
`fetch` into `S3Client`/`AdminClient` (`{ fetch }` option) or `vi.stubGlobal('fetch', …)`.
Code using Nuxt auto-imports (components, composables) is not covered by the
unit setup; prefer keeping logic in plain TS modules with explicit imports.

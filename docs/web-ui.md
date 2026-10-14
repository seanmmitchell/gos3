# Web UI

The gos3 web UI is a single-page app (Nuxt 4, TypeScript) for managing one or
more S3 servers: gos3 servers, and also any other S3-compatible service for the
bucket and object features. It runs in two modes built from the same code in
`web/`.

| | Embedded | Standalone |
|---|---|---|
| Where | Inside the gos3 binary, at `http://<server>:9000/_gos3/ui/` | A Node.js server (`node .output/server/index.mjs`), default port 3000 |
| Build | `make build` (or `cd web && pnpm generate:embed`, then `make build-go`) | `cd web && pnpm build`, or `docker build -f deploy/docker/web.Dockerfile .` |
| Default connection | The hosting gos3 server, detected automatically | None; add servers or ship presets |
| CORS needed | No for the hosting server (same origin); yes for other servers it manages | Yes, for every server |
| Local discovery (mDNS, beacon, scan from the UI host) | No (needs the Node server) | Optional, off by default (`GOS3_UI_LOCAL_DISCOVERY=true`) |
| Presets | `servers.json` baked in at build time | `NUXT_PUBLIC_SERVERS` at runtime, or `servers.json` at build time |

The UI works offline: fonts and icons are bundled, and it loads nothing from
the internet.

## What it does

- **Dashboard** of all configured servers with status, version and usage.
- **Buckets**: create and delete; versioning, policy editor with templates,
  CORS, lifecycle, tags, object lock, default encryption and ACL settings,
  and (gos3 only) compression and restic encryption providers.
- **Object browser**: folders, drag-and-drop uploads (multipart with parallel
  parts and progress), downloads, delete, rename and copy, metadata and tag
  editors, versions, and presigned share links.
- **IAM** (gos3 only): users, access keys, policies, and a policy simulator.
- **Server overview and settings** (gos3 only): version, request metrics,
  storage and disk usage, and every runtime setting from
  [configuration.md](configuration.md#runtime-settings-settingsjson).
  Server and bucket settings show one section at a time, picked with the
  chips under the page header (kept in the URL as `?tab=`); on the server
  settings page one Save covers every section, and the chips mark sections
  with unsaved changes or invalid fields.
- **Discovery** (gos3 only): peers seen by each connected server, network
  scans, local discovery from the UI host, and one-click "add server".
- **Storage** (gos3 only): the server's storage backings, and per bucket
  its placement, replication, moves and promotions (see
  [Storage backings and placement](#storage-backings-and-placement)).

Pages are shown according to the signed-in identity's permissions (from
`GET /_gos3/admin/v1/whoami`). For example, Users appears only with
`admin:CreateUser`. Creating a user with policies also needs `admin:UpdateUser`,
and with an initial access key (the create form's "Create an access key
now" option, on by default) also `admin:CreateAccessKey`. A role with only
`admin:CreateUser` gets a 403 from the create form unless it selects no
policies and clears that option.

- **Self-service keys.** A user without `admin:CreateUser` sees "My access
  keys" on the Users page: it lists the user's own keys and deletes them
  (except the key signing the session). Creating a key there needs a policy
  that explicitly allows `admin:CreateOwnAccessKey` (whoami capability
  `createOwnKeys`; no built-in policy except `admin` grants it, and it is
  always false for root, which has no IAM keys). Without it the "Create key"
  button is hidden and the card names the missing action. The "Own access
  keys" policy template grants it. With a server that does not report
  `createOwnKeys` yet, the UI assumes creation is allowed, as such servers
  allowed it unless explicitly denied.
- **Discovery probe.** "Probe a URL" on a server's Discovery page
  (`POST /discovery/probe`) needs `admin:ProbeNetwork` (whoami capability
  `probe`). Reading the peer table needs only `admin:GetDiscovery`, so the
  built-in `diagnostics` policy shows the peers but not the probe form; the
  card says that `admin:ProbeNetwork` is missing instead. With a server that
  does not report `probe` yet, the UI falls back to `discovery`
  (`admin:GetDiscovery`), the permission such servers checked.
- **Force delete and object lock.** Force delete (`admin:ForceDeleteBucket`,
  bucket list and the bucket's Danger zone) is refused with a 409 while
  versions are under legal hold or COMPLIANCE retention; the UI shows the
  server's explanation. Versions under GOVERNANCE retention are deleted only
  with "Bypass governance retention" (Danger zone checkbox, or the follow-up
  confirmation on the bucket list), which sends
  `bypassGovernanceRetention=true` and needs `s3:BypassGovernanceRetention`.
- **Encryption.** Bucket settings offers native AES256 or restic on gos3.
  Restic supports a local repository or a dedicated remote S3 prefix, with
  server-managed passwords and credentials. This encrypts new writes; older
  objects are unchanged. See [encryption.md](encryption.md).
- **Compression.** The server settings page has a Compression section where
  the administrator chooses the mode (Optional, Enforced, Disabled), the
  DEFLATE level, the minimum size and the media types to skip. A bucket's
  Compression tab (gos3 servers that report the `manageCompression`
  capability) shows whether new objects are compressed, the bucket's own
  setting and the server mode; while the administrator enforces or disables
  compression the switch is locked in the direction the server would refuse
  (409), and "Reset to server default" removes the bucket's setting. The tab
  is shown to everyone on such servers, since a grant of
  `admin:PutBucketCompression` scoped to some buckets does not show in the
  capabilities; a refused change shows the server's 403. Bucket lists, the
  bucket overview and the server overview show the stored size and the
  saving next to the size when compression saves space. See
  [operations.md](operations.md#compression).
- **Policy editor and simulator.** Admin actions are evaluated against
  `arn:gos3:admin:::user/<name>`, `arn:gos3:admin:::policy/<name>` and
  `arn:gos3:admin:::bucket/<name>` (see [admin-api.md](admin-api.md));
  `*` and `arn:gos3:admin:::*` match all of them. The policy editor lists
  these forms and the `admin:PolicyName` condition key (the policies a
  CreateUser / UpdateUser request attaches), and the "Manage some users"
  template scopes user management to a name pattern and to a list of
  attachable policies. Such a delegated manager still gets the Users page
  (it lists all users; actions on users outside the pattern are refused by
  the server and shown as errors). For an admin action the simulator asks for an admin
  resource instead of a bucket and key (empty = service level).

### Storage backings and placement

On servers with several storage backings (see
[admin-api.md](admin-api.md#storage-backings-and-bucket-placement)):

- **Storage** (sidebar, needs `admin:ListBackings`, whoami capability
  `viewStorage`) shows every backing with its kind, path, health (the last
  error of an unhealthy one), last check, bucket and replica counts and disk
  usage. It refreshes every 10 seconds.
- **Bucket settings → Placement** (also `viewStorage`) shows the replication
  mode and each member (primary, replicas, a move target) with its state
  (`in sync`, `syncing`, `lagging`, `degraded`, `offline`), pending changes,
  copy progress, last full sync and last error, and a banner while the
  bucket moves. The tab polls every 3 seconds while a copy runs, changes are
  pending or a move is in progress.
- With `admin:UpdateBucketPlacement` (capability `manageStorage`) the tab
  changes the mode (single, active-passive, active-active) and the replica
  backings (removing a replica asks first, because its copy is deleted),
  moves the bucket (or one replica) to a backing without a copy, cancels a
  move while it is copying, promotes a replica to primary and resyncs one or
  all replicas. "Force promote" is offered when the replica is not in sync
  or the primary is offline; it asks you to type the backing name, because
  changes not yet copied to the replica are lost. Without the capability the
  tab is read-only.
- The bucket list shows the primary backing, the mode with the number of
  copies (for example `AP ×2`) and `moving`, when the server reports
  placement and the buckets are not all on one backing.
- Servers that do not report `viewStorage` (older versions) show neither the
  page nor the tab.

### Object browser notes

- Listings use `encoding-type=url` (ListObjectsV2, ListObjectVersions and
  ListMultipartUploads), so keys with any characters are shown correctly.
- Keys with `.` or `..` path segments (for example `a/../b`, created by
  other clients) cannot be read, previewed, downloaded, shared, tagged or
  edited from the browser, because browsers resolve such segments in URLs.
  The UI says so and still allows Copy, Move and Delete for them (their
  links use `?key=` / `?prefix=`). The UI itself never creates such keys:
  uploads, new folders, renames and copy/move destinations are validated.
- Bucket names `.` and `..` (and names with `/`, `\` or control characters)
  are refused before any request is sent, for the same reason.
- The Markdown preview renders in linear time and nests block quotes at
  most 16 levels deep (deeper `>` markers stay text); if rendering fails,
  the raw text is shown.
- Copy, move and rename refuse a destination inside the source folder, and
  ask before overwriting existing objects at the destination.
- Uploads (including folder uploads and drag-and-drop) also ask before
  overwriting. Both check the server, not the rows on screen: with HeadObject
  for 20 keys or fewer, otherwise by listing the common prefix and then with
  HeadObject for the keys the listing did not reach (up to 500). When the
  check is incomplete (too many objects, or HeadObject denied, for example
  with 403), the UI asks too.
- Move, rename, restoring a version and the metadata editor rewrite the
  object, and keep a canned object ACL such as `public-read` (one extra
  GetObjectAcl per object; custom grants produce a warning). Keeping the ACL
  needs `s3:PutObjectAcl` where the server enforces it (gos3 and AWS do).
- Names are used as given: backslashes in uploaded file names stay in the
  key (they are not turned into `/`), and leading or trailing spaces in
  rename, copy/move destination folder and new-folder names are kept. Only
  empty or all-blank names are rejected.
- A non-ASCII `Content-Disposition` file name is stored in the RFC 6266 /
  RFC 5987 form (`filename="ascii"; filename*=UTF-8''…`). Other non-ASCII
  content in that header is rejected by the metadata editor.
- gos3 serves objects with `Content-Security-Policy: sandbox`, so an HTML or
  SVG object opened from a gos3 bucket in a browser tab (a download or share
  link) runs no scripts (see [security.md](security.md#object-responses)).

![Dashboard](screenshots/dashboard.png)

## Signing in and credentials

Every request is signed in the browser with AWS SigV4, both for the S3 API and
the admin API. There are no sessions or cookies. Admin API requests also sign
their payload: `X-Amz-Content-Sha256` is the hex SHA-256 of the exact body
sent (of the empty string when there is none), so a captured request cannot
be replayed with another body; gos3 rejects admin requests with a body whose
payload is not signed. (S3 requests keep `UNSIGNED-PAYLOAD`.) A connection is
`{name, endpoint, region, accessKeyId, secretAccessKey, kind, forcePathStyle}`,
where `kind` is `gos3` or `s3`.

- With "remember" on, the secret key is kept in the browser's `localStorage`.
  Otherwise it is kept in `sessionStorage` and is gone when the tab closes.
  Use "remember" only on a trusted device.
- Connections can be exported and imported as JSON, with or without secrets.
  Before importing, the dialog lists what each entry will do: add a server,
  update fields of an existing one (secret values are never shown), or skip
  it. An entry matches an existing connection by ID, or by endpoint and
  access key. An import never points an existing connection, and the secret
  stored with it, at another server: an entry that would change a
  connection's endpoint or kind is added as a separate server instead, with
  only the credentials the file itself holds. The embedded "this server"
  connection and presets are never changed by an import (matching entries
  for the same server are skipped), and imported entries cannot take their
  IDs. Changing the access key without a new secret clears the stored
  secret, and `remember` in a file is ignored: imported secrets stay in
  `sessionStorage` until you turn "remember" on (an import that replaces
  the secret of a remembered connection turns "remember" off).
- Several open tabs share connection changes (added, edited and removed
  connections and remembered secrets). The active server and secrets kept
  only for the session stay per tab.
- Downloads and share links are presigned URLs (up to 7 days).
- Keys entered on the sign-in page ("Connect to a server") are checked
  against the endpoint you typed and stored only with that endpoint: on a
  new connection, or on an existing one for the same endpoint that has no
  keys or the same access key. A stored connection that merely reports the
  same server ID (server IDs are not authenticated) is never given them.
- `/connect?endpoint=<url>&name=<name>` only prefills the form; it is never
  submitted automatically. The page says that a link filled in the address
  (and name). When its origin (scheme, host and port) is neither the UI's
  own nor one of your configured servers', you must tick "I trust <origin>"
  before connecting. Plain http for a host the UI or a configured server
  reaches over https also needs the tick, and the page warns about it.
  The post-login `?redirect=` accepts only in-app paths without `.`/`..`
  segments, encoded slashes or backslashes.
- **Adding discovered servers.** "Add as server" (Discover page and a
  server's Discovery page) prefills the address the server was observed and
  verified at, never the endpoint it advertises about itself, which is shown
  only as an unverified hint. Credentials are never prefilled. "Use the same
  credentials as <server>" is off by default; when it would sign requests
  for another host, the form names that host, warns that discovery is
  unauthenticated (and when the keys are root keys), and saves or tests
  only after "I trust <host> with <server>'s keys" is ticked. A server
  reporting the server ID of a configured connection at another address is
  flagged and added as a separate connection; the configured one is not
  changed.
- On the Discover page, results from different sources are merged by
  server ID only when they describe the same host (a shared address, or
  each advertising the other's address). Otherwise the row keeps the first
  record, is marked "ID conflict" and lists the other addresses as
  conflicting, and it offers no credential reuse. Credentials are only
  offered from a connected server whose own peer table lists the
  prefilled address. An https endpoint is never replaced by an http one.
  A discovered server counts as "Configured" (linked, and hidden by "hide
  configured") only when a connection points at an address it was observed
  at. One that only reports a configured server's ID is marked "ID of
  <name>" and stays listed.
- The `.env` and `~/.aws` snippets shown after creating an access key are
  only offered when the returned access key ID, secret key, region and
  endpoint have the expected format (no line breaks or control
  characters); otherwise the dialog says so and offers no snippets or
  downloads. Caller-chosen secret keys must be 16–128 printable characters.
  Choosing the access key ID yourself needs `admin:CreateAccessKey` at
  service level; admins scoped to some users get a 403 (the form says so)
  and can create generated key pairs.

## Embedded mode

`gos3 serve` serves the UI at `/_gos3/ui/` unless `ui.enabled` is false
(`--no-ui`). An anonymous browser opening `/` is redirected there unless
`ui.redirect_root` is false. A binary built without the UI (plain `go build`)
serves a placeholder page that says so; `make build` embeds the real UI.

The UI asks the hosting server for `/_gos3/info` and offers it as the default
connection ("this server"). You only need to enter the access keys. It can
manage other servers too, as long as their CORS settings allow the hosting
server's origin.

UI responses carry `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`nosniff` and a `Content-Security-Policy` with `default-src 'self'`,
`object-src 'none'`, `base-uri 'none'`, `form-action 'none'` and
`frame-ancestors 'none'`. Its `script-src` allows `'self'` plus the SHA-256
hashes of the inline scripts in the embedded HTML (Nuxt's import map and
runtime config, the color-mode bootstrap), which gos3 computes from the build
at startup, so a rebuilt UI needs no extra step. `connect-src`, `img-src` and
`media-src` allow any http(s) origin because the UI talks to, and previews
objects from, any server you add. Hashed assets are cached as immutable;
everything else is revalidated.

## Standalone mode

```sh
cd web
pnpm install --frozen-lockfile
pnpm build
HOST=0.0.0.0 PORT=3000 node .output/server/index.mjs
```

Or with Docker:

```sh
docker build -f deploy/docker/web.Dockerfile -t gos3-ui .
docker run -p 3000:3000 gos3-ui
```

The `docker compose --profile ui` demo in the repository root runs it next to
two gos3 nodes (see [deployment.md](deployment.md#docker-compose)).

The Node server sends the embedded UI's headers on every response
(`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `nosniff` and the
same `Content-Security-Policy`) plus `Permissions-Policy` denying camera,
microphone, geolocation, payment and USB. Because the runtime config it
inlines comes from the environment, the `script-src` hashes of each page's
inline scripts are computed when the page is rendered. `nuxt generate`
ignores these settings; gos3 sends its own headers for the embedded build.

### Presets

Ship a list of servers so that users only enter credentials:

- **`NUXT_PUBLIC_SERVERS`** (standalone, at runtime): a JSON array, for example
  `NUXT_PUBLIC_SERVERS='[{"name":"NAS","endpoint":"http://10.0.0.5:9000","kind":"gos3"}]'`.
- **`servers.json`** (both modes, at build time): copy
  `web/public/servers.example.json` to `web/public/servers.json` **before**
  building. It is served as `<base>/servers.json` (by the standalone Node
  server from a route behind the Host check below, not as a static file;
  `servers.example.json` is not served there). Adding the file to the build
  output afterwards has no effect, because both builds bundle `public/` at
  build time.

```json
{ "servers": [
  { "id": "nas", "name": "NAS", "endpoint": "http://10.0.0.5:9000", "region": "us-east-1", "kind": "gos3" },
  { "name": "AWS eu", "endpoint": "https://s3.eu-west-1.amazonaws.com", "region": "eu-west-1", "kind": "s3", "forcePathStyle": false }
] }
```

Accepted keys: `id`, `name`, `endpoint` (or `url`, required), `region`,
`kind` (`gos3` or `s3`), `forcePathStyle` (or `pathStyle`), `color`,
`accessKeyId` (or `accessKey`) and `secretAccessKey` (or `secretKey`). Presets
are read-only in the UI apart from their credentials. **Secret keys in presets
are ignored** (with a browser console warning), because anyone who can load
the UI can read presets; an `accessKeyId` only prefills the sign-in. When a
preset's endpoint changes, the secret key a user entered for it is
cleared, so it is never signed for the new host without the user signing
in again. The embedded UI's
`servers.json` is served without `secretAccessKey`/`secretKey` (gos3 strips
them and logs a warning at startup, and does not serve a `servers.json` that
is not valid JSON); gos3 also warns when a build has secrets baked in through
`NUXT_PUBLIC_SERVERS`. `pnpm build` and `pnpm generate` fail when
`public/servers.json` or `NUXT_PUBLIC_SERVERS` contains a non-empty
`secretAccessKey`/`secretKey` (set `GOS3_UI_ALLOW_PRESET_SECRETS=true` to
build anyway), and the Node server logs a warning when `NUXT_PUBLIC_SERVERS`
holds one at runtime. `web/public/servers.json` is git-ignored.

When presets are configured through `NUXT_PUBLIC_SERVERS`, and always for
`servers.json`, the Node server answers `421 Misdirected Request` unless the
`Host` (and any `X-Forwarded-Host` / `Forwarded: host=`) is an IP address,
`localhost`, a name that cannot exist in public DNS (a single label, or
`.local`, `.localhost`, `.internal`, `.home.arpa`, `.localdomain`, `.lan`,
`.home`, `.corp`) or a name listed in `GOS3_UI_ALLOWED_HOSTS`. Otherwise a
website could rebind its own name to the console's address and read the
presets inlined into the page. If you open the console with presets through
a public DNS name, list that name in `GOS3_UI_ALLOWED_HOSTS`.

Other runtime variables of the Node server:

| Variable | Meaning |
|---|---|
| `HOST`, `PORT` (or `NITRO_HOST`, `NITRO_PORT`) | Listen address (default port 3000). |
| `NUXT_PUBLIC_SERVERS` | Preset connections (JSON array). |
| `NUXT_PUBLIC_APP_NAME` | Display name (default `gos3`). |
| `GOS3_UI_LOCAL_DISCOVERY` | `true` (also `1`, `on`, `yes`, `enabled`) turns on the local discovery routes below. They are off when it is unset or has any other value. |
| `GOS3_UI_ALLOWED_HOSTS` | Extra host names (comma separated) under which the discovery routes below answer, and under which the console is served when presets are configured (see [Presets](#presets)), for example `console.example.com`. IP addresses, `localhost` and `*.localhost` are always accepted as `Host`. A listed name is also accepted as the host name of the browser's `Origin` (any port), which matters behind a reverse proxy that rewrites `Host`. `*` only turns the `Host` check off; it does not accept any `Origin`. Set it whenever the UI is reached through a DNS name. |
| `GOS3_UI_ALLOW_PRESET_SECRETS` | Build time: `true` lets `pnpm build`/`generate` proceed although presets contain secret keys (not recommended). |

### Local discovery routes

Browsers cannot send mDNS or multicast packets or scan networks, so the
standalone Node server can do this from the UI host. **These routes are off
unless `GOS3_UI_LOCAL_DISCOVERY=true`.** While they are off, `capabilities`
reports `localDiscovery: false` (the Discover page says local discovery is
turned off and still probes from the browser), and `local`, `scan` and
`probe` answer `403 AccessDenied`.

| Route | Purpose |
|---|---|
| `GET /api/discovery/capabilities` | `{localDiscovery, mdns, beacon, scan, maxScanHosts}`. The UI hides local discovery when this route is missing (embedded mode). |
| `GET /api/discovery/local?wait=<ms>` | mDNS browse (`_gos3._tcp`) plus a beacon query (`239.255.73.83:7373`), then verification through `/_gos3/info`. `wait` is 500–10000 ms (default 3000). |
| `POST /api/discovery/scan` | Scans CIDRs, hosts or URLs (an admin API `ScanRequest`): at most 1024 hosts, 64 ports, 4096 probes and concurrency 64. |
| `POST /api/discovery/probe` | `{"url": "http://host:9000"}` → that server's `Info`, or 502 `ProbeFailed` with a coarse reason: `unreachable`, `timed out`, `HTTP <status>`, `authentication required`, `not a gos3 server` or `address not allowed`. |

**The Node server has no authentication.** With local discovery on, anyone
who can reach it can make it probe and scan, within the limits below. Turn it
on only where every client is trusted: bind the server to a trusted interface
(the compose demo publishes it on `127.0.0.1`) or put it behind an
authenticating proxy.

Each UI server runs at most 2 `POST /api/discovery/scan`, 4
`GET /api/discovery/local` and 8 `POST /api/discovery/probe` requests at once;
further requests get `429 TooManyRequests` (the admin API error envelope) with
`Retry-After: 5`. Scans and probes take their slot before the request body is
read. Request bodies are capped at 8 KiB for probes and 64 KiB for scans:
a larger `Content-Length`, or a streamed body that grows past the cap, gets
`413` and the connection is closed; a body that does not arrive within 10
seconds gets `408`, invalid JSON `400`.

Probes, scans and the verification of discovered candidates never connect to
cloud metadata addresses (`169.254.169.254`, `169.254.170.2`,
`169.254.170.23`, `169.254.0.23`, `168.63.129.16`, `192.0.0.192`,
`100.100.100.200`, `fd00:ec2::254`, `fd00:ec2::23`, `fd20:ce::254`), unspecified
(`0.0.0.0/8`, `::`), multicast or broadcast addresses, including their
IPv4-mapped, IPv4-compatible and NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`)
IPv6 forms. IP literals are refused up front (a probe or an
explicit scan target answers `400`; such addresses inside a CIDR are
skipped), and host names are checked against every address they resolve to
when the connection is made, so DNS rebinding cannot bypass the check.
Loopback, private ranges and other link-local addresses stay allowed.

Protection against other websites:

- Against DNS rebinding (a site whose name later resolves to the UI host
  becomes same-origin), every `/api/discovery/*` route answers 403 unless
  the `Host` header is an IP address, `localhost` / `*.localhost`, or a name
  listed in `GOS3_UI_ALLOWED_HOSTS` (comma separated; `*` turns the check
  off). The same applies to every `X-Forwarded-Host` and `Forwarded: host=`
  value, so a reverse proxy that rewrites `Host` but forwards the original
  name still lets the check see it. Opening the console through an unlisted
  host name hides local discovery.
- Requests with a foreign `Origin`, or with `Sec-Fetch-Site: cross-site` or
  `same-site`, are refused too. An `Origin` counts as foreign unless it
  matches the `Host` header or its host name is listed in
  `GOS3_UI_ALLOWED_HOSTS` (`*` does not count for `Origin`). Behind a
  reverse proxy that rewrites `Host` (nginx's default `proxy_pass`), either
  list the public name in `GOS3_UI_ALLOWED_HOSTS` or forward the original
  host (`proxy_set_header Host $host;` or
  `proxy_set_header X-Forwarded-Host $host;`); otherwise
  `POST /api/discovery/probe` and `/scan` answer 403. A proxy that rewrites
  `Host` without forwarding the original name, and that also answers
  requests for unknown host names, defeats the rebinding check for
  `GET /api/discovery/local`: give it an explicit `server_name` and a
  `default_server` that refuses other names (nginx: `return 444;`).
- A browser `GET /api/discovery/local` without `Sec-Fetch-Site` (browsers
  older than about 2023, such as Safari before 16.4) is refused unless it
  carries an
  `X-Gos3-Discovery` header, which a cross-site page cannot send without a
  CORS preflight; non-browser clients are not affected.
- POST routes require `Content-Type: application/json`, which a cross-site
  page cannot send without a CORS preflight that the server never approves.

Probes read at most 16 KiB of an identity document (with or without
`Content-Length`), and never follow redirects or use proxies. Documents must
carry `"service": "gos3"`, a valid server ID and a `name`; strings are
cleaned of control and bidi characters and clipped, endpoints must be
http(s) URLs or paths, and at most 64 features and 64 `endpoints.all`
entries are kept (the probe route returns this sanitized document). Local
discovery keeps at most 2048 mDNS records and 1024 beacon candidates (16 per
sender), and verifies at most 256 URLs.

Nothing in an mDNS answer or beacon is authenticated, so local discovery
follows gos3's own rules ([discovery.md](discovery.md)): datagrams are
accepted only from this host or a directly connected subnet (only from
loopback when the interfaces cannot be listed), each sender's
mDNS records are kept apart, and an advertised address is used only when it
is the sender's own (or, for a sender on this host, a loopback or local
address). Beacon URLs keep only scheme and port, with the sender's address
as host; SRV target names are never resolved. When two addresses serve the
same server ID in one result, the first verified one (mDNS before beacon,
then arrival order) keeps the entry. The other address is added only when
both were observed together or each document advertises the other's
address; otherwise it is left out and, if it served a different identity
document, the peer's `error` reports the claim.
Socket errors appear in `errors` as fixed texts; details go to the server
log.

Multicast is often unavailable in containers (Kubernetes, Docker Desktop).
Scans and probes still work there.

## CORS

The browser talks to every S3 server **directly**, so each server the UI
manages from another origin must:

1. allow the UI origin, and
2. expose the response headers the UI reads.

| Header | Needed for |
|---|---|
| `ETag` | multipart uploads (otherwise the upload fails with `MissingETag`) and object details |
| `x-amz-meta-*`, `Content-Disposition`, `Content-Encoding`, `Date` | the metadata editor. The UI detects hidden headers: when a cross-origin HeadObject does not show `Date` (so not every header is exposed), the object details say "Some headers are hidden" and the metadata editor refuses to save, rather than erase metadata it cannot see. |
| `x-amz-version-id`, `x-amz-delete-marker`, `x-amz-checksum-*`, `x-amz-object-lock-*`, `Content-*`, `Last-Modified` | versions, details, checksums, retention |

**gos3**: the default `cors.expose_headers` already covers all of these
(including `*` for `x-amz-meta-*`). The default `cors.allowed_origins` is `*`,
which admits any UI (it applies to signed requests only, and the UI signs
every request; see [security.md](security.md#cors)). To restrict it, list the
UI origins:

```sh
gos3 serve --cors-origin https://console.example.com --cors-origin http://localhost:3000
# or GOS3_CORS_ORIGINS=https://console.example.com,http://localhost:3000
```

You can also change this at runtime under **Settings → CORS**. Bucket CORS
configurations override the global policy for that bucket, so a bucket with its
own rules must allow the UI origin too. Editing metadata cross-origin needs
every response header exposed, which only the global rule's `*` does: bucket
rules cannot, because `ExposeHeader` wildcards are rejected (as on AWS). For
such buckets, use the embedded UI of that server or list the headers as
described below.

New bucket CORS rules created in the UI default to `ExposeHeaders` `ETag`,
`Content-Disposition`, `Content-Encoding`, `Content-Language`,
`Content-Range`, `Accept-Ranges`, `x-amz-request-id`, `x-amz-version-id` and
`x-amz-delete-marker`.

**Other S3 services**: configure bucket CORS rules with the UI origin in
`AllowedOrigins`, the methods `GET`, `PUT`, `POST`, `DELETE` and `HEAD`,
`AllowedHeaders: ["*"]`, and the headers above in `ExposeHeaders`. AWS does not
support wildcards like `x-amz-meta-*` there. To enable the metadata editor,
list `Date` together with every `x-amz-meta-*` name you use,
`Content-Disposition` and `Content-Encoding`.

`GET /_gos3/info` always sends `Access-Control-Allow-Origin: *`, so the UI can
recognise a gos3 server, and tell "blocked by CORS" from "unreachable", before
CORS is configured. A `NetworkError` in the UI usually means that CORS blocked
the request.

## Development

```sh
make dev                    # in-memory gos3 on 127.0.0.1:9000, CORS for http://localhost:3000, prints its keys
cd web && pnpm install && pnpm dev   # http://localhost:3000
```

`web/README.md` covers the code layout, conventions and test suites (Vitest
unit tests, Playwright against mocks, against a real gos3 with the embedded UI,
and the standalone UI cross-origin).

## Screenshots

| | |
|---|---|
| ![Object browser](screenshots/browser.png) | ![Object details](screenshots/object-details.png) |
| ![Bucket settings](screenshots/bucket-settings.png) | ![Users](screenshots/users.png) |
| ![Server overview](screenshots/overview.png) | ![Discovery](screenshots/discovery.png) |
| ![Dashboard, dark](screenshots/dashboard-dark.png) | ![Object browser, dark](screenshots/browser-dark.png) |

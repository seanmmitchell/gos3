# gos3 discovery protocol

gos3 servers find each other — and the web UI finds servers — through five
mechanisms. Every mechanism only produces *candidates*; a candidate becomes a
verified peer once `GET <base>/_gos3/info` returns a valid `Info` document
with `"service": "gos3"`. Peers are keyed by `Info.id`; sightings of the same
ID from several mechanisms merge (`sources` is the union, `addresses` the
union of base URLs).

## 1. Identity document — `GET /_gos3/info`

Unauthenticated (unless `publicInfo` is disabled), `Access-Control-Allow-Origin: *`,
`Cache-Control: no-store`. Contains no secrets.

```json
{
  "service": "gos3",
  "id": "b3f1c2d4e5f60718",
  "name": "nas-gos3",
  "version": "0.1.0",
  "apiVersion": "v1",
  "region": "us-east-1",
  "endpoints": {
    "s3": "http://10.0.0.5:9000",
    "admin": "http://10.0.0.5:9000/_gos3/admin/v1",
    "ui": "http://10.0.0.5:9000/_gos3/ui/",
    "all": ["http://10.0.0.5:9000", "http://192.168.1.20:9000"]
  },
  "features": ["versioning", "object-lock", "multipart", "admin", "ui", "discovery", "lifecycle"],
  "startedAt": "2026-10-09T12:00:00Z"
}
```

The server ID is random (16 hex chars), persisted in `<data>/.gos3/server-id`
(regenerated per process for ephemeral memory-only servers).

## 2. mDNS / DNS-SD

- Service type `_gos3._tcp`, domain `local.`.
- Instance name: server name (sanitised to `[A-Za-z0-9-]`, ≤ 50 chars) + `-` + first 6 chars of ID.
- Port: the S3 listener port. Host: OS hostname `.local.` with all non-loopback IPs.
- TXT records: `id=<id>`, `name=<name>`, `v=<version>`, `api=v1`,
  `region=<region>`, `scheme=http|https`, `info=/_gos3/info`.
- Browsing: query every 30 s (3 s listen window). Candidate URL =
  `<scheme>://<ip>:<port>` for each address (IPv4 preferred).

## 3. UDP multicast beacon

Lightweight, dependency-free protocol that also works where mDNS is blocked
or unavailable, and that the standalone UI's Node server speaks too.

- Group `239.255.73.83`, port `7373` (configurable `beaconGroup`; must be an
  IPv4 multicast address), multicast TTL 1, loopback enabled. Joined on every
  multicast-capable interface; if multicast is unavailable the server logs
  once and keeps running without it.
- `beaconInterval` is 1 s – 1 h (default 10 s). Each server sends one query
  when it starts, so it learns about running peers immediately.
- Messages are UTF-8 JSON, one per datagram, ≤ 1400 bytes, field `proto` = `"gos3-beacon/1"`.
  Unknown `proto`/`type` values are ignored.

```jsonc
// Announce — sent on start, every beaconInterval (default 10 s, ±10% jitter),
// and as a unicast reply to queries.
{"proto":"gos3-beacon/1","type":"announce","id":"…","name":"…","version":"…",
 "region":"…","urls":["http://10.0.0.5:9000"],"info":"/_gos3/info","ts":1760000000}

// Query — sent by browsers of the network (servers, the UI's Node server, CLIs).
// Every server that receives it replies with an Announce *unicast* to the
// query's source address/port within 0–500 ms (random delay).
{"proto":"gos3-beacon/1","type":"query","nonce":"r4nd0m"}

// Bye — best-effort on graceful shutdown; receivers mark the peer unhealthy.
{"proto":"gos3-beacon/1","type":"bye","id":"…"}
```

Receivers ignore their own ID. Wildcard and loopback hosts in `urls` (and an
empty `urls`) are replaced by the datagram's source IP plus the advertised
port. Queriers should send from an ephemeral UDP socket and listen on it for
the unicast replies; servers do the same, so replies are never delivered to
another process bound to the group port. Unknown JSON fields are ignored so
later versions can add fields; everything else is strict (one JSON object,
valid UTF-8, ≤ 1400 bytes, exact `proto`, known `type`).

## 4. Static seeds

`discovery.seeds` is a list of base URLs (`http://host:9000`; a bare
`host[:port]` means `http://host[:port]`, and no default port is added). Each
is probed on start and every 30 s; peers found this way have source `static`.
A seed that has never answered has no server ID yet, so it does not appear in
the peer table until its first successful probe.

## 5. DNS SRV

For each domain in `discovery.dnsSrv` (at most 64), resolve
`_gos3._tcp.<domain>` SRV records every 60 s; each target/port becomes a
candidate (`https` when port is 443, else `http`); source `dns`.

In Kubernetes, a headless Service whose port is **named `gos3`** (protocol TCP)
publishes exactly these records:
`_gos3._tcp.<service>.<namespace>.svc.cluster.local` → one target per ready
pod. Set `dnsSrv` to `<service>.<namespace>.svc.cluster.local` (see
`deploy/kubernetes/`).

## 6. Active scan (on demand)

`POST /_gos3/admin/v1/discovery/scan` (and the standalone UI's local scan)
expands `targets` — CIDRs (bounded by `maxScanHosts`, network/broadcast
addresses skipped for /0–/30), host names, or base URLs — × `ports` ×
`schemes`, and probes `/_gos3/info` with `concurrency` workers and a per-probe
`timeout`. Found servers are added with source `scan`.

Target forms: a value containing `://` is a base URL probed as is (ports and
schemes ignored); a value containing `/` is a CIDR; anything else is a host
name or IP, optionally with `:port` (bracket IPv6 addresses when giving a
port). Defaults: ports `[9000]`, schemes `["http"]`, `timeout` 1.5 s per
probe (nanoseconds in JSON; clamped to 10 ms – 30 s), `concurrency` 64
(at most 512 workers are used). Limits of the admin API endpoint: at most 256
targets, 64 ports, `maxScanHosts` hosts (default 1024, at most 65536),
`timeout` ≤ 60 s and `concurrency` ≤ 1024 in the request, and 5 minutes per
scan. The server's own address is probed but left out of `found`.

## Peer lifecycle

- `sources` is the union of `mdns`, `beacon`, `static` (seeds), `dns`,
  `scan` and `probe` (admin API probe). `verified` is false for a sighting
  that has not been confirmed through `/_gos3/info` yet; treat it as a hint.
- `firstSeen` / `lastSeen` per peer; `healthy` reflects the latest probe. A
  `bye` beacon or a failed probe marks the peer unhealthy; an address that now
  answers with a different ID marks it unhealthy too.
- Probes use `GET <base>/_gos3/info` with a 5 s timeout and a 64 KiB limit;
  they never use HTTP proxies or follow redirects, and HTTPS certificates are
  verified (a peer with a self-signed certificate stays unverified with a TLS
  error).
- The table holds at most 4096 peers (unverified sightings are evicted first)
  and at most 32 addresses per peer.
- Verified peers are re-probed every 30 s; peers not seen (no sighting and no
  successful probe) for `peerTtl` are removed. Seeds are never removed.
- `GET /_gos3/admin/v1/discovery` exposes the peer table to the UI, which
  merges peer tables from every connected gos3 server plus its own local
  discovery into a single view.

## Security notes

- Beacons and mDNS are unauthenticated hints; only `/_gos3/info` (HTTP/HTTPS)
  verification promotes a candidate, and adding a discovered server to the UI
  still requires credentials.
- Scans are opt-in (`allowScan`) and require `admin:ScanNetwork`.
- Disable all advertising with `discovery.advertise: false` or the whole
  subsystem with `discovery.enabled: false`.

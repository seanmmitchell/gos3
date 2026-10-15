# gos3 discovery protocol

Discovery is opt-in. `enabled`, `advertise`, `mdns`, `beacon`, and
`allow_scan` all default to false; seeds and DNS SRV lists default to empty.
For beacon discovery, explicitly enable `discovery.enabled`,
`discovery.beacon`, and (to announce this server) `discovery.advertise`.
For seed-only discovery, enable `discovery.enabled` and populate `seeds`.
Existing explicit configuration and saved settings in managed mode retain
their values. The `gos3 discover` command explicitly requests a one-shot
search and still browses mDNS and beacons unless its disable flags are set.

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
  SRV/A records and the `scheme` TXT value describe the listener itself (its
  port and scheme at the interface IPs), never `server.public_url`, which
  may point at another host and port.
- TXT records: `id=<id>`, `name=<name>`, `v=<version>`, `api=v1`,
  `region=<region>`, `scheme=http|https`, `info=/_gos3/info`. Values are
  sent as their raw UTF-8 bytes (RFC 6763 §6.5), without any escaping, and
  read back the same way: `name=Seán` arrives as `Seán`.
- Records have a TTL of 120 s; at most 16 addresses are advertised.

### Responder

gos3 answers mDNS queries itself (no third-party mDNS library). It listens
on UDP 5353, joined to `224.0.0.251` and `ff02::fb` on every multicast
interface (either family may be unavailable; advertising stops only when
neither is). Every query is checked before it is answered:

- Datagrams from senders that are neither on this host nor on a directly
  connected subnet are dropped, as are datagrams from a broadcast, subnet
  network or unspecified source address. mDNS is link-local; the port also
  receives unicast datagrams from anywhere that can reach it. When the
  network interfaces cannot be listed (some sandboxes), only datagrams from
  this host are accepted.
- Only standard queries are read. Duplicate questions are collapsed and at
  most 8 distinct questions per query are considered.
- Only questions about this server's own records are answered: PTR for
  `_gos3._tcp.local.` (with the SRV, TXT, A and AAAA records as additional
  records) and for the DNS-SD type enumeration
  `_services._dns-sd._udp.local.`, SRV/TXT for the instance name, and A/AAAA
  for the host name. A query without such a question costs the sender
  nothing.
- Datagrams from other hosts must arrive with IP TTL / hop limit 255, as
  mDNS senders set it (RFC 6762 §11): a router decrements it, so a lower
  value means the datagram came from off the link with a spoofed on-link
  source, and it is dropped. Legacy tools that query with a lower TTL (for
  example `dig -p 5353`) therefore get answers only when run on this host.
  The TTL is read through IP control messages (`WSARecvMsg` with
  `IP_PKTINFO`/`IP_RECVTTL` on Windows). On a platform where it cannot be
  read, a warning is logged once and queries from other hosts are answered
  by multicast to the group only (never by unicast, see below); the same
  applies to any single datagram that arrives without a TTL. The browser
  drops such datagrams from other hosts.
- Each sender is rate-limited with the beacon's token bucket (a burst of 32
  answered queries, then 4 per second), and all responses together are
  limited to 50 per second. A query whose answer is not sent (the same
  multicast answer went out less than a second ago) counts against
  neither.
- Answers are multicast to the group with IP TTL / hop limit 255, out of the
  interface the query arrived on, and the same answers at most once per
  second per interface (RFC 6762 §6). Questions with the unicast-response
  bit (§5.4) and legacy queries sent from a port other than 5353 (§6.7) are
  answered by unicast to the sender, which the checks above restrict to
  this host and its directly connected subnets (and, where the TTL cannot
  be read, to this host: other hosts then get the multicast answer). Legacy replies repeat the
  query ID and question and carry TTLs of at most 10 s.
- Every response is one packet of at most 1400 bytes (additional records
  are dropped first if it would be larger).

Advertising runs whatever the S3 listen address is and is independent of
`server.public_info`. On hosts with a routable address, firewall UDP 5353
from outside the local network anyway, or set `discovery.mdns: false` (or
`discovery.advertise: false`).

### Browsing

- A PTR query for `_gos3._tcp.local.` is sent every 30 s from an ephemeral
  socket per IP family (a legacy query, so responders answer by unicast);
  answers are collected for 3 s on that socket and on the group port.
  Incomplete answers trigger follow-up SRV/TXT and A/AAAA questions (at most
  32 per browse).
- Responses from senders that are not on this host or a directly connected
  subnet are dropped, and so are responses from other hosts that arrived
  with a TTL / hop limit below 255 (forwarded by a router, so the source is
  spoofed). Records from different senders are never combined,
  and an advertised address is accepted only when it is the responding
  packet's source address, or, when the sender is this host, a loopback or
  local address. So an mDNS answer can only make the server probe the
  responder itself (like a beacon), never another host or a loopback port.
- At most 256 instances per browse, 16 per sender. Candidate URL =
  `<scheme>://<ip>:<port>` (IPv4 preferred, then IPv6 with the interface
  zone for link-local addresses); an instance needs SRV, TXT and an
  accepted address.
- Peer names have control characters (C0, DEL, C1), bidi controls and line
  or paragraph separators stripped.

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

Receivers ignore their own ID. Because nothing in a datagram is
authenticated, a server only ever probes the sender:

- Datagrams are accepted only from senders on this host or on a directly
  connected subnet (beacons never cross a router legitimately), and never
  from a broadcast or subnet network address (a reply would reach the whole
  link). When the network interfaces cannot be listed, only datagrams from
  this host are accepted.
- The host of every announced URL is replaced by the datagram's source IP,
  keeping the scheme and port. Only a sender on this host may announce
  loopback and local addresses, which are then kept as they are (so that
  several servers on one host find each other; a host that accepts packets
  from the network carrying its own source address would let an on-link
  sender name local ports too, see `could not be verified` below). Host names
  in announces are never resolved. An empty `urls` means the source IP with
  the default port.
- Announces are rate-limited per sender (a burst of 32, then 4 per second)
  and in total (a burst of 256, then 128 per second). Queries are answered
  at most 4 times in a burst, then once per second, per sender, and 20
  times per second in total. At most 1024 senders are tracked; a new one
  replaces the least recently active one.
- Unicast replies to queries are sent with TTL 1.
- A `bye` is believed only when it comes from one of the peer's known
  addresses; otherwise a verified peer is re-probed instead of being marked
  unhealthy.

Queriers should send from an ephemeral UDP socket and listen on it for
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
  `bye` beacon from one of the peer's addresses or a failed probe marks the
  peer unhealthy; an address that now answers with a different ID marks it
  unhealthy too. A failed probe of an unverified sighting never marks a
  verified peer unhealthy. `error` holds a fixed category (`connection
  refused`, `timed out`, `not a gos3 server`, `TLS certificate not trusted`,
  ...), never text the probed service sent.
- Probes use `GET <base>/_gos3/info` with a 5 s timeout and a 64 KiB limit;
  they never use HTTP proxies or follow redirects, and HTTPS certificates are
  verified (a peer with a self-signed certificate stays unverified with a TLS
  error). A failed check of a beacon or mDNS sighting that named a loopback
  or local address of this host is recorded only as `could not be
  verified`, so the table reveals nothing about local ports.
- A stored identity document is bounded: `name`, `version` and `region` at
  most 256 bytes, endpoint URLs at most 512 bytes (longer ones are
  dropped), `endpoints.all` at most 16 base URLs, `features` at most 32
  entries of 64 bytes, and all strings together at most 4 KiB (trailing
  `endpoints.all`, then `features` entries are dropped to fit).
- Probes never connect to cloud metadata addresses (`169.254.169.254`,
  `169.254.170.2`, `169.254.170.23`, `169.254.0.23`, `168.63.129.16`,
  `192.0.0.192`, `fd00:ec2::254`, `fd00:ec2::23`, `fd20:ce::254`,
  `100.100.100.200`),
  unspecified addresses (`0.0.0.0/8`, `::`), multicast addresses or
  `255.255.255.255`, also in IPv4-mapped, IPv4-compatible or NAT64 (`64:ff9b::/96`,
  `64:ff9b:1::/48`) form. The
  check runs on the resolved address of every connection attempt, so a host
  name that resolves there is refused too (`address not allowed`). This
  applies to the admin API probe, scans, seeds, DNS SRV targets, candidate
  verification and re-probes. Other link-local addresses stay allowed:
  zeroconf networks use them.

### Peer identity

Server IDs are not authenticated: any server can serve any ID. The first
verified record of an ID is kept while the peer still answers:

- A verified peer's *known addresses* are the addresses where its
  `/_gos3/info` was fetched. Unverified sightings never add to them, and
  when an unverified entry is verified its sighting addresses are replaced
  by the verified one.
- When a server at a new address answers with the ID of a verified peer
  (from the admin probe, a scan, a beacon or an mDNS sighting), nothing
  changes yet: the peer's known addresses are re-probed first (at most 4;
  not at all if one answered within the last 10 s). If one still answers
  with the ID, the verified record (name, version, region, endpoints,
  features, addresses, re-probe address) is kept and the claim is recorded:
  `error` reads `ID claimed by another address: <url> ...` for 10 minutes
  and a warning is logged. A claim whose document is identical to the
  verified one (the same server reached at another address) is ignored.
  The admin API probe and scans still report what the new address serves.
  If the re-probes are cut short before they are decided (the admin API
  probe's deadline ends while a known address is still being re-probed),
  nothing is recorded: neither a conflict nor a move.
- Only when no known address answers with the ID any more does the peer
  move: the new document replaces the record and the new address becomes
  its only address.
- Without a re-probe, a new address is accepted when the verified record
  itself advertises it (`endpoints.s3` or `endpoints.all`, e.g. the other
  interfaces of a multi-homed server), when it is a configured seed, or
  when it comes from a DNS SRV lookup of a configured domain.
- The table holds at most 1024 peers with at most 16 addresses each, so it
  (and `GET /_gos3/admin/v1/discovery`) stays a few MiB at most. Within it:
  - at most 256 unverified sightings, and at most 16 per sender address
    (an IPv4 address or an IPv6 /64); a new sighting replaces the stalest
    one of its sender, else the stalest one overall, and never a verified
    peer;
  - at most 512 verified peers known only from beacon or mDNS
    announcements, and at most 16 per sender address (one host can serve
    any number of IDs); further ones are not added;
  - peers found through seeds, DNS SRV, the admin probe or a scan are not
    limited by these shares. When the table is full, such a peer replaces
    the stalest unverified sighting, else the stalest peer known only from
    announcements.
  A scan result is bounded the same way (at most 1024 peers).
- Verified peers are re-probed every 30 s (twice per `peerTtl` when that is
  shorter). Unverified sightings not seen again, and verified peers without
  a successful probe, for `peerTtl` are removed: an announcement never
  keeps a verified peer alive, since anyone can announce any ID. Seeds are
  never removed.
- `GET /_gos3/admin/v1/discovery` exposes the peer table to the UI, which
  merges peer tables from every connected gos3 server plus its own local
  discovery into a single view. Records with the same ID are merged only
  when they describe the same host (they share an observed address, or
  each advertises the other's); otherwise the UI flags an "ID conflict"
  and never reuses saved credentials for it. "Add as server" prefills the
  observed (verified) address; the advertised endpoint is shown only as an
  unverified hint.

## Security notes

- Beacons and mDNS are unauthenticated hints; only `/_gos3/info` (HTTP/HTTPS)
  verification promotes a candidate, and adding a discovered server to the UI
  still requires credentials. Both accept datagrams only from this host and
  directly connected subnets, rate-limit each sender, and only make the
  server probe the sender itself.
- A verified peer cannot be taken over by another server claiming its ID
  while it still answers (see [Peer identity](#peer-identity)).
- Scans are opt-in (`allowScan`) and require `admin:ScanNetwork`.
- Disable all advertising with `discovery.advertise: false` or the whole
  subsystem with `discovery.enabled: false`. `publicInfo: false` only
  protects `/_gos3/info`: beacon and mDNS announcements still carry the ID,
  name, version, region and URLs unless `advertise` is off too.

package discovery

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bounds that keep the peer table small no matter what the network sends.
// Every field of an entry is bounded (the stored identity document by
// maxStoredInfo, the addresses by maxAddressesPerPeer normalised base
// URLs), so the entry counts below bound the table's memory and the size
// of GET /_gos3/admin/v1/discovery: about 10 KiB per entry at most.
const (
	maxPeers = 1024
	// maxUnverifiedPeers bounds the entries created from sightings that no
	// /_gos3/info probe confirmed yet (anyone on the link can announce).
	maxUnverifiedPeers = 256
	// maxAutoPeers bounds the verified peers known only from
	// unauthenticated announcements (beacon, mDNS): one host can serve any
	// number of IDs, so these never take the room of peers found through
	// seeds, DNS SRV, the admin probe or scans.
	maxAutoPeers = 512
	// maxPeersPerHost bounds the unverified entries, and separately the
	// verified peers known only from announcements, charged to one sender
	// (an IPv4 address or an IPv6 /64).
	maxPeersPerHost     = 16
	maxAddressesPerPeer = 16
	maxIDLen            = 64
	maxFieldLen         = 256
	maxFeatures         = 32
	maxFeatureLen       = 64
	maxEndpointsAll     = 16
	// maxEndpointLen bounds each endpoint URL; longer ones are dropped.
	maxEndpointLen = 512
	// maxStoredInfo bounds the strings of a stored identity document;
	// trailing endpoints.all and features entries are dropped to fit.
	maxStoredInfo = 4 << 10
	maxErrorLen   = 512
)

// validID reports whether id is a plausible server ID: 1-64 characters of
// [A-Za-z0-9._-]. gos3 itself uses 16 hex characters.
func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// cleanField makes an untrusted string safe to store and display: valid
// UTF-8, no control characters, trimmed and at most n bytes.
func cleanField(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = strings.Map(func(r rune) rune {
		// C0, DEL and C1 controls (e.g. U+009B, an 8-bit CSI), bidi
		// overrides and isolates, and line/paragraph separators: they
		// would let a peer spoof names or drive the operator's terminal.
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), n)
}

// sanitizeInfo bounds and cleans every field of an Info document received
// from the network. It assumes Service and ID were validated.
// Endpoints.All keeps only base URLs (see NormalizeBaseURL), and the
// document's strings together stay within maxStoredInfo bytes.
func sanitizeInfo(in Info) Info {
	out := Info{
		Service:    in.Service,
		ID:         in.ID,
		Name:       cleanField(in.Name, maxFieldLen),
		Version:    cleanField(in.Version, maxFieldLen),
		APIVersion: cleanField(in.APIVersion, 32),
		Region:     cleanField(in.Region, maxFieldLen),
		StartedAt:  in.StartedAt,
		Endpoints: Endpoints{
			S3:    cleanEndpoint(in.Endpoints.S3),
			Admin: cleanEndpoint(in.Endpoints.Admin),
			UI:    cleanEndpoint(in.Endpoints.UI),
		},
	}
	for _, a := range in.Endpoints.All {
		if len(out.Endpoints.All) >= maxEndpointsAll {
			break
		}
		if a = cleanEndpoint(a); a != "" {
			if _, err := NormalizeBaseURL(a); err == nil {
				out.Endpoints.All = append(out.Endpoints.All, a)
			}
		}
	}
	for _, f := range in.Features {
		if len(out.Features) >= maxFeatures {
			break
		}
		if f = cleanField(f, maxFeatureLen); f != "" {
			out.Features = append(out.Features, f)
		}
	}
	// The scalar fields alone are well below maxStoredInfo.
	for infoSize(out) > maxStoredInfo {
		if n := len(out.Endpoints.All); n > 0 {
			out.Endpoints.All = out.Endpoints.All[:n-1]
		} else if n := len(out.Features); n > 0 {
			out.Features = out.Features[:n-1]
		} else {
			break
		}
	}
	return out
}

// cleanEndpoint cleans an endpoint URL like cleanField; a URL longer than
// maxEndpointLen is dropped rather than cut into a different URL.
func cleanEndpoint(s string) string {
	if s = cleanField(s, maxURLLen); len(s) > maxEndpointLen {
		return ""
	}
	return s
}

// infoSize returns the total length of the strings of info.
func infoSize(info Info) int {
	n := len(info.Service) + len(info.ID) + len(info.Name) + len(info.Version) + len(info.APIVersion) +
		len(info.Region) + len(info.Endpoints.S3) + len(info.Endpoints.Admin) + len(info.Endpoints.UI)
	for _, a := range info.Endpoints.All {
		n += len(a)
	}
	for _, f := range info.Features {
		n += len(f)
	}
	return n
}

// candidate is an unverified sighting of a (possible) peer produced by a
// discovery mechanism. id and the descriptive fields are empty when the
// mechanism does not know them (seeds, DNS SRV); urls are normalised base
// URLs in order of preference.
type candidate struct {
	id, name, version, region string
	source                    Source
	urls                      []string
}

// peerEntry is one row of the peer table.
type peerEntry struct {
	peer      Peer
	info      Info      // last verified identity document
	lastProbe time.Time // last /_gos3/info probe (success or failure)
	probeURL  string    // last address that answered a probe
	// host is the sender the entry counts against for maxPeersPerHost
	// (see hostKey): the sighting's first address while unverified, then
	// the address that answered the last probe.
	host netip.Prefix

	// Claims of this (verified) peer's ID by another address while its
	// known addresses still answered (see Service.record).
	conflicts    int
	conflictAt   time.Time
	conflictAddr string
}

// Identity conflicts (docs/discovery.md, "Peer identity").
const (
	// conflictHold is how long a recorded conflict stays in Peer.Error.
	conflictHold = 10 * time.Minute
	// conflictFresh: a verified peer that answered a probe at a known
	// address this recently is not re-probed before a claim from another
	// address is rejected.
	conflictFresh = 10 * time.Second
	// maxConflictReprobes bounds the known addresses re-probed per claim.
	maxConflictReprobes = 4
)

// observeResult says what observeInfo did with a verified Info document.
type observeResult int

const (
	// observeRejected: our own ID, an invalid ID, or no room (the table or
	// the sender's share of it is full).
	observeRejected observeResult = iota
	// observeStored: the peer was added or refreshed.
	observeStored
	// observeConflict: a verified peer with this ID is known at other
	// addresses; nothing was changed.
	observeConflict
)

// peerTable is the set of known peers keyed by server ID. It is safe for
// concurrent use; all methods take the current time explicitly so that tests
// can use a fake clock.
type peerTable struct {
	mu         sync.Mutex
	peers      map[string]*peerEntry
	unverified int // entries with !peer.Verified
	ttl        time.Duration
	seeds      map[string]struct{} // normalised seed URLs: peers seen there never expire
}

func newPeerTable(ttl time.Duration) *peerTable {
	return &peerTable{peers: make(map[string]*peerEntry), ttl: ttl, seeds: map[string]struct{}{}}
}

// configure updates the TTL and the set of seed URLs.
func (t *peerTable) configure(ttl time.Duration, seeds []string) {
	set := make(map[string]struct{}, len(seeds))
	for _, s := range seeds {
		set[s] = struct{}{}
	}
	t.mu.Lock()
	t.ttl = ttl
	t.seeds = set
	t.mu.Unlock()
}

// pinnedLocked reports whether e was observed at a configured seed URL.
func (t *peerTable) pinnedLocked(e *peerEntry) bool {
	if slices.Contains(e.peer.Sources, SourceStatic) {
		for _, a := range e.peer.Addresses {
			if _, ok := t.seeds[a]; ok {
				return true
			}
		}
	}
	return false
}

// trustedSource reports whether s finds peers on the operator's behalf
// (seeds, DNS SRV, the admin probe and scans) rather than through
// unauthenticated announcements (beacon, mDNS).
func trustedSource(s Source) bool {
	switch s {
	case SourceStatic, SourceDNS, SourceProbe, SourceScan:
		return true
	}
	return false
}

// trusted reports whether e was found through a trusted source.
func (e *peerEntry) trusted() bool { return slices.ContainsFunc(e.peer.Sources, trustedSource) }

// auto reports whether e is a verified peer known only from announcements.
func (e *peerEntry) auto() bool { return e.peer.Verified && !e.trusted() }

// hostKey returns the sender a base URL with an IP literal host is charged
// to: the IPv4 address, or the /64 of an IPv6 address (one host can use a
// whole /64). URLs with host names (which only trusted sources produce)
// yield the zero Prefix and are not charged.
func hostKey(addr string) netip.Prefix {
	_, host, _, ok := urlHostPort(addr)
	if !ok {
		return netip.Prefix{}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}
	}
	ip = ip.WithZone("").Unmap()
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	p, _ := ip.Prefix(bits)
	return p
}

// countLocked returns the number of entries accepted by pred.
func (t *peerTable) countLocked(pred func(*peerEntry) bool) int {
	n := 0
	for _, e := range t.peers {
		if pred(e) {
			n++
		}
	}
	return n
}

// admitUnverifiedLocked makes room for a new unverified entry charged to
// host. Unverified entries only ever displace unverified entries: the
// stalest one of the same host when it has maxPeersPerHost of them, else
// the stalest one overall when there are maxUnverifiedPeers or the table
// is full. It reports whether there is room.
func (t *peerTable) admitUnverifiedLocked(host netip.Prefix) bool {
	unverified := func(e *peerEntry) bool { return !e.peer.Verified }
	sameHost := func(e *peerEntry) bool { return unverified(e) && e.host == host }
	if host.IsValid() && t.countLocked(sameHost) >= maxPeersPerHost {
		return t.evictLocked(sameHost)
	}
	if t.unverified >= maxUnverifiedPeers || len(t.peers) >= maxPeers {
		return t.evictLocked(unverified)
	}
	return true
}

// admitVerifiedLocked reports whether a document fetched from addr through
// src may be stored as a verified peer, either in the existing entry e or,
// when e is nil, in a new one, making room for it when the table is full.
// A peer that becomes verified while known only from announcements counts
// against maxAutoPeers and against maxPeersPerHost for addr's host, and a
// new one only displaces unverified entries. A peer found through a
// trusted source may also displace the stalest of those verified peers.
// Entries at seed URLs are never displaced.
func (t *peerTable) admitVerifiedLocked(e *peerEntry, addr string, src Source) bool {
	trusted := trustedSource(src) || (e != nil && e.trusted())
	if !trusted && (e == nil || !e.peer.Verified) {
		if t.countLocked((*peerEntry).auto) >= maxAutoPeers {
			return false
		}
		host := hostKey(addr)
		if host.IsValid() && t.countLocked(func(o *peerEntry) bool { return o.auto() && o.host == host }) >= maxPeersPerHost {
			return false
		}
	}
	if e != nil || len(t.peers) < maxPeers {
		return true
	}
	return t.evictLocked(func(o *peerEntry) bool { return !o.peer.Verified || (trusted && o.auto()) })
}

// insertLocked adds the new entry e; the caller made room for it.
func (t *peerTable) insertLocked(id string, e *peerEntry) {
	t.peers[id] = e
	if !e.peer.Verified {
		t.unverified++
	}
}

// evictLocked removes the stalest non-seed entry accepted by eligible,
// preferring unverified entries. It reports whether one was removed.
func (t *peerTable) evictLocked(eligible func(*peerEntry) bool) bool {
	var victim string
	var victimEntry *peerEntry
	for vid, ve := range t.peers {
		if t.pinnedLocked(ve) || !eligible(ve) {
			continue
		}
		if victimEntry == nil ||
			(victimEntry.peer.Verified && !ve.peer.Verified) ||
			(victimEntry.peer.Verified == ve.peer.Verified && ve.peer.LastSeen.Before(victimEntry.peer.LastSeen)) {
			victim, victimEntry = vid, ve
		}
	}
	if victimEntry == nil {
		return false
	}
	t.deleteLocked(victim)
	return true
}

// deleteLocked removes entry id.
func (t *peerTable) deleteLocked(id string) {
	if e, ok := t.peers[id]; ok {
		if !e.peer.Verified {
			t.unverified--
		}
		delete(t.peers, id)
	}
}

// observeInfo records a successful /_gos3/info probe of addr. src is added to
// the peer's sources unless empty (re-probes).
//
// Server IDs are not authenticated, so a document for an already verified
// peer fetched from an address the peer is not known at changes nothing:
// the result is observeConflict and the caller decides (Service.record
// re-probes the known addresses, then calls conflict or move). Exceptions,
// stored as usual: addresses the verified record itself advertises
// (Endpoints.S3 / Endpoints.All), configured seeds and DNS SRV targets.
//
// It returns the updated peer (observeStored), the unchanged verified peer
// (observeConflict) or nothing (observeRejected).
func (t *peerTable) observeInfo(info Info, addr string, src Source, now time.Time, selfID string) (Peer, observeResult) {
	if !validID(info.ID) || info.ID == selfID {
		return Peer{}, observeRejected
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[info.ID]
	if ok && e.peer.Verified && !slices.Contains(e.peer.Addresses, addr) && !t.vouchedLocked(e, addr, src) {
		return clonePeer(e.peer), observeConflict
	}
	if !ok {
		e = nil
	}
	if !t.admitVerifiedLocked(e, addr, src) {
		return Peer{}, observeRejected
	}
	if e == nil {
		e = &peerEntry{peer: Peer{ID: info.ID, FirstSeen: now}}
		t.insertLocked(info.ID, e)
	}
	t.storeLocked(e, info, addr, src, now)
	return clonePeer(e.peer), observeStored
}

// vouchedLocked reports whether a verified peer may be observed at the new
// address addr without a conflict check: the peer's own verified document
// advertises addr, or the operator configured it (a seed or DNS SRV).
func (t *peerTable) vouchedLocked(e *peerEntry, addr string, src Source) bool {
	switch src {
	case SourceDNS:
		return true
	case SourceStatic:
		if _, ok := t.seeds[addr]; ok {
			return true
		}
	}
	for _, u := range append([]string{e.peer.Endpoints.S3}, e.peer.Endpoints.All...) {
		if n, err := NormalizeBaseURL(u); err == nil && n == addr {
			return true
		}
	}
	return false
}

// storeLocked writes a verified document fetched from addr into e. The
// addresses of an entry that was not verified yet came from unauthenticated
// sightings and are replaced by addr. This (a successful probe) is the only
// thing that refreshes a verified peer's LastSeen.
func (t *peerTable) storeLocked(e *peerEntry, info Info, addr string, src Source, now time.Time) {
	p := &e.peer
	if !p.Verified {
		t.unverified--
		p.Addresses = nil
	}
	// The record and the peer share the (never modified in place) lists.
	e.info = info
	e.info.Endpoints = cloneEndpoints(info.Endpoints)
	e.info.Features = slices.Clone(info.Features)
	p.Name = info.Name
	p.Version = info.Version
	p.Region = info.Region
	p.Endpoints = e.info.Endpoints
	p.Features = e.info.Features
	if src != "" {
		addSource(p, src)
	}
	addAddress(p, addr)
	p.LastSeen = now
	p.Healthy = true
	p.Verified = true
	p.Error = ""
	if !e.conflictAt.IsZero() && now.Sub(e.conflictAt) < conflictHold {
		p.Error = conflictText(e.conflictAddr, e.conflicts)
	}
	e.lastProbe = now
	e.probeURL = addr
	e.host = hostKey(addr)
}

func conflictText(addr string, n int) string {
	msg := "ID claimed by another address: " + addr + " served this server ID while the known addresses still answered"
	if n > 1 {
		msg += fmt.Sprintf(" (%d claims)", n)
	}
	return cleanField(msg, maxErrorLen)
}

// knownTargets returns the known addresses of verified peer id to re-probe
// before a claim of its ID from another address is decided (last good
// address first, at most maxConflictReprobes), or alive=true when the peer
// answered at a known address within fresh, so no re-probe is needed.
func (t *peerTable) knownTargets(id string, now time.Time, fresh time.Duration) (urls []string, alive bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[id]
	if !ok || !e.peer.Verified {
		return nil, false
	}
	if e.peer.Healthy && !e.lastProbe.IsZero() && now.Sub(e.lastProbe) < fresh && !now.Before(e.lastProbe) {
		return nil, true
	}
	if e.probeURL != "" && slices.Contains(e.peer.Addresses, e.probeURL) {
		urls = append(urls, e.probeURL)
	}
	for _, a := range slices.Backward(e.peer.Addresses) {
		if len(urls) >= maxConflictReprobes {
			break
		}
		if a != e.probeURL {
			urls = append(urls, a)
		}
	}
	return urls, false
}

// conflict records that addr served info, claiming verified peer info.ID,
// while the peer still answered at a known address. The verified record is
// kept. A document identical to the verified one (the same server reached
// at another address) is not a conflict. It reports whether this is the
// first conflict for the peer within conflictHold (worth a warning).
func (t *peerTable) conflict(info Info, addr string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[info.ID]
	if !ok || !e.peer.Verified || sameInfo(e.info, info) {
		return false
	}
	first := e.conflictAt.IsZero() || now.Sub(e.conflictAt) >= conflictHold
	e.conflicts++
	e.conflictAt = now
	e.conflictAddr = addr
	e.peer.Error = conflictText(addr, e.conflicts)
	return first
}

// move stores info fetched from addr as peer info.ID after its known
// addresses stopped answering with that ID: the old addresses are dropped
// and addr becomes the peer's only address.
func (t *peerTable) move(info Info, addr string, src Source, now time.Time, selfID string) (Peer, bool) {
	if !validID(info.ID) || info.ID == selfID {
		return Peer{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[info.ID]
	if !ok {
		e = nil
	}
	if !t.admitVerifiedLocked(e, addr, src) {
		return Peer{}, false
	}
	if e == nil {
		e = &peerEntry{peer: Peer{ID: info.ID, FirstSeen: now}}
		t.insertLocked(info.ID, e)
	}
	e.peer.Addresses = nil
	e.conflicts, e.conflictAt, e.conflictAddr = 0, time.Time{}, ""
	t.storeLocked(e, info, addr, src, now)
	return clonePeer(e.peer), true
}

// sameInfo reports whether two verified documents describe the server
// identically.
func sameInfo(a, b Info) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Version == b.Version &&
		a.APIVersion == b.APIVersion && a.Region == b.Region && a.StartedAt.Equal(b.StartedAt) &&
		a.Endpoints.S3 == b.Endpoints.S3 && a.Endpoints.Admin == b.Endpoints.Admin &&
		a.Endpoints.UI == b.Endpoints.UI && slices.Equal(a.Endpoints.All, b.Endpoints.All) &&
		slices.Equal(a.Features, b.Features)
}

// hint records an unverified sighting (c.id must be set). It creates an
// unverified entry for unknown IDs and refreshes unverified ones. It reports
// whether the sighting's addresses should be probed: always for unverified
// or unhealthy peers, and for verified peers only when an address is new.
//
// Anyone can announce any ID, so a sighting never refreshes a verified
// peer: its LastSeen moves only with successful probes (storeLocked), and
// a peer that stops answering expires even while its ID is announced. The
// sighting's mechanism is added to the peer's sources only when it points
// at one of the peer's known addresses.
func (t *peerTable) hint(h candidate, now time.Time, selfID string) bool {
	if !validID(h.id) || h.id == selfID {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[h.id]
	if !ok {
		var host netip.Prefix
		if len(h.urls) > 0 {
			host = hostKey(h.urls[0])
		}
		if !t.admitUnverifiedLocked(host) {
			return false
		}
		e = &peerEntry{peer: Peer{
			ID:        h.id,
			Name:      cleanField(h.name, maxFieldLen),
			Version:   cleanField(h.version, maxFieldLen),
			Region:    cleanField(h.region, maxFieldLen),
			FirstSeen: now,
			Healthy:   true,
		}, host: host}
		t.insertLocked(h.id, e)
	}
	p := &e.peer
	if !p.Verified {
		addSource(p, h.source)
		p.LastSeen = now
		if h.name != "" {
			p.Name = cleanField(h.name, maxFieldLen)
		}
		if h.version != "" {
			p.Version = cleanField(h.version, maxFieldLen)
		}
		if h.region != "" {
			p.Region = cleanField(h.region, maxFieldLen)
		}
		for _, a := range h.urls {
			addAddress(p, a)
		}
		return true
	}
	newAddr := false
	for _, a := range h.urls {
		if slices.Contains(p.Addresses, a) {
			addSource(p, h.source)
		} else {
			newAddr = true
		}
	}
	return !p.Healthy || newAddr
}

// probeFailed marks peer id unhealthy with err.
func (t *peerTable) probeFailed(id, errMsg string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.peers[id]; ok {
		e.peer.Healthy = false
		e.peer.Error = cleanField(errMsg, maxErrorLen)
		e.lastProbe = now
	}
}

// sightingFailed records that the addresses of an unverified sighting of id
// could not be verified. Sightings are not authenticated, so a verified
// peer is left alone (it is re-probed at its own addresses); only
// unverified entries are marked unhealthy.
func (t *peerTable) sightingFailed(id, errMsg string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.peers[id]; ok && !e.peer.Verified {
		e.peer.Healthy = false
		e.peer.Error = cleanField(errMsg, maxErrorLen)
		e.lastProbe = now
	}
}

// bye handles a shutdown announcement for peer id. It is believed (the
// peer is marked unhealthy) only when fromPeer accepts one of the peer's
// addresses, i.e. the datagram came from where the peer is known to be;
// otherwise a verified peer is merely made due for a re-probe.
func (t *peerTable) bye(id string, fromPeer func(addr string) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[id]
	if !ok {
		return
	}
	if slices.ContainsFunc(e.peer.Addresses, fromPeer) {
		e.peer.Healthy = false
		e.peer.Error = "server announced shutdown"
		return
	}
	e.lastProbe = time.Time{}
}

// findByAddr returns the IDs of peers observed at addr.
func (t *peerTable) findByAddr(addr string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for id, e := range t.peers {
		if slices.Contains(e.peer.Addresses, addr) {
			ids = append(ids, id)
		}
	}
	return ids
}

// expire removes peers not seen for the TTL, except peers at seed URLs.
func (t *peerTable) expire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, e := range t.peers {
		if now.Sub(e.peer.LastSeen) > t.ttl && !t.pinnedLocked(e) {
			t.deleteLocked(id)
		}
	}
}

// probeTarget is a verified peer due for a re-probe.
type probeTarget struct {
	id   string
	urls []string // last good address first
}

// reprobeInterval is how often verified peers are re-probed: every
// reprobeEvery, or twice per TTL when that is shorter, so that a live peer
// is re-verified (refreshing LastSeen) before it would expire.
func (t *peerTable) reprobeInterval() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ttl <= 0 {
		return reprobeEvery
	}
	return min(reprobeEvery, t.ttl/2)
}

// due returns verified peers whose last probe is at least every ago.
func (t *peerTable) due(now time.Time, every time.Duration) []probeTarget {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []probeTarget
	for id, e := range t.peers {
		if !e.peer.Verified || now.Sub(e.lastProbe) < every {
			continue
		}
		urls := make([]string, 0, len(e.peer.Addresses))
		if e.probeURL != "" {
			urls = append(urls, e.probeURL)
		}
		for _, a := range e.peer.Addresses {
			if a != e.probeURL {
				urls = append(urls, a)
			}
		}
		out = append(out, probeTarget{id: id, urls: urls})
	}
	slices.SortFunc(out, func(a, b probeTarget) int { return cmp.Compare(a.id, b.id) })
	return out
}

// get returns a copy of peer id.
func (t *peerTable) get(id string) (Peer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[id]
	if !ok {
		return Peer{}, false
	}
	return clonePeer(e.peer), true
}

// snapshot returns copies of all peers except selfID, sorted by Name then ID.
func (t *peerTable) snapshot(selfID string) []Peer {
	t.mu.Lock()
	out := make([]Peer, 0, len(t.peers))
	for id, e := range t.peers {
		if id != selfID {
			out = append(out, clonePeer(e.peer))
		}
	}
	t.mu.Unlock()
	sortPeers(out)
	return out
}

// sortPeers sorts by Name then ID.
func sortPeers(ps []Peer) {
	slices.SortFunc(ps, func(a, b Peer) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func addSource(p *Peer, s Source) {
	if s != "" && !slices.Contains(p.Sources, s) {
		p.Sources = append(p.Sources, s)
	}
}

// addAddress appends addr, dropping the oldest address when the list is full.
func addAddress(p *Peer, addr string) {
	if addr == "" || slices.Contains(p.Addresses, addr) {
		return
	}
	if len(p.Addresses) >= maxAddressesPerPeer {
		p.Addresses = slices.Delete(p.Addresses, 0, 1)
	}
	p.Addresses = append(p.Addresses, addr)
}

func cloneEndpoints(e Endpoints) Endpoints {
	e.All = slices.Clone(e.All)
	return e
}

// clonePeer deep-copies p; Sources and Addresses are never nil so they
// encode as JSON arrays.
func clonePeer(p Peer) Peer {
	p.Endpoints = cloneEndpoints(p.Endpoints)
	p.Features = slices.Clone(p.Features)
	p.Sources = append(make([]Source, 0, len(p.Sources)), p.Sources...)
	p.Addresses = append(make([]string, 0, len(p.Addresses)), p.Addresses...)
	return p
}

// peerFromInfo builds a Peer from a verified Info document.
func peerFromInfo(info Info, addr string, src Source, now time.Time) Peer {
	p := Peer{
		ID:        info.ID,
		Name:      info.Name,
		Version:   info.Version,
		Region:    info.Region,
		Endpoints: cloneEndpoints(info.Endpoints),
		Features:  slices.Clone(info.Features),
		FirstSeen: now,
		LastSeen:  now,
		Healthy:   true,
		Verified:  true,
	}
	addSource(&p, src)
	addAddress(&p, addr)
	return clonePeer(p)
}

package discovery

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bounds that keep the peer table small no matter what the network sends.
const (
	maxPeers = 4096
	// maxUnverifiedPeers bounds the entries created from sightings that no
	// /_gos3/info probe confirmed yet (anyone on the link can announce).
	maxUnverifiedPeers  = 256
	maxAddressesPerPeer = 32
	maxIDLen            = 64
	maxFieldLen         = 256
	maxFeatures         = 64
	maxEndpointsAll     = 64
	maxErrorLen         = 512
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
			S3:    cleanField(in.Endpoints.S3, maxURLLen),
			Admin: cleanField(in.Endpoints.Admin, maxURLLen),
			UI:    cleanField(in.Endpoints.UI, maxURLLen),
		},
	}
	for _, a := range in.Endpoints.All {
		if len(out.Endpoints.All) >= maxEndpointsAll {
			break
		}
		if a = cleanField(a, maxURLLen); a != "" {
			out.Endpoints.All = append(out.Endpoints.All, a)
		}
	}
	for _, f := range in.Features {
		if len(out.Features) >= maxFeatures {
			break
		}
		if f = cleanField(f, 64); f != "" {
			out.Features = append(out.Features, f)
		}
	}
	return out
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
	// observeRejected: our own ID, an invalid ID, or the table is full.
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

// insertLocked adds a new entry, evicting the stalest non-seed entry
// (unverified ones first) when the table is full. Unverified entries are
// further bounded by maxUnverifiedPeers: a new one replaces the stalest
// unverified entry. It reports whether the entry was added.
func (t *peerTable) insertLocked(id string, e *peerEntry) bool {
	if !e.peer.Verified && t.unverified >= maxUnverifiedPeers {
		if !t.evictLocked(func(ve *peerEntry) bool { return !ve.peer.Verified }) {
			return false
		}
	}
	if len(t.peers) >= maxPeers {
		if !t.evictLocked(func(ve *peerEntry) bool { return !ve.peer.Verified || e.peer.Verified }) {
			return false
		}
	}
	t.peers[id] = e
	if !e.peer.Verified {
		t.unverified++
	}
	return true
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
		e = &peerEntry{peer: Peer{ID: info.ID, FirstSeen: now}}
		if !t.insertLocked(info.ID, e) {
			return Peer{}, observeRejected
		}
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
// sightings and are replaced by addr.
func (t *peerTable) storeLocked(e *peerEntry, info Info, addr string, src Source, now time.Time) {
	p := &e.peer
	if !p.Verified {
		t.unverified--
		p.Addresses = nil
	}
	e.info = info
	e.info.Endpoints = cloneEndpoints(info.Endpoints)
	e.info.Features = slices.Clone(info.Features)
	p.Name = info.Name
	p.Version = info.Version
	p.Region = info.Region
	p.Endpoints = cloneEndpoints(info.Endpoints)
	p.Features = slices.Clone(info.Features)
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
		e = &peerEntry{peer: Peer{ID: info.ID, FirstSeen: now}}
		if !t.insertLocked(info.ID, e) {
			return Peer{}, false
		}
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

// hint records an unverified sighting (c.id must be set). It creates an unverified entry for
// unknown IDs and refreshes LastSeen of known ones. It reports whether the
// sighting's addresses should be probed: always for unverified or unhealthy
// peers, and for verified peers only when an address is new.
func (t *peerTable) hint(h candidate, now time.Time, selfID string) bool {
	if !validID(h.id) || h.id == selfID {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.peers[h.id]
	if !ok {
		e = &peerEntry{peer: Peer{
			ID:        h.id,
			Name:      cleanField(h.name, maxFieldLen),
			Version:   cleanField(h.version, maxFieldLen),
			Region:    cleanField(h.region, maxFieldLen),
			FirstSeen: now,
			Healthy:   true,
		}}
		if !t.insertLocked(h.id, e) {
			return false
		}
	}
	p := &e.peer
	addSource(p, h.source)
	p.LastSeen = now
	if !p.Verified {
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
	if !p.Healthy {
		return true
	}
	for _, a := range h.urls {
		if !slices.Contains(p.Addresses, a) {
			return true
		}
	}
	return false
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

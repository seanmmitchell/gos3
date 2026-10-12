package discovery

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// Internal timing and sizing.
const (
	probeQueueSize     = 256
	probeWorkers       = 8
	reprobeConcurrency = 8
	seedConcurrency    = 8
	maintainEvery      = 5 * time.Second
	reprobeEvery       = 30 * time.Second
	seedEvery          = 30 * time.Second
	dnsEvery           = 60 * time.Second
	dnsLookupTimeout   = 5 * time.Second
	maxSRVRecords      = 64
	candidateThrottle  = 10 * time.Second
	maxThrottleEntries = 4096
)

// Mechanism names in start order.
const (
	mechCore       = "core"
	mechBeacon     = "beacon"
	mechMDNSAdvert = "mdns-advertise"
	mechMDNSBrowse = "mdns-browse"
	mechSeeds      = "seeds"
	mechDNSSRV     = "dns-srv"
)

var mechOrder = []string{mechCore, mechBeacon, mechMDNSAdvert, mechMDNSBrowse, mechSeeds, mechDNSSRV}

// mechanism is one running discovery mechanism.
type mechanism struct {
	key    string // settings the mechanism depends on
	cancel context.CancelFunc
	done   chan struct{}
}

// mechSpec describes a mechanism for a configuration. setup runs
// synchronously (opening sockets etc.) and returns the blocking run function.
type mechSpec struct {
	key   string
	setup func(ctx context.Context) (run func(), err error)
}

// plan returns the mechanisms e needs.
func (s *Service) plan(e effective) map[string]mechSpec {
	out := make(map[string]mechSpec)
	if !e.Enabled {
		return out
	}
	out[mechCore] = mechSpec{key: "on", setup: func(ctx context.Context) (func(), error) {
		return func() { s.runCore(ctx) }, nil
	}}
	if e.Beacon {
		out[mechBeacon] = mechSpec{
			key: fmt.Sprintf("%s|%s|%t", e.group, e.interval, e.Advertise),
			setup: func(ctx context.Context) (func(), error) {
				b, err := s.openBeacon(e.group, beaconOptions{
					interval:  e.interval,
					advertise: e.Advertise,
					query:     true,
					selfInfo:  s.selfInfo,
					sink:      s,
					logger:    s.logger,
					now:       s.now,
				})
				if err != nil {
					return nil, err
				}
				return func() { b.run(ctx) }, nil
			},
		}
	}
	if e.MDNS && e.Advertise {
		out[mechMDNSAdvert] = mechSpec{key: "on", setup: func(ctx context.Context) (func(), error) {
			a := &mdnsAdvertiser{self: s.selfInfo, logger: s.logger}
			a.refresh()
			return func() { a.run(ctx) }, nil
		}}
	}
	if e.MDNS {
		out[mechMDNSBrowse] = mechSpec{key: "on", setup: func(ctx context.Context) (func(), error) {
			return func() { s.runMDNSBrowse(ctx) }, nil
		}}
	}
	if len(e.seeds) > 0 {
		seeds := e.seeds
		out[mechSeeds] = mechSpec{key: strings.Join(seeds, "\n"), setup: func(ctx context.Context) (func(), error) {
			return func() { s.runSeeds(ctx, seeds) }, nil
		}}
	}
	if len(e.domains) > 0 {
		domains := e.domains
		out[mechDNSSRV] = mechSpec{key: strings.Join(domains, "\n"), setup: func(ctx context.Context) (func(), error) {
			return func() { s.runDNSSRV(ctx, domains) }, nil
		}}
	}
	return out
}

// applyLocked stops mechanisms that are no longer wanted or whose settings
// changed and starts missing ones. Requires s.life and a running service.
func (s *Service) applyLocked(e effective) {
	want := s.plan(e)
	var stopping []*mechanism
	for name, m := range s.mechs {
		if spec, ok := want[name]; !ok || spec.key != m.key {
			m.cancel()
			stopping = append(stopping, m)
			delete(s.mechs, name)
		}
	}
	for _, m := range stopping {
		<-m.done
	}
	for _, name := range mechOrder {
		spec, ok := want[name]
		if !ok {
			continue
		}
		if _, running := s.mechs[name]; running {
			continue
		}
		s.mechs[name] = s.startMech(name, spec)
	}
}

// startMech sets up and launches one mechanism. A setup failure is logged
// and recorded (with its key) so that it is retried only when its settings
// change.
func (s *Service) startMech(name string, spec mechSpec) *mechanism {
	ctx, cancel := context.WithCancel(s.rootCtx)
	m := &mechanism{key: spec.key, cancel: cancel, done: make(chan struct{})}
	run, err := spec.setup(ctx)
	if err != nil {
		s.logger.Warn("discovery mechanism unavailable", "mechanism", name, "err", err)
		close(m.done)
		return m
	}
	s.logger.Debug("discovery mechanism started", "mechanism", name)
	go func() {
		defer close(m.done)
		run()
	}()
	return m
}

// stopLocked cancels every mechanism and waits for them. Requires s.life.
func (s *Service) stopLocked() {
	if !s.running {
		return
	}
	s.rootCancel()
	for _, m := range s.mechs {
		m.cancel()
	}
	for _, m := range s.mechs {
		<-m.done
	}
	s.mechs = nil
	s.running = false
	s.prober.close()
}

// effective returns the resolved active configuration.
func (s *Service) effective() effective {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolve(s.cfg)
}

func (s *Service) selfInfo() Info {
	if s.self == nil {
		return Info{}
	}
	return s.self()
}

func (s *Service) selfID() string { return s.selfInfo().ID }

func (s *Service) multicastFailed(err error) {
	if s.multicastWarned.CompareAndSwap(false, true) {
		s.logger.Warn("multicast beacon unavailable; continuing without it", "err", err)
		return
	}
	s.logger.Debug("multicast beacon unavailable", "err", err)
}

// candidate implements sink: record the sighting and queue a verification
// probe unless the peer is already verified at these addresses or the
// addresses were probed recently. Never blocks.
func (s *Service) candidate(c candidate) {
	now := s.now()
	selfID := s.selfID()
	if c.id != "" {
		if c.id == selfID || !s.table.hint(c, now, selfID) {
			return
		}
	}
	urls := make([]string, 0, len(c.urls))
	for _, u := range c.urls {
		if s.throttle.allow(u, now) {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return
	}
	c.urls = urls
	select {
	case s.probeQ <- c:
	default:
		s.logger.Debug("probe queue full, dropping candidate", "source", c.source)
	}
}

// bye implements sink. A bye is only believed from an address the peer is
// known at (see peerTable.bye); otherwise the peer is re-probed soon.
func (s *Service) bye(id string, src netip.Addr) {
	s.table.bye(id, func(addr string) bool { return addrMatchesSource(addr, src, hostNets.isLocal) })
}

// addrMatchesSource reports whether the peer address addr (a base URL) is
// at src: same IP, or a loopback or local address when src is this host.
func addrMatchesSource(addr string, src netip.Addr, isLocal func(netip.Addr) bool) bool {
	_, host, _, ok := urlHostPort(addr)
	if !ok {
		return false
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return advertisedBy(ip, src, isLocal)
}

// verify probes the candidate's URLs in order; the first valid answer adds
// (or refreshes) the peer with the candidate's source.
func (s *Service) verify(ctx context.Context, c candidate, timeout time.Duration) {
	selfID := s.selfID()
	var lastErr error
	for _, u := range c.urls {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		info, err := s.prober.fetch(pctx, u)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if info.ID == selfID {
			return
		}
		s.record(ctx, info, u, c.source)
		if c.id != "" && c.id != info.ID {
			s.table.sightingFailed(c.id, fmt.Sprintf("%s is served by server %s", u, info.ID), s.now())
		}
		return
	}
	if lastErr == nil || ctx.Err() != nil {
		return
	}
	now := s.now()
	msg := ProbeErrorText(lastErr)
	if c.id != "" {
		// An unauthenticated sighting (anyone can announce any ID) never
		// marks a verified peer unhealthy: its own addresses are re-probed.
		if slices.ContainsFunc(c.urls, func(u string) bool { return onThisHost(u, hostNets.isLocal) }) {
			// The sender named addresses of this host: a fixed text, so
			// the peer table tells nobody which local ports are open.
			msg = sightingNotVerified
		}
		s.table.sightingFailed(c.id, msg, now)
		return
	}
	for _, u := range c.urls {
		for _, id := range s.table.findByAddr(u) {
			s.table.probeFailed(id, msg, now)
		}
	}
}

// sightingNotVerified is the error recorded for a sighting that named a
// loopback or local address and could not be verified.
const sightingNotVerified = "could not be verified"

// onThisHost reports whether the base URL addr points at a loopback or
// local address (by IP literal).
func onThisHost(addr string, isLocal func(netip.Addr) bool) bool {
	_, host, _, ok := urlHostPort(addr)
	if !ok {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && isLocal(ip)
}

// record stores info, fetched from addr, in the peer table and returns the
// stored peer. Server IDs are not authenticated: when a verified peer with
// the same ID is known at other addresses, those are re-probed first. If
// one still answers with the ID, the verified record is kept and the claim
// is recorded as a conflict (ok=false); only when none does is the peer
// moved to addr.
func (s *Service) record(ctx context.Context, info Info, addr string, src Source) (Peer, bool) {
	selfID := s.selfID()
	p, res := s.table.observeInfo(info, addr, src, s.now(), selfID)
	switch res {
	case observeStored:
		return p, true
	case observeRejected:
		return Peer{}, false
	}
	if s.knownStillAnswer(ctx, info.ID) {
		if s.table.conflict(info, addr, s.now()) {
			s.logger.Warn("server ID claimed by another address; keeping the verified peer",
				"id", info.ID, "name", p.Name, "known", p.Addresses, "claimedBy", addr)
		} else {
			s.logger.Debug("server ID seen at another address; keeping the verified peer",
				"id", info.ID, "known", p.Addresses, "address", addr)
		}
		return Peer{}, false
	}
	moved, ok := s.table.move(info, addr, src, s.now(), selfID)
	if ok {
		s.logger.Info("peer moved to a new address", "id", info.ID, "name", moved.Name, "from", p.Addresses, "to", addr)
	}
	return moved, ok
}

// knownStillAnswer reports whether verified peer id answers with its ID at
// one of its known addresses (re-probing them unless it answered very
// recently). A cancelled ctx counts as "still answers": nothing moves on
// an undecided check.
func (s *Service) knownStillAnswer(ctx context.Context, id string) bool {
	urls, alive := s.table.knownTargets(id, s.now(), conflictFresh)
	if alive {
		return true
	}
	selfID := s.selfID()
	for _, u := range urls {
		pctx, cancel := context.WithTimeout(ctx, candidateProbeTimeout)
		info, err := s.prober.fetch(pctx, u)
		cancel()
		if ctx.Err() != nil {
			return true
		}
		if err == nil && info.ID == id {
			s.table.observeInfo(info, u, "", s.now(), selfID)
			return true
		}
	}
	return false
}

// verifyAll verifies candidates with bounded concurrency and waits.
func (s *Service) verifyAll(ctx context.Context, cs []candidate, concurrency int, timeout time.Duration) {
	sem := make(chan struct{}, max(concurrency, 1))
	var wg sync.WaitGroup
	for _, c := range cs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Go(func() {
			defer func() { <-sem }()
			s.verify(ctx, c, timeout)
		})
	}
	wg.Wait()
}

// runCore runs the candidate probe workers and peer table maintenance.
func (s *Service) runCore(ctx context.Context) {
	var wg sync.WaitGroup
	for range probeWorkers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case c := <-s.probeQ:
					s.verify(ctx, c, candidateProbeTimeout)
				}
			}
		})
	}
	t := time.NewTicker(maintainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			s.maintain(ctx)
		}
	}
}

// maintain expires stale peers and re-probes verified peers every 30 s
// (twice per peer TTL when that is shorter).
func (s *Service) maintain(ctx context.Context) {
	now := s.now()
	s.table.expire(now)
	due := s.table.due(now, s.table.reprobeInterval())
	sem := make(chan struct{}, reprobeConcurrency)
	var wg sync.WaitGroup
	for _, d := range due {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Go(func() {
			defer func() { <-sem }()
			s.reprobe(ctx, d)
		})
	}
	wg.Wait()
}

// reprobe re-verifies a known peer at its addresses (last good one first).
func (s *Service) reprobe(ctx context.Context, d probeTarget) {
	selfID := s.selfID()
	lastErr := "no known address"
	for _, u := range d.urls {
		pctx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
		info, err := s.prober.fetch(pctx, u)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			lastErr = ProbeErrorText(err)
			continue
		}
		if info.ID != d.id {
			lastErr = fmt.Sprintf("%s is now served by server %s", u, info.ID)
			continue
		}
		// d.urls are known addresses: this refreshes the record (a
		// conflict result only means the peer moved meanwhile).
		s.table.observeInfo(info, u, "", s.now(), selfID)
		return
	}
	s.table.probeFailed(d.id, lastErr, s.now())
}

// runSeeds probes the static seeds on start and every 30 s.
func (s *Service) runSeeds(ctx context.Context, seeds []string) {
	cs := make([]candidate, len(seeds))
	for i, u := range seeds {
		cs[i] = candidate{source: SourceStatic, urls: []string{u}}
	}
	t := time.NewTicker(seedEvery)
	defer t.Stop()
	for {
		s.verifyAll(ctx, cs, seedConcurrency, defaultProbeTimeout)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runDNSSRV resolves _gos3._tcp.<domain> SRV records on start and every 60 s
// and verifies every target.
func (s *Service) runDNSSRV(ctx context.Context, domains []string) {
	t := time.NewTicker(dnsEvery)
	defer t.Stop()
	for {
		s.verifyAll(ctx, s.resolveSRV(ctx, domains), seedConcurrency, candidateProbeTimeout)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// resolveSRV turns the SRV records of each domain into candidates (https
// when the port is 443).
func (s *Service) resolveSRV(ctx context.Context, domains []string) []candidate {
	var out []candidate
	for _, d := range domains {
		if ctx.Err() != nil {
			break
		}
		lctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
		_, srvs, err := s.lookupSRV(lctx, "gos3", "tcp", d)
		cancel()
		if err != nil && len(srvs) == 0 {
			s.logger.Debug("DNS SRV lookup failed", "domain", d, "err", err)
			continue
		}
		for i, r := range srvs {
			if i >= maxSRVRecords {
				break
			}
			if r == nil || r.Port == 0 {
				continue
			}
			target := strings.TrimSuffix(r.Target, ".")
			if target == "" {
				continue
			}
			scheme := "http"
			if r.Port == 443 {
				scheme = "https"
			}
			u, err := NormalizeBaseURL(baseURL(scheme, target, int(r.Port)))
			if err != nil {
				continue
			}
			out = append(out, candidate{source: SourceDNS, urls: []string{u}})
		}
	}
	return out
}

// runMDNSBrowse browses for _gos3._tcp every 30 s.
func (s *Service) runMDNSBrowse(ctx context.Context) {
	t := time.NewTicker(mdnsBrowseEvery)
	defer t.Stop()
	for {
		if err := mdnsQuery(ctx, mdnsQueryTimeout, s.logger, s.selfID(), s); err != nil && ctx.Err() == nil {
			if s.mdnsWarned.CompareAndSwap(false, true) {
				s.logger.Warn("mDNS browsing unavailable", "err", err)
			} else {
				s.logger.Debug("mDNS browsing unavailable", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// throttle limits how often the same candidate URL is probed. It tracks at
// most maxThrottleEntries URLs; a new URL replaces the one probed longest
// ago, so a flood of new URLs never re-enables the URLs probed most
// recently.
type throttle struct {
	mu     sync.Mutex
	window time.Duration
	last   *lru[string, time.Time]
}

func newThrottle(window time.Duration) *throttle {
	return &throttle{window: window, last: newLRU[string, time.Time](maxThrottleEntries)}
}

// allow reports whether key may be probed now and records the attempt.
func (t *throttle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ts, ok := t.last.get(key); ok && now.Sub(ts) < t.window && !now.Before(ts) {
		return false
	}
	t.last.put(key, now)
	return true
}

// collector is a sink that gathers candidates for one-shot browsing.
type collector struct {
	mu   sync.Mutex
	seen map[string]struct{}
	list []candidate
}

func (c *collector) candidate(cd candidate) {
	if len(cd.urls) == 0 {
		return
	}
	key := cd.id + "|" + strings.Join(cd.urls, ",")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, dup := c.seen[key]; dup || len(c.list) >= maxPeers {
		return
	}
	c.seen[key] = struct{}{}
	c.list = append(c.list, cd)
}

func (c *collector) bye(string, netip.Addr) {}

func (c *collector) candidates() []candidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]candidate(nil), c.list...)
}

// Browse performs a one-shot discovery (for the `gos3 discover` command):
// an mDNS query and a beacon query lasting wait (default 3 s), plus the
// configured seeds and DNS SRV domains. Every candidate is verified via
// /_gos3/info; only verified peers are returned, sorted by Name then ID.
// cfg.Enabled and cfg.Advertise are ignored (nothing is advertised); MDNS
// and Beacon select the mechanisms. Errors are returned only for an invalid
// config or a cancelled ctx (with the peers found so far).
func Browse(ctx context.Context, cfg Config, wait time.Duration) ([]Peer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if wait <= 0 {
		wait = mdnsQueryTimeout
	}
	s := New(cfg, nil, nil)
	return s.browse(ctx, wait)
}

func (s *Service) browse(ctx context.Context, wait time.Duration) ([]Peer, error) {
	defer s.prober.close()
	e := s.effective()
	col := &collector{}
	bctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var wg sync.WaitGroup
	if e.MDNS {
		wg.Go(func() {
			if err := mdnsQuery(bctx, wait, s.logger, "", col); err != nil && bctx.Err() == nil {
				s.logger.Debug("mDNS query failed", "err", err)
			}
		})
	}
	if e.Beacon {
		b, err := s.openBeacon(e.group, beaconOptions{interval: e.interval, query: true, sink: col, logger: s.logger, now: s.now})
		if err != nil {
			s.logger.Debug("beacon unavailable", "err", err)
		} else {
			wg.Go(func() { b.run(bctx) })
		}
	}
	for _, u := range e.seeds {
		col.candidate(candidate{source: SourceStatic, urls: []string{u}})
	}
	if len(e.domains) > 0 {
		wg.Go(func() {
			for _, c := range s.resolveSRV(bctx, e.domains) {
				col.candidate(c)
			}
		})
	}
	wg.Wait()
	s.verifyAll(ctx, col.candidates(), 16, candidateProbeTimeout)
	peers := s.table.snapshot("")
	return peers, ctx.Err()
}

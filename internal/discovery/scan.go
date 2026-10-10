package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scan defaults and bounds.
const (
	DefaultScanTimeout     = 1500 * time.Millisecond
	DefaultScanConcurrency = 64
	maxScanTimeout         = 30 * time.Second
	minScanTimeout         = 10 * time.Millisecond
	maxScanConcurrency     = 512
	maxScanTargets         = 1024
	maxScanPorts           = 64
)

var (
	// ErrTooManyHosts is returned when a scan expands to more hosts than
	// Config.MaxScanHosts (admin API: 400).
	ErrTooManyHosts = errors.New("discovery: scan exceeds maxScanHosts")
	// ErrInvalidScan is returned (wrapped) for malformed scan requests:
	// bad targets, ports or schemes (admin API: 400).
	ErrInvalidScan = errors.New("discovery: invalid scan request")
	// ErrScanDisabled is returned when Config.AllowScan is false (admin API: 403).
	ErrScanDisabled = errors.New("discovery: network scans are disabled")
)

// scanTarget is one parsed ScanRequest target.
type scanTarget struct {
	url    string       // base URL target (probed as is)
	prefix netip.Prefix // CIDR target (valid when url == "" and host == "")
	host   string       // host name or IP literal
	port   int          // explicit port of a host target (0 = request ports)
}

// hosts returns the number of hosts the target expands to, or ok=false when
// it exceeds limit.
func (t scanTarget) hosts(limit uint64) (uint64, bool) {
	if t.url != "" || t.host != "" {
		return 1, limit >= 1
	}
	bits := t.prefix.Addr().BitLen() - t.prefix.Bits()
	if bits >= 63 {
		return 0, false
	}
	n := uint64(1) << bits
	if t.prefix.Addr().Is4() && t.prefix.Bits() <= 30 {
		n -= 2 // network and broadcast address
	}
	return n, n <= limit
}

// forEachAddr calls fn for every address of an (IPv4: non-network,
// non-broadcast for prefixes up to /30) CIDR target until fn returns false.
func forEachAddr(p netip.Prefix, fn func(netip.Addr) bool) {
	p = p.Masked()
	bits := p.Addr().BitLen() - p.Bits()
	if bits >= 63 {
		return
	}
	n := uint64(1) << bits
	a := p.Addr()
	if a.Is4() && p.Bits() <= 30 {
		a = a.Next()
		n -= 2
	}
	for i := uint64(0); i < n && a.IsValid(); i++ {
		if !fn(a) {
			return
		}
		a = a.Next()
	}
}

// parseScanTarget classifies a target: base URL (contains "://"), CIDR
// (contains "/"), or host[:port] (host name or IP literal; bracket IPv6
// literals with a port).
func parseScanTarget(raw string) (scanTarget, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxURLLen {
		return scanTarget{}, fmt.Errorf("%w: empty or oversized target", ErrInvalidScan)
	}
	if strings.Contains(s, "://") {
		u, err := NormalizeBaseURL(s)
		if err != nil {
			return scanTarget{}, fmt.Errorf("%w: target %q: %v", ErrInvalidScan, clip(s, 64), err)
		}
		return scanTarget{url: u}, nil
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return scanTarget{}, fmt.Errorf("%w: target %q is not a valid CIDR", ErrInvalidScan, clip(s, 64))
		}
		return scanTarget{prefix: p.Masked()}, nil
	}
	if ip, err := netip.ParseAddr(s); err == nil { // bare IPv4/IPv6 literal
		return scanTarget{host: ip.Unmap().String()}, nil
	}
	host, port := s, 0
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := parsePort(p)
		if err != nil {
			return scanTarget{}, fmt.Errorf("%w: target %q has an invalid port", ErrInvalidScan, clip(s, 64))
		}
		host, port = h, n
	}
	host, ok := canonicalHost(host)
	if !ok {
		return scanTarget{}, fmt.Errorf("%w: target %q is not a host name, IP, CIDR or URL", ErrInvalidScan, clip(s, 64))
	}
	return scanTarget{host: host, port: port}, nil
}

// scanPlan is a validated ScanRequest.
type scanPlan struct {
	targets     []scanTarget
	ports       []int
	schemes     []string
	timeout     time.Duration
	concurrency int
	hosts       uint64
	probes      uint64
}

// planScan validates req against the host limit and applies defaults.
func planScan(req ScanRequest, maxHosts int) (scanPlan, error) {
	var p scanPlan
	if len(req.Targets) == 0 {
		return p, fmt.Errorf("%w: no targets", ErrInvalidScan)
	}
	if len(req.Targets) > maxScanTargets {
		return p, fmt.Errorf("%w: at most %d targets", ErrInvalidScan, maxScanTargets)
	}
	if len(req.Ports) > maxScanPorts {
		return p, fmt.Errorf("%w: at most %d ports", ErrInvalidScan, maxScanPorts)
	}
	for _, port := range req.Ports {
		if port < 1 || port > 65535 {
			return p, fmt.Errorf("%w: invalid port %d", ErrInvalidScan, port)
		}
		if !slices.Contains(p.ports, port) {
			p.ports = append(p.ports, port)
		}
	}
	if len(p.ports) == 0 {
		p.ports = []int{DefaultPort}
	}
	for _, s := range req.Schemes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "http" && s != "https" {
			return p, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidScan, clip(s, 16))
		}
		if !slices.Contains(p.schemes, s) {
			p.schemes = append(p.schemes, s)
		}
	}
	if len(p.schemes) == 0 {
		p.schemes = []string{"http"}
	}
	p.timeout = req.Timeout
	if p.timeout <= 0 {
		p.timeout = DefaultScanTimeout
	}
	p.timeout = min(max(p.timeout, minScanTimeout), maxScanTimeout)
	p.concurrency = req.Concurrency
	if p.concurrency <= 0 {
		p.concurrency = DefaultScanConcurrency
	}
	p.concurrency = min(p.concurrency, maxScanConcurrency)

	limit := uint64(max(maxHosts, 0))
	seen := make(map[string]struct{}, len(req.Targets))
	for _, raw := range req.Targets {
		t, err := parseScanTarget(raw)
		if err != nil {
			return scanPlan{}, err
		}
		key := fmt.Sprintf("%s|%s|%s|%d", t.url, t.prefix, t.host, t.port)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		n, ok := t.hosts(limit - min(p.hosts, limit))
		if !ok || p.hosts+n > limit {
			return scanPlan{}, fmt.Errorf("%w (%d)", ErrTooManyHosts, limit)
		}
		p.hosts += n
		switch {
		case t.url != "":
			p.probes++
		case t.port != 0:
			p.probes += uint64(len(p.schemes))
		default:
			p.probes += n * uint64(len(p.ports)*len(p.schemes))
		}
		p.targets = append(p.targets, t)
	}
	return p, nil
}

// generate sends every probe URL of the plan to out until ctx is done.
func (p scanPlan) generate(ctx context.Context, out chan<- string) {
	send := func(u string) bool {
		select {
		case out <- u:
			return true
		case <-ctx.Done():
			return false
		}
	}
	hostURLs := func(host string, ports []int) bool {
		for _, port := range ports {
			for _, scheme := range p.schemes {
				if !send(baseURL(scheme, host, port)) {
					return false
				}
			}
		}
		return true
	}
	for _, t := range p.targets {
		switch {
		case t.url != "":
			if !send(t.url) {
				return
			}
		case t.host != "":
			ports := p.ports
			if t.port != 0 {
				ports = []int{t.port}
			}
			if !hostURLs(t.host, ports) {
				return
			}
		default:
			ok := true
			forEachAddr(t.prefix, func(a netip.Addr) bool {
				ok = hostURLs(a.String(), p.ports)
				return ok
			})
			if !ok {
				return
			}
		}
	}
}

// scan implements Service.Scan.
func (s *Service) scan(ctx context.Context, req ScanRequest) (ScanResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := s.effective()
	if !cfg.AllowScan {
		return ScanResult{Found: []Peer{}}, ErrScanDisabled
	}
	plan, err := planScan(req, cfg.maxHosts)
	if err != nil {
		return ScanResult{Found: []Peer{}}, err
	}
	start := time.Now()
	selfID := s.selfID()

	jobs := make(chan string)
	var (
		probed atomic.Int64
		mu     sync.Mutex
		found  = make(map[string]Peer)
		wg     sync.WaitGroup
	)
	workers := int(min(uint64(plan.concurrency), max(plan.probes, 1)))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				pctx, cancel := context.WithTimeout(ctx, plan.timeout)
				info, err := s.prober.fetch(pctx, u)
				cancel()
				probed.Add(1)
				if err != nil || info.ID == selfID {
					continue
				}
				now := s.now()
				p, ok := s.table.observeInfo(info, u, SourceScan, now, selfID)
				if !ok {
					p = peerFromInfo(info, u, SourceScan, now)
				}
				mu.Lock()
				if prev, dup := found[p.ID]; dup {
					for _, a := range prev.Addresses {
						addAddress(&p, a)
					}
				}
				found[p.ID] = p
				mu.Unlock()
			}
		}()
	}
	plan.generate(ctx, jobs)
	close(jobs)
	wg.Wait()

	res := ScanResult{
		Found:   make([]Peer, 0, len(found)),
		Probed:  int(probed.Load()),
		Elapsed: time.Since(start).Round(time.Millisecond).String(),
	}
	for id, p := range found {
		// Prefer the merged table view (sources from all mechanisms).
		if tp, ok := s.table.get(id); ok {
			for _, a := range p.Addresses {
				addAddress(&tp, a)
			}
			p = tp
		}
		res.Found = append(res.Found, p)
	}
	sortPeers(res.Found)
	if err := ctx.Err(); err != nil {
		return res, err
	}
	return res, nil
}

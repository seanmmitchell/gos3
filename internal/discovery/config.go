package discovery

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Limits applied by Config.Validate.
const (
	// MaxSeeds bounds Config.Seeds.
	MaxSeeds = 256
	// MaxDNSSRV bounds Config.DNSSRV.
	MaxDNSSRV = 64
	// MaxScanHostsLimit is the largest accepted Config.MaxScanHosts.
	MaxScanHostsLimit = 1 << 20
	// MinBeaconInterval / MaxBeaconInterval bound Config.BeaconInterval (0 = default).
	MinBeaconInterval = time.Second
	MaxBeaconInterval = time.Hour
	// DefaultMaxScanHosts is used when Config.MaxScanHosts is 0.
	DefaultMaxScanHosts = 1024
	// minPeerTTL is the smallest effective peer TTL.
	minPeerTTL = 30 * time.Second
)

// ErrInvalidConfig is returned (wrapped) by Config.Validate.
var ErrInvalidConfig = errors.New("discovery: invalid config")

// Validate checks c for values that discovery cannot use. Zero values are
// valid and mean "default" (BeaconGroup, BeaconInterval, PeerTTL,
// MaxScanHosts). Seeds must be http(s) base URLs (see NormalizeBaseURL) and
// DNSSRV entries DNS domain names.
func (c Config) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidConfig}, args...)...)
	}
	if c.BeaconGroup != "" {
		if _, err := parseBeaconGroup(c.BeaconGroup); err != nil {
			return bad("beaconGroup %q: %v", clip(c.BeaconGroup, 64), err)
		}
	}
	if c.BeaconInterval < 0 || (c.BeaconInterval > 0 && (c.BeaconInterval < MinBeaconInterval || c.BeaconInterval > MaxBeaconInterval)) {
		return bad("beaconInterval must be between %s and %s (or 0 for the default)", MinBeaconInterval, MaxBeaconInterval)
	}
	if c.PeerTTL < 0 {
		return bad("peerTtl must not be negative")
	}
	if c.MaxScanHosts < 0 || c.MaxScanHosts > MaxScanHostsLimit {
		return bad("maxScanHosts must be between 0 and %d", MaxScanHostsLimit)
	}
	if len(c.Seeds) > MaxSeeds {
		return bad("at most %d seeds are allowed", MaxSeeds)
	}
	for _, s := range c.Seeds {
		if _, err := NormalizeBaseURL(s); err != nil {
			return bad("seed: %v", err)
		}
	}
	if len(c.DNSSRV) > MaxDNSSRV {
		return bad("at most %d dnsSrv domains are allowed", MaxDNSSRV)
	}
	for _, d := range c.DNSSRV {
		if !validHostname(strings.TrimSpace(d)) {
			return bad("dnsSrv domain %q is not a valid DNS name", clip(d, 64))
		}
	}
	return nil
}

// parseBeaconGroup parses "ip:port" where ip is an IPv4 multicast address.
func parseBeaconGroup(s string) (*net.UDPAddr, error) {
	ap, err := netip.ParseAddrPort(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("must be <ipv4-multicast-address>:<port>")
	}
	ip := ap.Addr().Unmap()
	if !ip.Is4() || !ip.IsMulticast() {
		return nil, errors.New("address must be an IPv4 multicast address (224.0.0.0/4)")
	}
	if ap.Port() == 0 {
		return nil, errors.New("port must be 1-65535")
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, ap.Port())), nil
}

// effective is a validated Config with defaults applied and lists normalised.
type effective struct {
	Config
	group    *net.UDPAddr
	interval time.Duration
	ttl      time.Duration
	maxHosts int
	seeds    []string // normalised, deduplicated
	domains  []string // trimmed, lower-cased, deduplicated
}

// resolve applies defaults to a Config that passed Validate. Invalid entries
// are skipped defensively.
func resolve(c Config) effective {
	e := effective{Config: c}
	g := c.BeaconGroup
	if g == "" {
		g = DefaultBeaconGroup
	}
	if addr, err := parseBeaconGroup(g); err == nil {
		e.group = addr
	} else {
		e.group, _ = parseBeaconGroup(DefaultBeaconGroup)
	}
	e.interval = c.BeaconInterval
	if e.interval <= 0 {
		e.interval = DefaultBeaconInterval
	}
	e.interval = min(max(e.interval, MinBeaconInterval), MaxBeaconInterval)
	e.ttl = c.PeerTTL
	if e.ttl <= 0 {
		e.ttl = 3 * e.interval
	}
	e.ttl = max(e.ttl, minPeerTTL)
	e.maxHosts = c.MaxScanHosts
	if e.maxHosts <= 0 {
		e.maxHosts = DefaultMaxScanHosts
	}
	e.maxHosts = min(e.maxHosts, MaxScanHostsLimit)
	for _, s := range c.Seeds {
		if n, err := NormalizeBaseURL(s); err == nil && !slices.Contains(e.seeds, n) {
			e.seeds = append(e.seeds, n)
		}
	}
	for _, d := range c.DNSSRV {
		d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if validHostname(d) && !slices.Contains(e.domains, d) {
			e.domains = append(e.domains, d)
		}
	}
	return e
}

// cloneConfig returns a deep copy of c.
func cloneConfig(c Config) Config {
	c.Seeds = slices.Clone(c.Seeds)
	c.DNSSRV = slices.Clone(c.DNSSRV)
	return c
}

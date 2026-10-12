// Package discovery lets gos3 servers find each other (and lets the web UI
// find servers) via mDNS/DNS-SD, a UDP multicast beacon, static seeds, DNS SRV
// records and on-demand network scans. See docs/discovery.md for the wire
// protocol.
//
// CONTRACT: exported names/signatures in this file are fixed (other packages
// code against them concurrently). The implementation is owned by the
// discovery work item; it may add files and replace the stub bodies.
package discovery

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ServiceName is the value of Info.Service for gos3 servers.
const ServiceName = "gos3"

// MDNSService is the DNS-SD service type advertised over mDNS.
const MDNSService = "_gos3._tcp"

// Default beacon settings.
const (
	DefaultBeaconGroup    = "239.255.73.83:7373"
	DefaultBeaconInterval = 10 * time.Second
)

// Info is the public identity document served (unauthenticated, CORS "*") at
// GET /_gos3/info and carried in beacons. Keep it free of secrets.
type Info struct {
	Service    string    `json:"service"`    // always "gos3"
	ID         string    `json:"id"`         // stable server ID (persisted)
	Name       string    `json:"name"`       // human friendly server name
	Version    string    `json:"version"`    // gos3 version
	APIVersion string    `json:"apiVersion"` // admin API version, "v1"
	Region     string    `json:"region"`
	Endpoints  Endpoints `json:"endpoints"`
	Features   []string  `json:"features"` // e.g. "versioning","object-lock","multipart","admin","ui","discovery"
	StartedAt  time.Time `json:"startedAt"`
}

// Endpoints are absolute URLs (when known) or paths of the server's APIs.
type Endpoints struct {
	S3    string   `json:"s3"`            // e.g. "http://10.0.0.5:9000"
	Admin string   `json:"admin"`         // e.g. "http://10.0.0.5:9000/_gos3/admin/v1"
	UI    string   `json:"ui,omitempty"`  // e.g. "http://10.0.0.5:9000/_gos3/ui/" ("" if disabled)
	All   []string `json:"all,omitempty"` // every advertised base URL (multi-homed hosts)
}

// Source identifies how a peer was found.
type Source string

const (
	SourceMDNS   Source = "mdns"
	SourceBeacon Source = "beacon"
	SourceStatic Source = "static"
	SourceDNS    Source = "dns"
	SourceScan   Source = "scan"
	SourceProbe  Source = "probe"
)

// Peer is a discovered gos3 server.
type Peer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Region    string    `json:"region"`
	Endpoints Endpoints `json:"endpoints"`
	Features  []string  `json:"features,omitempty"`
	Sources   []Source  `json:"sources"`
	// Addresses are base URLs this peer was observed at.
	Addresses []string  `json:"addresses"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// Healthy is the result of the last /_gos3/info probe (or true for fresh
	// beacon/mDNS sightings). Verified means Info was fetched over HTTP.
	Healthy  bool   `json:"healthy"`
	Verified bool   `json:"verified"`
	Error    string `json:"error,omitempty"`
}

// Config configures discovery. It is part of the runtime-editable settings.
type Config struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Advertise controls whether this server announces itself (mDNS + beacon).
	Advertise bool `json:"advertise" yaml:"advertise"`
	MDNS      bool `json:"mdns" yaml:"mdns"`
	Beacon    bool `json:"beacon" yaml:"beacon"`
	// BeaconGroup is the UDP multicast group:port (DefaultBeaconGroup).
	BeaconGroup    string        `json:"beaconGroup" yaml:"beacon_group"`
	BeaconInterval time.Duration `json:"beaconInterval" yaml:"beacon_interval"`
	// Seeds are base URLs of known servers probed periodically.
	Seeds []string `json:"seeds" yaml:"seeds"`
	// DNSSRV are domains queried for _gos3._tcp.<domain> SRV records.
	DNSSRV []string `json:"dnsSrv" yaml:"dns_srv"`
	// PeerTTL: peers not seen for this long are dropped (default
	// max(3*BeaconInterval, 30s); 0 or at least MinPeerTTL).
	PeerTTL time.Duration `json:"peerTtl" yaml:"peer_ttl"`
	// AllowScan permits POST /discovery/scan from the admin API.
	AllowScan bool `json:"allowScan" yaml:"allow_scan"`
	// MaxScanHosts bounds a single scan (default 1024).
	MaxScanHosts int `json:"maxScanHosts" yaml:"max_scan_hosts"`
}

// DefaultConfig returns the default discovery configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		Advertise:      true,
		MDNS:           true,
		Beacon:         true,
		BeaconGroup:    DefaultBeaconGroup,
		BeaconInterval: DefaultBeaconInterval,
		PeerTTL:        60 * time.Second,
		AllowScan:      true,
		MaxScanHosts:   1024,
	}
}

// ScanRequest describes an active scan for gos3 servers.
type ScanRequest struct {
	// Targets are CIDRs ("10.0.0.0/24"), hosts ("nas.local") or base URLs
	// ("https://s3.example.com").
	Targets []string `json:"targets"`
	// Ports to probe for CIDR/host targets (default [9000]).
	Ports []int `json:"ports"`
	// Schemes to try (default ["http"]).
	Schemes []string `json:"schemes"`
	// Timeout per probe (default 1.5s); Concurrency (default 64).
	Timeout     time.Duration `json:"timeout"`
	Concurrency int           `json:"concurrency"`
}

// ScanResult is returned by Scan.
type ScanResult struct {
	Found   []Peer `json:"found"`
	Probed  int    `json:"probed"`
	Elapsed string `json:"elapsed"`
}

// Service runs discovery. It is safe for concurrent use.
type Service struct {
	self   func() Info
	logger *slog.Logger

	// Replaceable in tests.
	now        func() time.Time
	lookupSRV  func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	openBeacon func(group *net.UDPAddr, o beaconOptions) (*beacon, error)

	prober   *prober
	table    *peerTable
	probeQ   chan candidate
	throttle *throttle

	multicastWarned atomic.Bool
	mdnsWarned      atomic.Bool

	mu  sync.RWMutex // guards cfg
	cfg Config

	life       sync.Mutex // serialises Start/Stop/Reconfigure
	running    bool
	rootCtx    context.Context
	rootCancel context.CancelFunc
	mechs      map[string]*mechanism
}

// New creates a discovery service. self returns this server's current Info
// (used for advertising and to recognise/ignore our own announcements); it
// may be nil for a pure browser. A nil logger means slog.Default().
//
// The configuration is validated by Start and Reconfigure (see
// Config.Validate); zero values select defaults.
func New(cfg Config, self func() Info, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	e := resolve(cfg)
	s := &Service{
		self:     self,
		logger:   logger.With("component", "discovery"),
		now:      time.Now,
		prober:   newProber(),
		table:    newPeerTable(e.ttl),
		probeQ:   make(chan candidate, probeQueueSize),
		throttle: newThrottle(candidateThrottle),
		cfg:      cloneConfig(cfg),
	}
	s.lookupSRV = net.DefaultResolver.LookupSRV
	s.openBeacon = func(group *net.UDPAddr, o beaconOptions) (*beacon, error) {
		return openBeacon(group, o, s.multicastFailed)
	}
	s.table.configure(e.ttl, e.seeds)
	return s
}

// Start begins advertising/browsing according to the config. It returns once
// listeners are set up; background work stops when ctx is done or Stop is
// called. Failures of individual mechanisms (e.g. multicast unavailable) are
// logged, not fatal; an invalid configuration is returned as an error.
// Start is idempotent while running.
func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.life.Lock()
	defer s.life.Unlock()
	if s.running {
		if s.rootCtx.Err() == nil {
			return nil
		}
		s.stopLocked() // previous context ended; start afresh
	}
	cfg := s.Config()
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.rootCtx, s.rootCancel = context.WithCancel(ctx)
	s.running = true
	s.mechs = make(map[string]*mechanism)
	s.applyLocked(resolve(cfg))
	return nil
}

// Stop stops all background work, sends a beacon bye, withdraws mDNS
// advertisements and waits for every goroutine to exit. It is idempotent.
// The peer table is kept.
func (s *Service) Stop() {
	s.life.Lock()
	defer s.life.Unlock()
	s.stopLocked()
}

// Reconfigure validates and applies a new configuration, restarting only the
// mechanisms whose settings changed. When the service is not running the
// configuration is stored for the next Start.
func (s *Service) Reconfigure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	s.cfg = cloneConfig(cfg)
	s.mu.Unlock()
	e := resolve(cfg)
	s.table.configure(e.ttl, e.seeds)
	if s.running && s.rootCtx.Err() == nil {
		s.applyLocked(e)
	}
	return nil
}

// Config returns the active configuration.
func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// Peers returns currently known peers (excluding this server), sorted by Name
// then ID. Expired peers are dropped first.
func (s *Service) Peers() []Peer {
	s.table.expire(s.now())
	return s.table.snapshot(s.selfID())
}

// Probe fetches GET <baseURL>/_gos3/info and returns the peer it describes,
// adding it to the peer table with SourceProbe. baseURL is normalised with
// NormalizeBaseURL ("nas:9000" → "http://nas:9000"). Without a deadline on
// ctx the probe times out after 5 s. Probing this server itself returns its
// peer view without adding it to the table, and so does probing a URL that
// claims the ID of a verified peer still answering at its known addresses
// (the claim is recorded as a conflict). Errors wrap ErrInvalidURL,
// ErrNotGos3, ErrBlockedAddress (a cloud metadata, unspecified, multicast or
// broadcast address, checked after DNS resolution) or the transport error;
// ProbeErrorText turns them into a fixed category.
func (s *Service) Probe(ctx context.Context, baseURL string) (Peer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return Peer{}, err
	}
	pctx, cancel := withDefaultTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	info, err := s.prober.fetch(pctx, u)
	if err != nil {
		now := s.now()
		for _, id := range s.table.findByAddr(u) {
			s.table.probeFailed(id, ProbeErrorText(err), now)
		}
		return Peer{}, err
	}
	if info.ID != s.selfID() {
		// A claim of a verified peer's ID from a new address does not
		// change the table while the peer still answers (see record); the
		// caller then gets what this URL serves.
		if p, ok := s.record(pctx, info, u, SourceProbe); ok {
			return p, nil
		}
	}
	return peerFromInfo(info, u, SourceProbe, s.now()), nil
}

// Scan actively probes targets and adds found servers with SourceScan.
//
// Targets are expanded as described on ScanRequest (CIDRs skip network and
// broadcast addresses for IPv4 prefixes up to /30; base URLs are probed as
// given) × Ports × Schemes. The number of hosts is bounded by
// Config.MaxScanHosts (ErrTooManyHosts); malformed requests yield
// ErrInvalidScan and a disabled scan (Config.AllowScan false) ErrScanDisabled.
// Found peers are de-duplicated by ID, exclude this server, are sorted by
// Name then ID, and are bounded like the peer table (at most 1024). When ctx is cancelled the partial result is returned with
// ctx.Err().
func (s *Service) Scan(ctx context.Context, req ScanRequest) (ScanResult, error) {
	return s.scan(ctx, req)
}

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
	// PeerTTL: peers not seen for this long are dropped (default 3*BeaconInterval, min 30s).
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

// Service runs discovery.
type Service struct {
	cfg    Config
	self   func() Info
	logger *slog.Logger
}

// New creates a discovery service. self returns this server's current Info
// (used for advertising and to recognise/ignore our own announcements).
//
// CONTRACT STUB.
func New(cfg Config, self func() Info, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: cfg, self: self, logger: logger}
}

// Start begins advertising/browsing according to the config. It returns once
// listeners are set up; background work stops when ctx is done or Stop is
// called. Failures of individual mechanisms (e.g. multicast unavailable) are
// logged, not fatal. CONTRACT STUB.
func (s *Service) Start(ctx context.Context) error { return nil }

// Stop stops all background work and withdraws mDNS advertisements.
func (s *Service) Stop() {}

// Reconfigure applies a new configuration (restarting mechanisms as needed).
// CONTRACT STUB.
func (s *Service) Reconfigure(cfg Config) error { s.cfg = cfg; return nil }

// Config returns the active configuration.
func (s *Service) Config() Config { return s.cfg }

// Peers returns currently known peers (excluding this server), sorted by Name then ID.
// CONTRACT STUB.
func (s *Service) Peers() []Peer { return nil }

// Probe fetches GET <baseURL>/_gos3/info and returns the peer it describes,
// adding it to the peer table with SourceProbe. CONTRACT STUB.
func (s *Service) Probe(ctx context.Context, baseURL string) (Peer, error) {
	return Peer{}, context.Canceled
}

// Scan actively probes targets and adds found servers with SourceScan.
// CONTRACT STUB.
func (s *Service) Scan(ctx context.Context, req ScanRequest) (ScanResult, error) {
	return ScanResult{}, nil
}

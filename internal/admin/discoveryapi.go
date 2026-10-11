package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
)

// Discovery request bounds.
const (
	probeTimeout       = 10 * time.Second
	scanTimeout        = 5 * time.Minute
	maxScanTargets     = 256
	maxScanPorts       = 64
	maxScanConcurrency = 1024
	maxProbeTimeout    = time.Minute
	// maxConcurrentScans bounds the network scans running at once on this
	// server: each one may run hundreds of probe workers for minutes.
	maxConcurrentScans = 2
	// maxConcurrentProbes bounds the single-URL probes running at once, so
	// one key cannot make the server hold open many outbound connections.
	maxConcurrentProbes = 4
)

type discoveryState struct {
	Enabled bool             `json:"enabled"`
	Config  discovery.Config `json:"config"`
	Self    discovery.Info   `json:"self"`
	Peers   []discovery.Peer `json:"peers"`
}

func (a *api) getDiscovery(rc *reqCtx) (int, any, error) {
	cfg := a.d.Settings.Get().Discovery
	if cfg.Seeds == nil {
		cfg.Seeds = []string{}
	}
	if cfg.DNSSRV == nil {
		cfg.DNSSRV = []string{}
	}
	st := discoveryState{
		Enabled: a.d.Discovery != nil && cfg.Enabled,
		Config:  cfg,
		Self:    a.info(),
		Peers:   []discovery.Peer{},
	}
	if a.d.Discovery != nil {
		if p := a.d.Discovery.Peers(); p != nil {
			st.Peers = p
		}
	}
	return http.StatusOK, st, nil
}

func (a *api) requireDiscovery() error {
	if a.d.Discovery == nil {
		return badRequest("discovery is disabled on this server")
	}
	return nil
}

func (a *api) probe(rc *reqCtx) (int, any, error) {
	var req struct {
		URL string `json:"url"`
	}
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	req.URL = strings.TrimSpace(req.URL)
	if err := config.ValidateBaseURL(req.URL, true); err != nil {
		return 0, nil, badRequest("url: %v", err)
	}
	if err := a.requireDiscovery(); err != nil {
		return 0, nil, err
	}
	select {
	case a.probes <- struct{}{}:
		defer func() { <-a.probes }()
	default:
		return 0, nil, errorf(http.StatusTooManyRequests, codeTooManyRequests,
			"%d probes are already running on this server; retry when one has finished", maxConcurrentProbes)
	}
	ctx, cancel := context.WithTimeout(rc.ctx, probeTimeout)
	defer cancel()
	peer, err := a.d.Discovery.Probe(ctx, req.URL)
	if err != nil {
		return 0, nil, errorf(http.StatusBadGateway, codeProbeFailed, "probing %s failed: %s", req.URL, probeFailure(err))
	}
	return http.StatusOK, peer, nil
}

// probeFailure maps a probe error to a coarse category: "timed out",
// "HTTP <status>", "not a gos3 server" or "unreachable". The raw error is
// never returned: it would tell refused, filtered and TLS ports apart and
// echo the first line a non-HTTP service sends (an SSH banner, say), which
// made probes a port scanner and banner grabber for internal hosts.
func probeFailure(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case errors.Is(err, discovery.ErrNotGos3):
		// discovery reports a non-200 answer as
		// "<ErrNotGos3>: GET /_gos3/info returned <code> <reason>"; only the
		// three-digit code is passed on (the reason phrase is the target's).
		prefix := discovery.ErrNotGos3.Error() + ": GET " + discovery.InfoPath + " returned "
		if rest, ok := strings.CutPrefix(err.Error(), prefix); ok && len(rest) >= 3 &&
			isDigits(rest[:3]) && (len(rest) == 3 || rest[3] == ' ') {
			return "HTTP " + rest[:3]
		}
		return "not a gos3 server"
	}
	return "unreachable"
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func (a *api) scan(rc *reqCtx) (int, any, error) {
	var req discovery.ScanRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	cfg := a.d.Settings.Get().Discovery
	if !cfg.AllowScan {
		return 0, nil, errorf(http.StatusForbidden, codeAccessDenied,
			"network scanning is disabled on this server (discovery.allowScan is false)")
	}
	if err := a.requireDiscovery(); err != nil {
		return 0, nil, err
	}
	if err := validateScanRequest(&req, cfg.MaxScanHosts); err != nil {
		return 0, nil, err
	}
	select {
	case a.scans <- struct{}{}:
		defer func() { <-a.scans }()
	default:
		return 0, nil, errorf(http.StatusTooManyRequests, codeTooManyRequests,
			"%d network scans are already running on this server; retry when one has finished", maxConcurrentScans)
	}
	ctx, cancel := context.WithTimeout(rc.ctx, scanTimeout)
	defer cancel()
	res, err := a.d.Discovery.Scan(ctx, req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, err
		}
		return 0, nil, badRequest("scan failed: %v", err)
	}
	if res.Found == nil {
		res.Found = []discovery.Peer{}
	}
	return http.StatusOK, res, nil
}

// validateScanRequest checks a scan request's shape and that it does not
// expand to more than maxHosts hosts (CIDR network/broadcast addresses
// excluded like the scanner does).
func validateScanRequest(req *discovery.ScanRequest, maxHosts int) error {
	if len(req.Targets) == 0 {
		return badRequest("targets: at least one CIDR, host or URL is required")
	}
	if len(req.Targets) > maxScanTargets {
		return badRequest("targets: at most %d entries allowed", maxScanTargets)
	}
	if len(req.Ports) > maxScanPorts {
		return badRequest("ports: at most %d entries allowed", maxScanPorts)
	}
	for _, p := range req.Ports {
		if p < 1 || p > 65535 {
			return badRequest("ports: %d is not a valid port", p)
		}
	}
	for _, s := range req.Schemes {
		if s != "http" && s != "https" {
			return badRequest("schemes: %q is not supported (http or https)", s)
		}
	}
	if req.Timeout < 0 || req.Timeout > maxProbeTimeout {
		return badRequest("timeout: must be between 0 and %d nanoseconds", int64(maxProbeTimeout))
	}
	if req.Concurrency < 0 || req.Concurrency > maxScanConcurrency {
		return badRequest("concurrency: must be between 0 and %d", maxScanConcurrency)
	}
	if maxHosts <= 0 {
		maxHosts = discovery.DefaultConfig().MaxScanHosts
	}
	var total uint64
	for _, t := range req.Targets {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > config.MaxURLLen {
			return badRequest("targets: empty or overlong entry")
		}
		n, err := scanTargetHosts(t)
		if err != nil {
			return badRequest("targets: %q %v", t, err)
		}
		total += n
		if total > uint64(maxHosts) {
			return badRequest("scan covers too many hosts (more than maxScanHosts = %d)", maxHosts)
		}
	}
	return nil
}

// scanTargetHosts returns how many hosts a scan target expands to.
func scanTargetHosts(t string) (uint64, error) {
	if strings.Contains(t, "://") {
		u, err := url.Parse(t)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return 0, errors.New("is not a valid http(s) base URL")
		}
		return 1, nil
	}
	if strings.Contains(t, "/") {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			return 0, errors.New("is not a valid CIDR")
		}
		hostBits := p.Addr().BitLen() - p.Bits()
		if hostBits >= 63 {
			return 1 << 63, nil
		}
		n := uint64(1) << hostBits
		if p.Addr().Is4() && hostBits >= 2 {
			n -= 2 // network and broadcast addresses
		}
		return n, nil
	}
	if _, err := netip.ParseAddr(t); err == nil {
		return 1, nil
	}
	h := t
	if host, port, ok := strings.Cut(t, ":"); ok && port != "" && !strings.Contains(port, ":") {
		h = host
	}
	if !config.ValidHostname(h) {
		return 0, errors.New("is not a valid CIDR, host or URL")
	}
	return 1, nil
}

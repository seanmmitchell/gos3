package discovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// MaxInfoSize bounds the /_gos3/info document accepted by probes.
const MaxInfoSize = 64 << 10

// Probe timeouts.
const (
	// defaultProbeTimeout applies to Probe calls whose context has no deadline
	// and to periodic re-probes.
	defaultProbeTimeout = 5 * time.Second
	// candidateProbeTimeout applies to verification of beacon/mDNS/DNS candidates.
	candidateProbeTimeout = 3 * time.Second
)

// ErrNotGos3 is returned (wrapped) when a probed URL does not serve a valid
// gos3 identity document.
var ErrNotGos3 = errors.New("discovery: not a gos3 server")

// ErrBlockedAddress is returned (wrapped) when a probe target is, or
// resolves to, an address discovery never contacts: cloud metadata
// services and unspecified, multicast and broadcast addresses.
var ErrBlockedAddress = errors.New("discovery: address not allowed")

// metadataAddrs are the instance metadata endpoints of the big clouds (AWS
// IMDS and the ECS/EKS credential endpoints; GCP, Azure and OpenStack share
// 169.254.169.254; Alibaba uses 100.100.100.200). Other link-local addresses
// stay allowed: zeroconf networks use them.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("169.254.170.23"),
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("100.100.100.200"),
}

// nat64Prefix is the well-known NAT64 prefix: an address in it reaches the
// embedded IPv4 address.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// blockedProbeAddr reports whether discovery must never connect to ip:
// metadata addresses, unspecified addresses (including 0.0.0.0/8),
// multicast addresses and the IPv4 limited broadcast address, also in their
// IPv4-mapped and NAT64 forms.
func blockedProbeAddr(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if nat64Prefix.Contains(ip) {
		b := ip.As16()
		if blockedProbeAddr(netip.AddrFrom4([4]byte(b[12:]))) {
			return true
		}
	}
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast():
		return true
	case ip.Is4() && ip.As4()[0] == 0: // 0.0.0.0/8, "this network"
		return true
	case ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return true
	}
	return slices.Contains(metadataAddrs, ip)
}

func blockedAddrError(ip netip.Addr) error {
	return fmt.Errorf("%w: %s is a cloud metadata, unspecified, multicast or broadcast address", ErrBlockedAddress, ip)
}

// checkDialAddr is the dialer's Control hook. It runs for every connection
// attempt with the resolved "ip:port", after any DNS lookup, so a name that
// resolves to a blocked address is refused as well as a literal one.
func checkDialAddr(_ context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unexpected dial address %q", ErrBlockedAddress, clip(address, 64))
	}
	if blockedProbeAddr(ap.Addr()) {
		return blockedAddrError(ap.Addr())
	}
	return nil
}

// prober fetches identity documents. It never uses an HTTP proxy (discovery
// targets are LAN addresses), never follows redirects, never connects to a
// blocked address (blockedProbeAddr, checked after DNS resolution) and never
// reuses connections, so no goroutines outlive a probe.
type prober struct {
	client *http.Client
	dialer *net.Dialer
	// lookup resolves a host name to its addresses (replaceable in tests).
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
}

func newProber() *prober {
	p := &prober{
		dialer: &net.Dialer{
			Timeout:        10 * time.Second,
			KeepAlive:      -1,
			ControlContext: checkDialAddr,
		},
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            p.dialContext,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	p.client = &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return p
}

// dialContext resolves address itself, skips blocked addresses and dials
// the others in order; the dialer's Control hook checks every attempt again.
func (p *prober) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = p.lookup(ctx, host); err != nil {
		return nil, err
	}
	var firstErr error
	for _, ip := range ips {
		if ip.Is4In6() {
			ip = ip.Unmap()
		}
		if blockedProbeAddr(ip) {
			if firstErr == nil {
				firstErr = blockedAddrError(ip)
			}
			continue
		}
		c, err := p.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		if firstErr == nil || errors.Is(firstErr, ErrBlockedAddress) {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if firstErr == nil {
		firstErr = &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	return nil, firstErr
}

// close releases idle resources.
func (p *prober) close() { p.client.CloseIdleConnections() }

// fetch performs GET <base>/_gos3/info (base must be normalised) and returns
// the validated, sanitised document. The caller bounds the time with ctx.
func (p *prober) fetch(ctx context.Context, base string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+InfoPath, nil)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gos3-discovery/1")
	resp, err := p.client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Info{}, fmt.Errorf("%w: GET %s returned %s", ErrNotGos3, InfoPath, resp.Status)
	}
	if resp.ContentLength > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document too large (%d bytes)", ErrNotGos3, resp.ContentLength)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxInfoSize+1))
	if err != nil {
		return Info{}, err
	}
	if len(body) > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document exceeds %d bytes", ErrNotGos3, MaxInfoSize)
	}
	return ParseInfo(body)
}

// ParseInfo decodes and validates an identity document: it must be a single
// JSON object with "service": "gos3" and a valid "id" (1-64 characters of
// [A-Za-z0-9._-]). Strings and lists are cleaned and bounded.
func ParseInfo(b []byte) (Info, error) {
	if len(b) > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document exceeds %d bytes", ErrNotGos3, MaxInfoSize)
	}
	var info Info
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&info); err != nil {
		return Info{}, fmt.Errorf("%w: invalid JSON: %v", ErrNotGos3, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Info{}, fmt.Errorf("%w: trailing data after JSON document", ErrNotGos3)
	}
	if info.Service != ServiceName {
		return Info{}, fmt.Errorf("%w: service is %q", ErrNotGos3, clip(info.Service, 32))
	}
	if !validID(info.ID) {
		return Info{}, fmt.Errorf("%w: missing or invalid id", ErrNotGos3)
	}
	return sanitizeInfo(info), nil
}

// ProbeErrorText describes a probe error by a fixed category ("connection
// refused", "timed out", "not a gos3 server", ...). The peer table stores
// this instead of the transport error, whose text can carry data the remote
// end chose, such as the first line a non-HTTP service sent.
func ProbeErrorText(err error) string {
	var (
		dnsErr   *net.DNSError
		certErr  *tls.CertificateVerificationError
		authErr  x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		invalErr x509.CertificateInvalidError
		recErr   tls.RecordHeaderError
		netErr   net.Error
	)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBlockedAddress):
		return "address not allowed (cloud metadata, unspecified, multicast or broadcast)"
	case errors.Is(err, ErrInvalidURL):
		return "invalid URL"
	case errors.Is(err, ErrNotGos3):
		return "not a gos3 server"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable"
	case errors.As(err, &dnsErr):
		return "host name could not be resolved"
	case errors.As(err, &certErr), errors.As(err, &authErr), errors.As(err, &hostErr), errors.As(err, &invalErr):
		return "TLS certificate not trusted"
	case errors.As(err, &recErr):
		return "not a TLS server"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed by the server"
	case strings.Contains(err.Error(), "malformed HTTP"), strings.Contains(err.Error(), "HTTP response to HTTPS client"):
		return "not an HTTP server"
	}
	return "probe failed"
}

// withDefaultTimeout bounds ctx by d unless it already has a deadline.
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

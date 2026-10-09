package discovery

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// NewServerID returns a new random server ID: 16 lower-case hex characters
// from crypto/rand.
func NewServerID() string { return randomHex(8) }

// randomHex returns 2n hex characters of crypto-random data.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go ≥ 1.24)
	return hex.EncodeToString(b)
}

// SelfAdvertisedURLs returns the base URLs under which a server listening on
// listenAddr (e.g. ":9000", "0.0.0.0:9000", "[::]:9000", "10.0.0.5:9000") can
// be reached, for Info.Endpoints.All and discovery announcements.
//
// publicURL (if valid) comes first. A specific listen host yields just that
// host; a wildcard host yields every non-loopback address of the up
// interfaces (IPv4 first; IPv6 only for "::"/"" listeners, link-local
// excluded). When no such address exists the loopback address is used.
// The scheme is https when tls is set. Invalid listen addresses yield only
// the public URL.
func SelfAdvertisedURLs(listenAddr, publicURL string, tls bool) []string {
	var out []string
	add := func(u string) {
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	if publicURL != "" {
		if u, err := NormalizeBaseURL(publicURL); err == nil {
			add(u)
		}
	}
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return out
	}
	port, err := parsePort(portStr)
	if err != nil {
		return out
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	host = strings.TrimSuffix(host, ".")
	if !isWildcardHost(host) {
		if !validHost(host) {
			return out
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			host = ip.Unmap().String()
		} else {
			host = strings.ToLower(host)
		}
		add(baseURL(scheme, host, port))
		return out
	}
	v6 := host == "" || host == "::"
	ips := interfaceIPs(v6)
	if len(ips) == 0 {
		ips = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}
	}
	for _, ip := range ips {
		add(baseURL(scheme, ip.String(), port))
	}
	return out
}

// interfaceIPs returns the non-loopback unicast addresses of up interfaces,
// IPv4 first, then (when v6 is set) global IPv6. IPv4 link-local
// (169.254/16) and IPv6 link-local addresses are skipped.
func interfaceIPs(v6 bool) []netip.Addr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4s, v6s []netip.Addr
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			switch {
			case ip.Is4():
				if !slices.Contains(v4s, ip) {
					v4s = append(v4s, ip)
				}
			case v6 && ip.IsGlobalUnicast():
				if !slices.Contains(v6s, ip) {
					v6s = append(v6s, ip)
				}
			}
		}
	}
	return append(v4s, v6s...)
}

// portOf returns the port of a base URL (explicit or scheme default).
func portOf(base string) (scheme string, port int, ok bool) {
	u, err := NormalizeBaseURL(base)
	if err != nil {
		return "", 0, false
	}
	s, _, p, ok := urlHostPort(u)
	if !ok {
		return "", 0, false
	}
	return s, effectivePort(s, p), true
}

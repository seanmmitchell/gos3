// Package clientip derives a request's client address and transport
// security, honouring the X-Forwarded-For and X-Forwarded-Proto headers of
// a trusted reverse proxy. The S3 API, the admin API and the system
// endpoints all use it, so one policy condition (aws:SourceIp,
// aws:SecureTransport) gives the same answer everywhere.
//
// Rules (server.trust_proxy_headers and server.trusted_proxies):
//
//   - Without trust_proxy_headers the headers are ignored: the client is the
//     connection's RemoteAddr and the request is secure when it arrived over
//     TLS.
//   - With trust_proxy_headers the headers are honoured only when
//     trusted_proxies is empty or RemoteAddr is inside it.
//   - The client IP is found by walking X-Forwarded-For (every header line,
//     comma-separated) from the right, skipping entries inside
//     trusted_proxies: the first entry outside it is the client. A proxy
//     appends the address it received the request from, so entries to the
//     right of the last trusted hop were written by trusted proxies and
//     entries further left may be forged by the client. An invalid entry
//     stops the walk; the last trusted hop (or RemoteAddr) is used then.
//     With an empty trusted_proxies the rightmost entry is the client (a
//     single proxy that appends to or overwrites the header). The server
//     configuration does not allow that combination (config.Validate):
//     any client reaching gos3 directly could then choose its address.
//   - Secure is the last X-Forwarded-Proto value equal to "https"; the
//     proxy must set the header, not append to a client-sent one.
//   - Without forwarding headers, secure follows Request.TLS; the server
//     fills that in from the connection (an HTTP/2 client's ":scheme"
//     does not decide it).
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver computes the client address of requests. The zero value ignores
// the forwarding headers. It is immutable and safe for concurrent use.
type Resolver struct {
	trust   bool
	proxies []netip.Prefix
}

// New returns a Resolver. trustHeaders is server.trust_proxy_headers;
// proxies is the parsed server.trusted_proxies (see ParsePrefixes). proxies
// has no effect unless trustHeaders is set.
func New(trustHeaders bool, proxies []netip.Prefix) Resolver {
	return Resolver{trust: trustHeaders, proxies: append([]netip.Prefix(nil), proxies...)}
}

// TrustsHeaders reports whether the resolver ever honours the forwarding
// headers (server.trust_proxy_headers).
func (r Resolver) TrustsHeaders() bool { return r.trust }

// ParsePrefixes parses server.trusted_proxies entries: CIDR prefixes
// ("10.0.0.0/8", "2001:db8::/32") or single addresses ("192.0.2.10",
// "::1", "[::1]"). Host bits of a prefix are cleared; IPv4-mapped IPv6
// forms ("::ffff:10.0.0.0/104") are converted to IPv4. Zones are rejected.
// The error names the first invalid entry by its index.
func ParsePrefixes(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for i, e := range entries {
		p, err := ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// ParsePrefix parses one trusted proxy entry (see ParsePrefixes).
func ParsePrefix(s string) (netip.Prefix, error) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		t = t[1 : len(t)-1]
	}
	invalid := fmt.Errorf("%q is not an IP address or CIDR prefix (e.g. \"10.0.0.0/8\", \"192.0.2.10\", \"2001:db8::/32\")", s)
	if t == "" || strings.Contains(t, "%") {
		return netip.Prefix{}, invalid
	}
	if strings.Contains(t, "/") {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			return netip.Prefix{}, invalid
		}
		a, bits := p.Addr(), p.Bits()
		if a.Is4In6() && bits >= 96 {
			a, bits = a.Unmap(), bits-96
		}
		return netip.PrefixFrom(a, bits).Masked(), nil
	}
	a, err := netip.ParseAddr(t)
	if err != nil {
		return netip.Prefix{}, invalid
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ClientInfo returns the client IP address of req and whether it reached
// the server (or the trusted proxy in front of it) over TLS. ip is empty only
// when RemoteAddr is empty and no forwarding header is honoured.
func (r Resolver) ClientInfo(req *http.Request) (ip string, secure bool) {
	remote, remoteOK := parseRemote(req.RemoteAddr)
	if remoteOK {
		ip = remote.String()
	} else {
		ip = req.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}
	secure = req.TLS != nil
	if !r.trust {
		return ip, secure
	}
	if len(r.proxies) > 0 && (!remoteOK || !r.trusted(remote)) {
		return ip, secure
	}
	if client, ok := r.walkForwardedFor(req.Header.Values("X-Forwarded-For")); ok {
		ip = client.String()
	}
	if p, ok := lastValue(req.Header.Values("X-Forwarded-Proto")); ok {
		secure = strings.EqualFold(p, "https")
	}
	return ip, secure
}

// walkForwardedFor applies the right-to-left rule to the X-Forwarded-For
// header lines. ok is false when no entry is usable (the caller keeps
// RemoteAddr).
func (r Resolver) walkForwardedFor(lines []string) (client netip.Addr, ok bool) {
	var entries []string
	for _, line := range lines {
		for e := range strings.SplitSeq(line, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	var lastHop netip.Addr
	haveHop := false
	for i := len(entries) - 1; i >= 0; i-- {
		a, valid := parseForwarded(entries[i])
		if !valid {
			break
		}
		if !r.trusted(a) {
			return a, true
		}
		lastHop, haveHop = a, true
	}
	return lastHop, haveHop
}

// trusted reports whether a is inside trusted_proxies.
func (r Resolver) trusted(a netip.Addr) bool {
	for _, p := range r.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseRemote parses a RemoteAddr ("ip:port", "[ipv6%zone]:port" or a bare
// IP) into an unmapped, zone-less address.
func parseRemote(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	return parseAddr(s)
}

// parseForwarded parses one X-Forwarded-For entry: an IPv4 or IPv6
// address, optionally bracketed ("[2001:db8::1]"), with an optional port
// ("192.0.2.1:443", "[2001:db8::1]:443") or zone, and optionally quoted.
func parseForwarded(e string) (netip.Addr, bool) {
	if len(e) >= 2 && e[0] == '"' && e[len(e)-1] == '"' {
		e = e[1 : len(e)-1]
	}
	if strings.HasPrefix(e, "[") {
		end := strings.IndexByte(e, ']')
		if end < 0 {
			return netip.Addr{}, false
		}
		if rest := e[end+1:]; rest != "" && !validPort(rest) {
			return netip.Addr{}, false
		}
		e = e[1:end]
		if !strings.Contains(e, ":") { // brackets are only for IPv6
			return netip.Addr{}, false
		}
		return parseAddr(e)
	}
	if strings.Count(e, ":") == 1 { // IPv4 with a port
		host, port, _ := strings.Cut(e, ":")
		if !validPort(":" + port) {
			return netip.Addr{}, false
		}
		a, ok := parseAddr(host)
		if !ok || !a.Is4() {
			return netip.Addr{}, false
		}
		return a, true
	}
	return parseAddr(e)
}

func parseAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// validPort reports whether s is ":<1-5 digits>".
func validPort(s string) bool {
	if len(s) < 2 || len(s) > 6 || s[0] != ':' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// lastValue returns the last non-empty comma-separated value of the header
// lines.
func lastValue(lines []string) (string, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		parts := strings.Split(lines[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			if v := strings.TrimSpace(parts[j]); v != "" {
				return v, true
			}
		}
	}
	return "", false
}

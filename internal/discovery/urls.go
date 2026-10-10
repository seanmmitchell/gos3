package discovery

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// InfoPath is the path of the public identity document on every gos3 server.
const InfoPath = "/_gos3/info"

// DefaultPort is the default gos3 listener port, used for scan targets
// without an explicit port and for beacon announcements without URLs.
const DefaultPort = 9000

// maxURLLen bounds every URL string accepted from configuration, requests or
// the network.
const maxURLLen = 2048

// ErrInvalidURL is returned (wrapped) for base URLs that cannot be normalised.
var ErrInvalidURL = errors.New("discovery: invalid base URL")

// NormalizeBaseURL turns user or network supplied server addresses into a
// canonical base URL "scheme://host[:port]":
//
//   - "http://" is prepended when no scheme is given ("nas.local:9000");
//   - only http and https are accepted;
//   - user info, path, query and fragment are dropped ("http://h:9000/x/" →
//     "http://h:9000");
//   - scheme and host name are lower-cased, a trailing dot of the host is
//     removed and the port (if any) must be 1-65535.
//
// The port is kept as given; no default port is added.
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxURLLen {
		return "", fmt.Errorf("%w: %q", ErrInvalidURL, clip(raw, 64))
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidURL, clip(raw, 64))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrInvalidURL, clip(u.Scheme, 16))
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" || !validHost(host) {
		return "", fmt.Errorf("%w: invalid host in %q", ErrInvalidURL, clip(raw, 64))
	}
	port := u.Port()
	if port != "" {
		if _, err := parsePort(port); err != nil {
			return "", fmt.Errorf("%w: invalid port in %q", ErrInvalidURL, clip(raw, 64))
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("%w: empty port in %q", ErrInvalidURL, clip(raw, 64))
	}
	if _, err := netip.ParseAddr(host); err != nil {
		host = strings.ToLower(host)
	}
	return scheme + "://" + joinHostPort(host, port), nil
}

// parsePort parses a decimal TCP/UDP port in 1..65535.
func parsePort(s string) (int, error) {
	if s == "" || len(s) > 5 {
		return 0, errors.New("invalid port")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("invalid port")
	}
	return n, nil
}

// joinHostPort is net.JoinHostPort for URLs: IPv6 zones are escaped as %25
// and an empty port yields just the (bracketed) host.
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + strings.Replace(host, "%", "%25", 1) + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// baseURL builds "scheme://host:port".
func baseURL(scheme, host string, port int) string {
	return scheme + "://" + joinHostPort(host, strconv.Itoa(port))
}

// validHost reports whether h is an IP literal (optionally with an IPv6
// zone) or a syntactically valid DNS host name.
func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return validHostname(h)
}

// validHostname reports whether h is a DNS name made of labels of letters,
// digits, '-' and '_' (≤ 63 bytes each, ≤ 253 bytes overall). A single
// trailing dot is allowed.
func validHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			case c == '-' && i != 0 && i != len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// urlHostPort splits a normalised base URL into scheme, host and port (port
// "" when not explicit).
func urlHostPort(base string) (scheme, host, port string, ok bool) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	return strings.ToLower(u.Scheme), u.Hostname(), u.Port(), true
}

// effectivePort returns the explicit port or the scheme's default port.
func effectivePort(scheme, port string) int {
	if p, err := parsePort(port); err == nil {
		return p
	}
	if scheme == "https" {
		return 443
	}
	return 80
}

// isWildcardHost reports whether host is an unspecified address (0.0.0.0, ::)
// or empty.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsUnspecified()
}

// ipFromAddr extracts the IP of a UDP/IP net.Addr.
func ipFromAddr(a net.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *net.UDPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.Addr{}, false
		}
		ip = ip.Unmap()
		if v.Zone != "" && ip.Is6() {
			ip = ip.WithZone(v.Zone)
		}
		return ip, true
	case *net.IPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		return ip.Unmap(), ok
	}
	if a == nil {
		return netip.Addr{}, false
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// clip truncates s to at most n bytes (on a rune boundary) for use in error
// messages and stored fields.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

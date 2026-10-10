package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/discovery"
)

// Settings validation limits (shared by Store.Update, Config.Validate and the
// admin API, which calls ValidateSettings).
const (
	MaxNameLen          = 128
	MaxListEntries      = 256
	MaxURLLen           = 2048
	MaxCORSMaxAge       = 7 * 24 * 3600
	MinPeerTTL          = time.Second
	MaxPeerTTL          = 7 * 24 * time.Hour
	MaxScanHosts        = 65536
	MinLifecycleSeconds = 60
	MaxLifecycleSeconds = 365 * 24 * 3600
)

var (
	regionRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	headerNameRe = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	schemeRe     = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
)

// ValidRegion reports whether r is a valid region name (lowercase letters,
// digits and hyphens, e.g. "us-east-1").
func ValidRegion(r string) bool { return regionRe.MatchString(r) }

// ValidateSettings checks a full Settings document. It is the single source
// of truth for settings validation: Store.Update, Config.Validate and the
// admin API (PUT /config) all use it. Errors carry a precise, user-facing
// message naming the offending field (JSON names, e.g. "cors.allowedOrigins[0]").
func ValidateSettings(s Settings) error {
	if len(s.Name) > MaxNameLen || !utf8.ValidString(s.Name) || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("name: must be at most %d characters of printable UTF-8", MaxNameLen)
	}
	if !ValidRegion(s.Region) {
		return fmt.Errorf("region: %q is invalid (expected lowercase letters, digits and hyphens, e.g. \"us-east-1\")", s.Region)
	}
	if s.PublicURL != "" {
		if err := ValidateBaseURL(s.PublicURL, true); err != nil {
			return fmt.Errorf("publicUrl: %v", err)
		}
	}
	if len(s.Domains) > MaxListEntries {
		return fmt.Errorf("domains: at most %d entries allowed", MaxListEntries)
	}
	for i, d := range s.Domains {
		if !ValidHostname(d) {
			return fmt.Errorf("domains[%d]: %q is not a valid DNS name (no scheme, port or path)", i, d)
		}
	}
	if err := validateCORS(&s.CORS); err != nil {
		return fmt.Errorf("cors.%v", err)
	}
	if err := validateDiscovery(&s.Discovery); err != nil {
		return fmt.Errorf("discovery.%v", err)
	}
	if _, err := ParseLogLevel(s.Logging.Level); err != nil {
		return fmt.Errorf("logging.level: %q is invalid (expected debug, info, warn or error)", s.Logging.Level)
	}
	switch s.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format: %q is invalid (expected text or json)", s.Logging.Format)
	}
	if s.Lifecycle.IntervalSeconds < MinLifecycleSeconds || s.Lifecycle.IntervalSeconds > MaxLifecycleSeconds {
		return fmt.Errorf("lifecycle.intervalSeconds: must be between %d and %d, got %d",
			MinLifecycleSeconds, MaxLifecycleSeconds, s.Lifecycle.IntervalSeconds)
	}
	return nil
}

func validateCORS(c *CORSConfig) error {
	if len(c.AllowedOrigins) > MaxListEntries {
		return fmt.Errorf("allowedOrigins: at most %d entries allowed", MaxListEntries)
	}
	for i, o := range c.AllowedOrigins {
		if err := ValidateCORSOrigin(o); err != nil {
			return fmt.Errorf("allowedOrigins[%d]: %q %v", i, o, err)
		}
	}
	if len(c.AllowedHeaders) > MaxListEntries {
		return fmt.Errorf("allowedHeaders: at most %d entries allowed", MaxListEntries)
	}
	for i, h := range c.AllowedHeaders {
		if !headerNameRe.MatchString(h) {
			return fmt.Errorf("allowedHeaders[%d]: %q is not a valid header name or pattern", i, h)
		}
	}
	if len(c.ExposeHeaders) > MaxListEntries {
		return fmt.Errorf("exposeHeaders: at most %d entries allowed", MaxListEntries)
	}
	for i, h := range c.ExposeHeaders {
		// A lone "*" is the Fetch-standard wildcard (every header, for
		// requests without credentials); partial patterns are not valid.
		if h == "*" {
			continue
		}
		if !headerNameRe.MatchString(h) || strings.Contains(h, "*") {
			return fmt.Errorf("exposeHeaders[%d]: %q is not a valid header name (only a lone \"*\" wildcard is allowed)", i, h)
		}
	}
	if c.MaxAgeSeconds < 0 || c.MaxAgeSeconds > MaxCORSMaxAge {
		return fmt.Errorf("maxAgeSeconds: must be between 0 and %d, got %d", MaxCORSMaxAge, c.MaxAgeSeconds)
	}
	return nil
}

// ValidateCORSOrigin accepts "*", "scheme://host[:port]",
// "scheme://[ipv6][:port]" and wildcard subdomain origins
// "scheme://*.example.com[:port]". Any URI scheme is accepted (e.g. app://).
func ValidateCORSOrigin(o string) error {
	if o == "*" {
		return nil
	}
	scheme, rest, ok := strings.Cut(o, "://")
	if !ok || !schemeRe.MatchString(scheme) {
		return fmt.Errorf(`must be "*" or an origin like "https://ui.example.com:8443"`)
	}
	if rest == "" || strings.ContainsAny(rest, "/?#@ ") {
		return fmt.Errorf("must not contain a path, query, fragment or credentials")
	}
	host, port := rest, ""
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return fmt.Errorf("has an invalid IPv6 host")
		}
		host, port = rest[1:end], rest[end+1:]
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() {
			return fmt.Errorf("has an invalid IPv6 host")
		}
		if port != "" {
			if !strings.HasPrefix(port, ":") {
				return fmt.Errorf("has an invalid port")
			}
			port = port[1:]
		}
	} else if i := strings.LastIndex(rest, ":"); i >= 0 {
		host, port = rest[:i], rest[i+1:]
		if port == "" {
			return fmt.Errorf("has an empty port")
		}
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || port[0] == '+' {
			return fmt.Errorf("has an invalid port")
		}
	}
	if strings.HasPrefix(o, scheme+"://[") {
		return nil
	}
	if wild, ok := strings.CutPrefix(host, "*."); ok {
		if !ValidHostname(wild) {
			return fmt.Errorf("has an invalid wildcard host (expected e.g. https://*.example.com)")
		}
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !ValidHostname(host) {
		return fmt.Errorf("has an invalid host")
	}
	return nil
}

// validateDiscovery applies the settings rules (explicit values, no zero
// "defaults") and then discovery's own Config.Validate, so a document that
// passes can always be applied by discovery.Service.Reconfigure.
func validateDiscovery(c *discovery.Config) error {
	host, port, err := net.SplitHostPort(c.BeaconGroup)
	if err != nil {
		return fmt.Errorf("beaconGroup: %q must be host:port (e.g. %q)", c.BeaconGroup, discovery.DefaultBeaconGroup)
	}
	if a, err := netip.ParseAddr(host); err != nil || !a.Unmap().Is4() || !a.IsMulticast() {
		return fmt.Errorf("beaconGroup: %q is not an IPv4 multicast address (224.0.0.0/4)", host)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("beaconGroup: port %q must be 1-65535", port)
	}
	if c.BeaconInterval < discovery.MinBeaconInterval || c.BeaconInterval > discovery.MaxBeaconInterval {
		return fmt.Errorf("beaconInterval: must be between %s and %s (nanoseconds), got %d",
			discovery.MinBeaconInterval, discovery.MaxBeaconInterval, int64(c.BeaconInterval))
	}
	if c.PeerTTL < MinPeerTTL || c.PeerTTL > MaxPeerTTL {
		return fmt.Errorf("peerTtl: must be between %s and %s (nanoseconds), got %d",
			MinPeerTTL, MaxPeerTTL, int64(c.PeerTTL))
	}
	if len(c.Seeds) > MaxListEntries {
		return fmt.Errorf("seeds: at most %d entries allowed", MaxListEntries)
	}
	for i, s := range c.Seeds {
		if err := ValidateBaseURL(s, false); err != nil {
			return fmt.Errorf("seeds[%d]: %v", i, err)
		}
	}
	if len(c.DNSSRV) > discovery.MaxDNSSRV {
		return fmt.Errorf("dnsSrv: at most %d entries allowed", discovery.MaxDNSSRV)
	}
	for i, d := range c.DNSSRV {
		if !ValidHostname(strings.TrimSuffix(d, ".")) {
			return fmt.Errorf("dnsSrv[%d]: %q is not a valid domain name", i, d)
		}
	}
	if c.MaxScanHosts < 1 || c.MaxScanHosts > MaxScanHosts {
		return fmt.Errorf("maxScanHosts: must be between 1 and %d, got %d", MaxScanHosts, c.MaxScanHosts)
	}
	if err := c.Validate(); err != nil {
		// Strip discovery's sentinel prefix; the caller adds "discovery.".
		msg := strings.TrimPrefix(err.Error(), discovery.ErrInvalidConfig.Error()+": ")
		return errors.New(msg)
	}
	return nil
}

// ValidateBaseURL checks an absolute http(s) URL without credentials, query
// or fragment. A path is allowed only when allowPath is set.
func ValidateBaseURL(raw string, allowPath bool) error {
	if len(raw) > MaxURLLen {
		return fmt.Errorf("URL longer than %d bytes", MaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an absolute http:// or https:// URL", raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%q must not contain credentials, a query or a fragment", raw)
	}
	if !allowPath && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%q must not contain a path", raw)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q has an invalid port", raw)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("%q has an empty port", raw)
	}
	h := u.Hostname()
	if _, err := netip.ParseAddr(h); err != nil && !ValidHostname(h) {
		return fmt.Errorf("%q has an invalid host", raw)
	}
	return nil
}

// ValidHostname reports whether s is a DNS name: 1-253 chars, dot-separated
// labels of 1-63 letters, digits, hyphens or underscores, not starting or
// ending with a hyphen.
func ValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

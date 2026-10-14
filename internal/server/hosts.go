package server

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/admin"
	"github.com/seanmmitchell/gos3/internal/config"
)

// maxHostWarnings bounds the distinct unknown Host values logged (clients
// choose them freely).
const maxHostWarnings = 32

// hostPolicy decides whether a request's Host header is one of the names
// this server is known by. A website using DNS rebinding makes its own host
// name resolve to the server: the visitor's browser then talks to gos3
// same-origin with the attacker's page, so CORS never applies, and only the
// Host header tells such requests apart. Known hosts are:
//
//   - IP addresses, "localhost" and "*.localhost" (browsers resolve these
//     themselves, so a website cannot rebind them);
//   - the OS host name, its first label and "<label>.local" (mDNS);
//   - the host of server.address when it is a name;
//   - the host of the public URL and the virtual-host domains with their
//     subdomains (runtime settings, read per request);
//   - server.allowed_hosts entries ("*.example.com" for subdomains).
//
// With server.allowed_hosts empty, requests for other hosts are served only
// when they need no ambient authority: signed S3 and admin requests (the
// signature covers the Host the client meant), /_gos3/info, health and
// ready. Anonymous S3 requests, the web UI and public metrics are refused.
// With allowed_hosts set every request for another host is refused, which
// also stops a signature captured by a foreign endpoint from being relayed
// to this server. allowed_hosts ["*"] turns the check off.
type hostPolicy struct {
	any      bool            // allowed_hosts ["*"]
	strict   bool            // allowed_hosts lists names: refuse every unknown host
	exact    map[string]bool // lower-case names without port or trailing dot
	suffixes []string        // ".example.com" for "*.example.com"

	mu     sync.Mutex
	warned map[string]bool
}

// newHostPolicy builds the static part of the policy from cfg.
func newHostPolicy(cfg config.Config) *hostPolicy {
	p := &hostPolicy{exact: map[string]bool{}, warned: map[string]bool{}}
	add := func(h string) {
		if h = hostOnly(h); h != "" {
			p.exact[h] = true
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		label, _, _ := strings.Cut(name, ".")
		add(name)
		add(label)
		add(label + ".local")
	}
	if host, _, err := net.SplitHostPort(cfg.Server.Address); err == nil {
		add(host)
	}
	for _, h := range cfg.Server.AllowedHosts {
		switch {
		case h == "*":
			p.any = true
		case strings.HasPrefix(h, "*."):
			p.suffixes = append(p.suffixes, strings.ToLower(h[1:]))
		default:
			add(h)
		}
	}
	p.strict = len(cfg.Server.AllowedHosts) > 0 && !p.any
	return p
}

// known reports whether host (a Host header) names this server, given the
// current runtime settings.
func (p *hostPolicy) known(host string, st *config.Settings) bool {
	if p.any {
		return true
	}
	h := hostOnly(host)
	if h == "" {
		return true // HTTP/1.0 without Host: not a browser
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || p.exact[h] {
		return true
	}
	for _, suf := range p.suffixes {
		if len(h) > len(suf) && strings.HasSuffix(h, suf) {
			return true
		}
	}
	if st.PublicURL != "" {
		if u, err := url.Parse(st.PublicURL); err == nil && hostOnly(u.Host) == h {
			return true
		}
	}
	for _, d := range st.Domains {
		if d = hostOnly(d); d != "" && (h == d || strings.HasSuffix(h, "."+d)) {
			return true
		}
	}
	return false
}

// warn logs the first requests for each of the first maxHostWarnings
// unknown hosts.
func (p *hostPolicy) warn(log *slog.Logger, host string) {
	h := hostOnly(host)
	p.mu.Lock()
	if p.warned[h] || len(p.warned) >= maxHostWarnings {
		p.mu.Unlock()
		return
	}
	p.warned[h] = true
	p.mu.Unlock()
	if p.strict {
		log.Warn("refusing requests for a host that is not one of this server's names; "+
			"add it to server.allowed_hosts (GOS3_ALLOWED_HOSTS) if clients use it", "host", h)
		return
	}
	log.Warn("request for a host that is not one of this server's names: only signed requests are served for it "+
		"(anonymous S3 access, the web UI and public metrics are refused, against DNS rebinding); "+
		"add it to server.allowed_hosts (GOS3_ALLOWED_HOSTS) if clients use it", "host", h)
}

// hostOnly returns the lower-case host of a Host header value (or URL
// host) without port, IPv6 brackets or trailing dot.
func hostOnly(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end > 0 {
			return h[1:end]
		}
		return h
	}
	if strings.Count(h, ":") == 1 {
		h, _, _ = strings.Cut(h, ":")
	}
	return strings.TrimSuffix(h, ".")
}

// hostIndependentPath reports whether a /_gos3/ endpoint is served for an
// unknown host (when server.allowed_hosts is empty): the admin API (every
// request is signed), info, health and ready (public by design and used by
// probes and discovery peers), and metrics unless they are public.
func hostIndependentPath(p string, st *config.Settings) bool {
	switch {
	case p == admin.Prefix || strings.HasPrefix(p, admin.Prefix+"/"):
		return true
	case p == InfoPath, p == HealthPath, p == ReadyPath:
		return true
	case p == MetricsPath:
		return !st.MetricsPublic
	}
	return false
}

// rejectHost answers a request for an unknown host with 421 Misdirected
// Request: a JSON error for gos3's own endpoints, an S3 XML error
// otherwise.
func (s *Server) rejectHost(w http.ResponseWriter, r *http.Request, strict bool) {
	h := hostOnly(r.Host)
	msg := fmt.Sprintf("this server does not answer to host %q; add it to server.allowed_hosts (GOS3_ALLOWED_HOSTS) if it is one of its names", h)
	if !strict {
		msg = fmt.Sprintf("host %q is not one of this server's names: only signed requests are served for it "+
			"(anonymous access and the web UI are refused); add it to server.allowed_hosts (GOS3_ALLOWED_HOSTS) if it is one of its names", h)
	}
	s.logSystem(r, http.StatusMisdirectedRequest)
	if p := r.URL.Path; p == "/_gos3" || strings.HasPrefix(p, SystemPrefix) {
		writeJSONError(w, http.StatusMisdirectedRequest, "MisdirectedRequest", msg)
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<Error><Code>MisdirectedRequest</Code><Message>")
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString("</Message></Error>")
	hd := w.Header()
	hd.Set("Content-Type", "application/xml")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMisdirectedRequest)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(b.String()))
	}
}

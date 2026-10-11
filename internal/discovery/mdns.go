package discovery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// mDNS settings (docs/discovery.md §2).
const (
	mdnsDomain        = "local."
	mdnsQueryTimeout  = 3 * time.Second
	mdnsBrowseEvery   = 30 * time.Second
	mdnsRefreshEvery  = 30 * time.Second
	mdnsMaxInstance   = 50
	mdnsMaxTXTFields  = 32
	mdnsEntriesBuffer = 64
	// mdnsMaxEntries bounds the distinct services kept from one browse.
	mdnsMaxEntries = 256
)

// debugWriter adapts the standard logger used by hashicorp/mdns to slog at
// debug level, so the library's chatter never reaches normal logs.
type debugWriter struct{ logger *slog.Logger }

func (w debugWriter) Write(p []byte) (int, error) {
	w.logger.Debug(strings.TrimSpace(clip(string(p), 512)))
	return len(p), nil
}

func quietLogger(l *slog.Logger) *log.Logger {
	return log.New(debugWriter{l}, "", 0)
}

// sanitizeLabel maps s to [A-Za-z0-9-] (other characters become '-'),
// trims leading/trailing '-' and truncates to n bytes.
func sanitizeLabel(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= n {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortID returns the first 6 characters of id.
func shortID(id string) string { return id[:min(6, len(id))] }

// mdnsInstanceName returns "<sanitised name ≤ 50>-<first 6 of id>".
func mdnsInstanceName(name, id string) string {
	n := sanitizeLabel(name, mdnsMaxInstance)
	if n == "" {
		n = "gos3"
	}
	return n + "-" + shortID(id)
}

// mdnsHostName returns "<os hostname>.local." (first label, sanitised).
func mdnsHostName(id string) string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = sanitizeLabel(h, 63)
	if h == "" {
		h = "gos3-" + shortID(id)
	}
	return h + "." + mdnsDomain
}

// buildMDNSZone builds the DNS-SD records advertising info. The returned
// key changes whenever any advertised value changes.
func buildMDNSZone(info Info) (*mdns.MDNSService, string, error) {
	if !validID(info.ID) {
		return nil, "", errors.New("server ID unknown")
	}
	addrs := interfaceIPs(true)
	if len(addrs) == 0 {
		return nil, "", errors.New("no non-loopback interface address")
	}
	scheme, port, ok := portOf(mdnsBaseURL(info, addrs))
	if !ok {
		return nil, "", errors.New("no S3 endpoint URL to advertise")
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, net.IP(a.AsSlice()))
	}
	api := cleanField(info.APIVersion, 16)
	if api == "" {
		api = "v1"
	}
	txt := []string{
		"id=" + info.ID,
		"name=" + cleanField(info.Name, 200),
		"v=" + cleanField(info.Version, 200),
		"api=" + api,
		"region=" + cleanField(info.Region, 200),
		"scheme=" + scheme,
		"info=" + InfoPath,
	}
	instance := mdnsInstanceName(info.Name, info.ID)
	host := mdnsHostName(info.ID)
	svc, err := mdns.NewMDNSService(instance, MDNSService, mdnsDomain, host, port, ips, txt)
	if err != nil {
		return nil, "", err
	}
	key := fmt.Sprintf("%s|%s|%d|%v|%s", instance, host, port, addrs, strings.Join(txt, "|"))
	return svc, key, nil
}

// mdnsBaseURL picks the endpoint whose scheme and port the mDNS records
// advertise together with the interface addresses addrs: the first endpoint
// served directly at one of those addresses (the listener), not
// Endpoints.S3, which is server.public_url when one is set and may live on
// another host and port (e.g. https on 443 at a reverse proxy). Without
// such an endpoint it falls back to Endpoints.S3, then Endpoints.All[0].
func mdnsBaseURL(info Info, addrs []netip.Addr) string {
	for _, u := range append(slices.Clone(info.Endpoints.All), info.Endpoints.S3) {
		_, host, _, ok := urlHostPort(u)
		if !ok {
			continue
		}
		if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && slices.Contains(addrs, ip.Unmap()) {
			return u
		}
	}
	if info.Endpoints.S3 != "" || len(info.Endpoints.All) == 0 {
		return info.Endpoints.S3
	}
	return info.Endpoints.All[0]
}

// mdnsAdvertiser keeps an mDNS responder in sync with the server's Info.
type mdnsAdvertiser struct {
	self    func() Info
	logger  *slog.Logger
	server  *mdns.Server
	key     string
	lastErr string
}

// refresh (re)starts the responder when the advertised data changed.
func (a *mdnsAdvertiser) refresh() {
	svc, key, err := buildMDNSZone(a.self())
	if err == nil && key == a.key && a.server != nil {
		return
	}
	a.shutdown()
	if err == nil {
		a.server, err = mdns.NewServer(&mdns.Config{Zone: svc, Logger: quietLogger(a.logger)})
	}
	if err != nil {
		a.server = nil
		if msg := err.Error(); msg != a.lastErr {
			a.logger.Warn("mDNS advertising unavailable", "err", err)
			a.lastErr = msg
		} else {
			a.logger.Debug("mDNS advertising unavailable", "err", err)
		}
		return
	}
	a.key, a.lastErr = key, ""
	a.logger.Debug("mDNS advertising", "service", MDNSService)
}

func (a *mdnsAdvertiser) shutdown() {
	if a.server != nil {
		_ = a.server.Shutdown()
		a.server = nil
		a.key = ""
	}
}

// run refreshes the advertisement every 30 s until ctx is done, then
// withdraws it.
func (a *mdnsAdvertiser) run(ctx context.Context) {
	defer a.shutdown()
	t := time.NewTicker(mdnsRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh()
		}
	}
}

// ipFamilies reports whether IPv4 / IPv6 multicast appears usable.
func ipFamilies() (v4, v6 bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return true, false
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ipn.IP.To4() != nil {
					v4 = true
				} else {
					v6 = true
				}
			}
		}
	}
	if v6 {
		c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
		if err != nil {
			v6 = false
		} else {
			_ = c.Close()
		}
	}
	return v4, v6
}

// mdnsQuery runs one DNS-SD browse for _gos3._tcp lasting timeout and, once
// it has finished, feeds the results to out as candidates.
func mdnsQuery(ctx context.Context, timeout time.Duration, logger *slog.Logger, selfID string, out sink) error {
	v4, v6 := ipFamilies()
	if !v4 && !v6 {
		return errors.New("no multicast-capable interface")
	}
	entries := make(chan *mdns.ServiceEntry, mdnsEntriesBuffer)
	// hashicorp/mdns keeps rewriting a ServiceEntry after delivering it (a
	// later answer for the same service, e.g. the same reply received on a
	// second interface, updates its fields in the query goroutine), so only
	// collect the pointers while the query runs and read the entries once
	// QueryContext has returned and nothing writes to them any more.
	var got []*mdns.ServiceEntry
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := make(map[*mdns.ServiceEntry]bool)
		for e := range entries {
			if e != nil && !seen[e] && len(got) < mdnsMaxEntries {
				seen[e] = true
				got = append(got, e)
			}
		}
	}()
	err := mdns.QueryContext(ctx, &mdns.QueryParam{
		Service:     MDNSService,
		Domain:      "local",
		Timeout:     timeout,
		Entries:     entries,
		DisableIPv4: !v4,
		DisableIPv6: !v6,
		Logger:      quietLogger(logger),
	})
	// QueryContext has returned, so nothing sends on or writes to entries
	// any more.
	close(entries)
	<-done
	for _, e := range got {
		c, ok := mdnsCandidate(e, selfID)
		if !ok {
			continue
		}
		// Like beacons, mDNS answers are unauthenticated and link-local:
		// only probe addresses on this host's links.
		c.urls = slices.DeleteFunc(c.urls, func(u string) bool {
			_, host, _, ok := urlHostPort(u)
			ip, err := netip.ParseAddr(host)
			return !ok || err != nil || !hostNets.onLink(ip)
		})
		if len(c.urls) > 0 {
			out.candidate(c)
		}
	}
	return err
}

// mdnsCandidate converts a DNS-SD result into a probe candidate.
func mdnsCandidate(e *mdns.ServiceEntry, selfID string) (candidate, bool) {
	if e == nil || e.Port <= 0 || e.Port > 65535 {
		return candidate{}, false
	}
	fields := make(map[string]string)
	for i, f := range e.InfoFields {
		if i >= mdnsMaxTXTFields {
			break
		}
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[strings.ToLower(k)] = v
		}
	}
	id := fields["id"]
	if id != "" && (!validID(id) || id == selfID) {
		return candidate{}, false
	}
	scheme := "http"
	if strings.EqualFold(fields["scheme"], "https") {
		scheme = "https"
	}
	c := candidate{
		id:      id,
		name:    fields["name"],
		version: fields["v"],
		region:  fields["region"],
		source:  SourceMDNS,
	}
	if c.name == "" {
		inst, _, _ := strings.Cut(e.Name, ".")
		c.name = inst
	}
	add := func(ip net.IP, zone string) {
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return
		}
		host := ip.String()
		if zone != "" && ip.To4() == nil && ip.IsLinkLocalUnicast() {
			host += "%" + zone
		}
		if u, err := NormalizeBaseURL(baseURL(scheme, host, e.Port)); err == nil {
			c.urls = append(c.urls, u)
		}
	}
	add(e.AddrV4, "") // IPv4 preferred
	if e.AddrV6IPAddr != nil {
		add(e.AddrV6IPAddr.IP, e.AddrV6IPAddr.Zone)
	}
	if len(c.urls) == 0 && e.Addr != nil {
		add(e.Addr, "")
	}
	if len(c.urls) == 0 {
		return candidate{}, false
	}
	return c, true
}

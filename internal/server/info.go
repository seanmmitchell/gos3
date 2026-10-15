package server

import (
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/admin"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/webui"
)

// Info returns this server's public identity document (GET /_gos3/info,
// discovery announcements, admin whoami/info). It reflects the current
// settings (name, region, public URL, discovery/lifecycle switches) and the
// bound listen address. It is cheap and safe for concurrent use: the
// interface enumeration behind Endpoints.All is cached for 30 s.
func (s *Server) Info() discovery.Info {
	st := s.settings.Get()
	all := s.advertisedURLs(st.PublicURL)
	base := s.URL()
	if len(all) > 0 {
		base = all[0]
	}
	base = strings.TrimSuffix(base, "/")

	features := []string{"versioning", "object-lock", "multipart"}
	ep := discovery.Endpoints{
		S3:  base,
		All: all,
	}
	if s.cfg.Admin.Enabled {
		ep.Admin = base + admin.Prefix
		features = append(features, "admin")
	}
	if s.UIBuilt() {
		ep.UI = base + webui.Prefix
		features = append(features, "ui")
	}
	if st.Discovery.Enabled {
		features = append(features, "discovery")
	}
	if st.Lifecycle.Enabled {
		features = append(features, "lifecycle")
	}
	return discovery.Info{
		Service:    discovery.ServiceName,
		ID:         s.serverID,
		Name:       st.Name,
		Version:    s.version,
		APIVersion: "v1",
		Region:     st.Region,
		Endpoints:  ep,
		Features:   features,
		StartedAt:  s.startedAt,
	}
}

// advertisedURLs returns discovery.SelfAdvertisedURLs for the bound (or
// configured) listen address and publicURL, cached for urlCacheTTL.
func (s *Server) advertisedURLs(publicURL string) []string {
	addr := s.Addr()
	if addr == "" {
		addr = s.cfg.Server.Address
	}
	key := addr + "\x00" + publicURL
	now := time.Now()
	s.urlMu.Lock()
	defer s.urlMu.Unlock()
	if s.urlKey != key || now.After(s.urlExpires) {
		s.urls = discovery.SelfAdvertisedURLs(addr, publicURL, s.tlsConfig != nil)
		s.urlKey, s.urlExpires = key, now.Add(urlCacheTTL)
	}
	return append([]string(nil), s.urls...)
}

// applySettings is the config.Store subscriber: it applies changed runtime
// settings to the subsystems that do not read the store themselves. (The S3
// and admin handlers read region, domains, CORS and the access-log switch
// from the store on every request; Info reads name, region and public URL.)
func (s *Server) applySettings(old, cur config.Settings) {
	if old.Logging.Level != cur.Logging.Level || old.Logging.Format != cur.Logging.Format {
		s.logControl.Set(cur.Logging.Level, cur.Logging.Format)
		if s.logControl == nil {
			s.log.Warn("logging settings changed but this logger cannot be reconfigured at run time; restart to apply")
		}
		s.log.Info("settings: logging changed", "level", cur.Logging.Level, "format", cur.Logging.Format)
	}
	if old.Logging.AccessLog != cur.Logging.AccessLog {
		s.log.Info("settings: access log changed", "enabled", cur.Logging.AccessLog)
	}
	if !discoveryEqual(old.Discovery, cur.Discovery) {
		if err := s.disc.Reconfigure(cur.Discovery); err != nil {
			s.log.Error("settings: applying discovery settings failed", "err", err)
		} else {
			s.log.Info("settings: discovery reconfigured", "enabled", cur.Discovery.Enabled)
		}
	}
	if old.Lifecycle != cur.Lifecycle {
		s.lifecycle.Reconfigure(time.Duration(cur.Lifecycle.IntervalSeconds)*time.Second, cur.Lifecycle.Enabled)
		s.log.Info("settings: lifecycle worker reconfigured",
			"enabled", cur.Lifecycle.Enabled, "interval", time.Duration(cur.Lifecycle.IntervalSeconds)*time.Second)
	}
	if old.Region != cur.Region {
		// With auth.strict_region, signatures must now use the new region.
		s.verifier.SetRegion(cur.Region)
	}
}

func discoveryEqual(a, b discovery.Config) bool {
	return a.Enabled == b.Enabled && a.Advertise == b.Advertise && a.MDNS == b.MDNS && a.Beacon == b.Beacon &&
		a.BeaconGroup == b.BeaconGroup && a.BeaconInterval == b.BeaconInterval && a.PeerTTL == b.PeerTTL &&
		a.AllowScan == b.AllowScan && a.MaxScanHosts == b.MaxScanHosts &&
		strings.Join(a.Seeds, "\n") == strings.Join(b.Seeds, "\n") &&
		strings.Join(a.DNSSRV, "\n") == strings.Join(b.DNSSRV, "\n")
}

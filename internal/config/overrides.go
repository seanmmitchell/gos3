package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// SettingOverride is a runtime setting whose value persisted in
// settings.json differs from the static configuration (defaults < file <
// GOS3_* env < flags). settings.json wins: it holds the changes made through
// the admin API and the web UI.
type SettingOverride struct {
	Key        string // YAML key of the static configuration, e.g. "discovery.enabled"
	Configured string // value from the static configuration
	Persisted  string // value from settings.json (the effective one)
	// Explicit is set when the configured value is not the default, i.e. it
	// was set by the config file, a GOS3_* variable or a flag and is being
	// ignored.
	Explicit bool
}

// settingField is one runtime setting: its YAML key and a getter.
type settingField struct {
	key string
	get func(*Settings) any
}

// settingFields lists every runtime setting in YAML key order.
var settingFields = []settingField{
	{"server.name", func(s *Settings) any { return s.Name }},
	{"server.region", func(s *Settings) any { return s.Region }},
	{"server.public_url", func(s *Settings) any { return s.PublicURL }},
	{"server.domains", func(s *Settings) any { return s.Domains }},
	{"server.public_info", func(s *Settings) any { return s.PublicInfo }},
	{"ui.redirect_root", func(s *Settings) any { return s.UIRedirect }},
	{"cors.allowed_origins", func(s *Settings) any { return s.CORS.AllowedOrigins }},
	{"cors.allowed_headers", func(s *Settings) any { return s.CORS.AllowedHeaders }},
	{"cors.expose_headers", func(s *Settings) any { return s.CORS.ExposeHeaders }},
	{"cors.max_age_seconds", func(s *Settings) any { return s.CORS.MaxAgeSeconds }},
	{"discovery.enabled", func(s *Settings) any { return s.Discovery.Enabled }},
	{"discovery.advertise", func(s *Settings) any { return s.Discovery.Advertise }},
	{"discovery.mdns", func(s *Settings) any { return s.Discovery.MDNS }},
	{"discovery.beacon", func(s *Settings) any { return s.Discovery.Beacon }},
	{"discovery.beacon_group", func(s *Settings) any { return s.Discovery.BeaconGroup }},
	{"discovery.beacon_interval", func(s *Settings) any { return s.Discovery.BeaconInterval }},
	{"discovery.seeds", func(s *Settings) any { return s.Discovery.Seeds }},
	{"discovery.dns_srv", func(s *Settings) any { return s.Discovery.DNSSRV }},
	{"discovery.peer_ttl", func(s *Settings) any { return s.Discovery.PeerTTL }},
	{"discovery.allow_scan", func(s *Settings) any { return s.Discovery.AllowScan }},
	{"discovery.max_scan_hosts", func(s *Settings) any { return s.Discovery.MaxScanHosts }},
	{"logging.level", func(s *Settings) any { return s.Logging.Level }},
	{"logging.format", func(s *Settings) any { return s.Logging.Format }},
	{"logging.access_log", func(s *Settings) any { return s.Logging.AccessLog }},
	{"lifecycle.enabled", func(s *Settings) any { return s.Lifecycle.Enabled }},
	{"lifecycle.interval", func(s *Settings) any { return time.Duration(s.Lifecycle.IntervalSeconds) * time.Second }},
	{"metrics.public", func(s *Settings) any { return s.MetricsPublic }},
	{"compression.mode", func(s *Settings) any { return s.Compression.Mode }},
	{"compression.level", func(s *Settings) any { return s.Compression.Level }},
	{"compression.min_size", func(s *Settings) any { return s.Compression.MinSize }},
	{"compression.skip_content_types", func(s *Settings) any { return s.Compression.SkipContentTypes }},
}

// PersistedOverrides lists the runtime settings whose value in persisted
// (the store loaded from settings.json, see LoadStore) differs from the
// static configuration cfg, in YAML key order. Servers log the explicit
// ones at startup so that a flag or variable that has no effect is not
// ignored silently.
func PersistedOverrides(cfg Config, persisted Settings) []SettingOverride {
	configured := SettingsFrom(cfg)
	def := Default()
	def.Normalize()
	defaults := SettingsFrom(def)
	persisted = normalizeSettings(cloneSettings(persisted))
	var out []SettingOverride
	for _, f := range settingFields {
		c, p := f.get(&configured), f.get(&persisted)
		if reflect.DeepEqual(c, p) {
			continue
		}
		out = append(out, SettingOverride{
			Key:        f.key,
			Configured: formatSetting(c),
			Persisted:  formatSetting(p),
			Explicit:   !reflect.DeepEqual(c, f.get(&defaults)),
		})
	}
	return out
}

// formatSetting renders a setting value for logs and `gos3 config print`.
func formatSetting(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []string:
		q := make([]string, len(v))
		for i, s := range v {
			q[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]"
	default:
		return fmt.Sprint(v)
	}
}

// ApplySettings stores runtime settings s into the static configuration
// fields they are derived from (the inverse of SettingsFrom), e.g. to show
// the effective configuration once settings.json overrides it.
func ApplySettings(c *Config, s Settings) {
	s = cloneSettings(s)
	c.Server.Name = s.Name
	c.Server.Region = s.Region
	c.Server.PublicURL = s.PublicURL
	c.Server.Domains = s.Domains
	c.Server.PublicInfo = s.PublicInfo
	c.UI.RedirectRoot = s.UIRedirect
	c.CORS = s.CORS
	c.Discovery = s.Discovery
	c.Logging = s.Logging
	c.Lifecycle.Enabled = s.Lifecycle.Enabled
	c.Lifecycle.Interval = time.Duration(s.Lifecycle.IntervalSeconds) * time.Second
	c.Metrics.Public = s.MetricsPublic
	c.Compression = CompressionConfig{
		Mode:             s.Compression.Mode,
		Level:            s.Compression.Level,
		MinSize:          s.Compression.MinSize,
		SkipContentTypes: s.Compression.SkipContentTypes,
	}
}

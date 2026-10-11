package config

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// EnvConfig names the YAML configuration file (overridden by --config).
const EnvConfig = "GOS3_CONFIG"

// EnvVar documents one GOS3_* environment variable.
type EnvVar struct {
	Name        string // e.g. "GOS3_ADDRESS"
	Key         string // YAML key it overrides, e.g. "server.address" ("" for GOS3_CONFIG)
	Description string // value format / meaning
	set         func(c *Config, v string) error
}

// envVars is the full list of supported environment variables, in display
// order. Every configuration key has one. Lists are comma-separated;
// booleans accept 1/0, true/false, t/f, yes/no and on/off; durations use Go
// syntax ("30s", "1h"); sizes accept unit suffixes ("5GiB", see ParseSize).
var envVars = []EnvVar{
	{EnvConfig, "", "path of the YAML config file", nil},
	{"GOS3_ADDRESS", "server.address", "listen address, [host]:port", str(func(c *Config) *string { return &c.Server.Address })},
	{"GOS3_NAME", "server.name", "server name (default: host name)", str(func(c *Config) *string { return &c.Server.Name })},
	{"GOS3_REGION", "server.region", "region reported to clients", str(func(c *Config) *string { return &c.Server.Region })},
	{"GOS3_PUBLIC_URL", "server.public_url", "externally reachable base URL", str(func(c *Config) *string { return &c.Server.PublicURL })},
	{"GOS3_DOMAINS", "server.domains", "virtual-host domains, comma-separated", list(func(c *Config) *[]string { return &c.Server.Domains })},
	{"GOS3_TLS_CERT", "server.tls_cert", "TLS certificate (PEM) file", str(func(c *Config) *string { return &c.Server.TLSCert })},
	{"GOS3_TLS_KEY", "server.tls_key", "TLS private key (PEM) file", str(func(c *Config) *string { return &c.Server.TLSKey })},
	{"GOS3_MAX_OBJECT_SIZE", "server.max_object_size", "single PUT limit, bytes or 5GiB-style", size(func(c *Config) *int64 { return &c.Server.MaxObjectSize })},
	{"GOS3_PUBLIC_INFO", "server.public_info", "serve /_gos3/info without auth (bool)", boolean(func(c *Config) *bool { return &c.Server.PublicInfo })},
	{"GOS3_SHUTDOWN_TIMEOUT", "server.shutdown_timeout", "graceful shutdown timeout (duration)", duration(func(c *Config) *time.Duration { return &c.Server.ShutdownTimeout })},
	{"GOS3_TRUST_PROXY_HEADERS", "server.trust_proxy_headers", "honour X-Forwarded-For/-Proto (bool)", boolean(func(c *Config) *bool { return &c.Server.TrustProxyHeaders })},
	{"GOS3_TRUSTED_PROXIES", "server.trusted_proxies", "proxies whose X-Forwarded-* are honoured, CIDRs or IPs, comma-separated", list(func(c *Config) *[]string { return &c.Server.TrustedProxies })},
	{"GOS3_DATA", "storage.path", "data directory", str(func(c *Config) *string { return &c.Storage.Path })},
	{"GOS3_STORAGE_BACKEND", "storage.backend", "fs or memory", str(func(c *Config) *string { return &c.Storage.Backend })},
	{"GOS3_FSYNC", "storage.fsync_data", "fsync object data on commit (bool)", boolean(func(c *Config) *bool { return &c.Storage.FsyncData })},
	{"GOS3_ROOT_ACCESS_KEY", "auth.root_access_key", "root access key", str(func(c *Config) *string { return &c.Auth.RootAccessKey })},
	{"GOS3_ROOT_SECRET_KEY", "auth.root_secret_key", "root secret key", str(func(c *Config) *string { return &c.Auth.RootSecretKey })},
	{"GOS3_STRICT_REGION", "auth.strict_region", "reject signatures for other regions (bool)", boolean(func(c *Config) *bool { return &c.Auth.StrictRegion })},
	{"GOS3_UI_ENABLED", "ui.enabled", "serve the embedded web UI (bool)", boolean(func(c *Config) *bool { return &c.UI.Enabled })},
	{"GOS3_UI_REDIRECT", "ui.redirect_root", "redirect browsers from / to the UI (bool)", boolean(func(c *Config) *bool { return &c.UI.RedirectRoot })},
	{"GOS3_CORS_ORIGINS", "cors.allowed_origins", "allowed origins, comma-separated (\"*\" = any)", list(func(c *Config) *[]string { return &c.CORS.AllowedOrigins })},
	{"GOS3_CORS_ALLOWED_HEADERS", "cors.allowed_headers", "allowed request headers, comma-separated", list(func(c *Config) *[]string { return &c.CORS.AllowedHeaders })},
	{"GOS3_CORS_EXPOSE_HEADERS", "cors.expose_headers", "exposed response headers, comma-separated", list(func(c *Config) *[]string { return &c.CORS.ExposeHeaders })},
	{"GOS3_CORS_MAX_AGE", "cors.max_age_seconds", "preflight cache seconds", integer(func(c *Config) *int { return &c.CORS.MaxAgeSeconds })},
	{"GOS3_DISCOVERY_ENABLED", "discovery.enabled", "enable discovery (bool)", boolean(func(c *Config) *bool { return &c.Discovery.Enabled })},
	{"GOS3_DISCOVERY_ADVERTISE", "discovery.advertise", "announce this server (bool)", boolean(func(c *Config) *bool { return &c.Discovery.Advertise })},
	{"GOS3_DISCOVERY_MDNS", "discovery.mdns", "use mDNS (bool)", boolean(func(c *Config) *bool { return &c.Discovery.MDNS })},
	{"GOS3_DISCOVERY_BEACON", "discovery.beacon", "use the UDP beacon (bool)", boolean(func(c *Config) *bool { return &c.Discovery.Beacon })},
	{"GOS3_DISCOVERY_BEACON_GROUP", "discovery.beacon_group", "beacon multicast group ip:port", str(func(c *Config) *string { return &c.Discovery.BeaconGroup })},
	{"GOS3_DISCOVERY_BEACON_INTERVAL", "discovery.beacon_interval", "beacon interval (duration)", duration(func(c *Config) *time.Duration { return &c.Discovery.BeaconInterval })},
	{"GOS3_DISCOVERY_SEEDS", "discovery.seeds", "seed base URLs, comma-separated", list(func(c *Config) *[]string { return &c.Discovery.Seeds })},
	{"GOS3_DISCOVERY_DNS_SRV", "discovery.dns_srv", "DNS SRV domains, comma-separated", list(func(c *Config) *[]string { return &c.Discovery.DNSSRV })},
	{"GOS3_DISCOVERY_PEER_TTL", "discovery.peer_ttl", "peer expiry (duration)", duration(func(c *Config) *time.Duration { return &c.Discovery.PeerTTL })},
	{"GOS3_DISCOVERY_ALLOW_SCAN", "discovery.allow_scan", "allow admin network scans (bool)", boolean(func(c *Config) *bool { return &c.Discovery.AllowScan })},
	{"GOS3_DISCOVERY_MAX_SCAN_HOSTS", "discovery.max_scan_hosts", "host limit per scan", integer(func(c *Config) *int { return &c.Discovery.MaxScanHosts })},
	{"GOS3_LOG_LEVEL", "logging.level", "debug, info, warn or error", str(func(c *Config) *string { return &c.Logging.Level })},
	{"GOS3_LOG_FORMAT", "logging.format", "text or json", str(func(c *Config) *string { return &c.Logging.Format })},
	{"GOS3_ACCESS_LOG", "logging.access_log", "log every request at info (bool)", boolean(func(c *Config) *bool { return &c.Logging.AccessLog })},
	{"GOS3_LIFECYCLE_ENABLED", "lifecycle.enabled", "run the lifecycle worker (bool)", boolean(func(c *Config) *bool { return &c.Lifecycle.Enabled })},
	{"GOS3_LIFECYCLE_INTERVAL", "lifecycle.interval", "lifecycle pass interval (duration)", duration(func(c *Config) *time.Duration { return &c.Lifecycle.Interval })},
	{"GOS3_METRICS_ENABLED", "metrics.enabled", "serve /_gos3/metrics (bool)", boolean(func(c *Config) *bool { return &c.Metrics.Enabled })},
	{"GOS3_METRICS_PUBLIC", "metrics.public", "serve metrics without auth (bool)", boolean(func(c *Config) *bool { return &c.Metrics.Public })},
}

// EnvVars returns the documented GOS3_* environment variables.
func EnvVars() []EnvVar { return append([]EnvVar(nil), envVars...) }

// Keys returns the configuration keys accepted by Config.Set and Override
// (the YAML keys, e.g. "server.address"), in display order.
func Keys() []string {
	keys := make([]string, 0, len(envVars))
	for _, e := range envVars {
		if e.Key != "" {
			keys = append(keys, e.Key)
		}
	}
	return keys
}

// Set sets the configuration key (a YAML key such as "server.address", or
// the name of its GOS3_* environment variable) to value, parsed exactly like
// the environment variable: lists are comma-separated, booleans, durations
// and sizes are parsed (see EnvVars). The CLI uses it to apply flags.
// Call Normalize and Validate afterwards.
func (c *Config) Set(key, value string) error {
	for _, e := range envVars {
		if e.set != nil && (e.Key == key || e.Name == key) {
			if err := e.set(c, value); err != nil {
				return fmt.Errorf("%s: %w", e.Key, err)
			}
			return nil
		}
	}
	return fmt.Errorf("unknown configuration key %q", key)
}

// WriteEnvTable writes the environment variable reference as an aligned
// table (variable, YAML key, description).
func WriteEnvTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "VARIABLE\tYAML KEY\tDESCRIPTION")
	for _, e := range envVars {
		key := e.Key
		if key == "" {
			key = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, key, e.Description)
	}
	return tw.Flush()
}

// applyEnv applies every set GOS3_* variable to c. Empty values are applied
// too (e.g. GOS3_DOMAINS= clears the list), except for GOS3_CONFIG.
func applyEnv(c *Config, lookup func(string) (string, bool)) error {
	for _, e := range envVars {
		if e.set == nil {
			continue
		}
		v, ok := lookup(e.Name)
		if !ok {
			continue
		}
		if err := e.set(c, v); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	return nil
}

func str(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *field(c) = strings.TrimSpace(v); return nil }
}

func list(field func(*Config) *[]string) func(*Config, string) error {
	return func(c *Config, v string) error { *field(c) = SplitList(v); return nil }
}

func boolean(field func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := ParseBool(v)
		if err != nil {
			return err
		}
		*field(c) = b
		return nil
	}
}

func integer(field func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid integer %q", v)
		}
		*field(c) = n
		return nil
	}
}

func size(field func(*Config) *int64) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := ParseSize(v)
		if err != nil {
			return err
		}
		*field(c) = n
		return nil
	}
}

func duration(field func(*Config) *time.Duration) func(*Config, string) error {
	return func(c *Config, v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid duration %q (e.g. \"30s\", \"1h\")", v)
		}
		*field(c) = d
		return nil
	}
}

// ParseBool parses a boolean: 1/0, t/f, true/false, yes/no, y/n and on/off
// (case-insensitive, surrounding spaces ignored).
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q (expected true or false)", v)
}

// SplitList splits a comma-separated list, trimming spaces and dropping
// empty entries. An empty string yields an empty (non-nil) list.
func SplitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

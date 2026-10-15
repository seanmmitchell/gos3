package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// Defaults that are not obvious from Default().
const (
	DefaultAddress         = ":9000"
	DefaultRegion          = "us-east-1"
	DefaultDataPath        = "./data"
	DefaultMaxObjectSize   = int64(5) << 30 // 5 GiB, as AWS
	MaxMaxObjectSize       = int64(5) << 40 // 5 TiB, the S3 object size limit
	DefaultShutdownTimeout = 30 * time.Second
	DefaultLifecycleEvery  = time.Hour
	DefaultCORSMaxAge      = 3600
	// DefaultMaxConnections is the default server.max_connections (no
	// limit: the process's file descriptor limit applies).
	DefaultMaxConnections = 0
	// DefaultCompressionLevel is the default compression.level (DEFLATE
	// level 1: fastest, still about 3x on source code and text).
	DefaultCompressionLevel = compress.DefaultLevel
	// DefaultCompressionMinSize is the default compression.min_size: the
	// smallest object that can save a file system block.
	DefaultCompressionMinSize = 4096
	// MinRootSecretLen is the root secret key length below which gos3
	// warns at startup (a short secret can be brute-forced offline from a
	// single captured signed request).
	MinRootSecretLen = 16

	// BackendFS, BackendMemory and BackendS3 (a bucket of a remote S3
	// service, see S3Config and docs/backings.md) are the values of
	// StorageConfig.Backend and BackingConfig.Backend.
	BackendFS     = "fs"
	BackendMemory = "memory"
	BackendS3     = "s3"

	// DefaultBackingName names the backing configured by storage.backend
	// and storage.path.
	DefaultBackingName = "default"
	// Replication defaults and limits (storage.replication).
	DefaultReplicationWorkers = 2
	MaxReplicationWorkers     = 64
	DefaultHealthInterval     = 10 * time.Second
	DefaultCutoverTimeout     = 60 * time.Second
	// MaxBackings bounds storage.backings.
	MaxBackings = 64

	// Replication modes (storage.replication.default_mode).
	ReplicationSingle        = "single"
	ReplicationActivePassive = "active-passive"
	ReplicationActiveActive  = "active-active"

	// SystemDirName is the directory under the data path holding system
	// state (iam.json, settings.json, server-id, root-credentials.json and
	// the fs backend's metadata).
	SystemDirName = ".gos3"
	// SettingsFileName is the persisted runtime settings file in SystemDir.
	SettingsFileName = "settings.json"
)

// DefaultExposeHeaders are the default global CORS expose headers. "*" lets
// browsers read every response header (including x-amz-meta-*) for requests
// without credentials, which is how the web UI and SDKs call the API; the
// explicit names cover credentialed requests, for which "*" is literal.
var DefaultExposeHeaders = []string{
	"*",
	"ETag", "Content-Length", "Content-Type", "Content-Range", "Content-Disposition",
	"Content-Encoding", "Content-Language", "Cache-Control", "Expires", "Last-Modified",
	"Accept-Ranges",
	"x-amz-request-id", "x-amz-id-2", "x-amz-version-id", "x-amz-delete-marker",
	"x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme",
	"x-amz-checksum-sha1", "x-amz-checksum-sha256", "x-amz-checksum-type",
	"x-amz-mp-parts-count", "x-amz-tagging-count", "x-amz-storage-class",
	"x-amz-server-side-encryption", "x-amz-object-lock-mode",
	"x-amz-object-lock-retain-until-date", "x-amz-object-lock-legal-hold",
	"x-amz-expiration", "x-amz-restore", "x-amz-website-redirect-location",
	"x-amz-abort-date", "x-amz-abort-rule-id", "x-amz-bucket-region", "x-gos3-backing",
}

// DefaultSkipContentTypes are the media types stored uncompressed by
// default (compression.skip_content_types): formats that are compressed
// already, where DEFLATE would spend CPU for nothing.
var DefaultSkipContentTypes = []string{
	"image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/heic", "image/heif", "image/jxl",
	"video/*", "audio/*", "font/woff", "font/woff2",
	"application/zip", "application/gzip", "application/x-gzip", "application/zstd", "application/x-bzip2",
	"application/x-xz", "application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar",
	"application/x-lz4", "application/java-archive", "application/epub+zip",
	"application/vnd.openxmlformats-officedocument.*", "application/vnd.oasis.opendocument.*",
}

// Default returns the default configuration. Name defaults to the host name
// (truncated to MaxNameLen). Storage.Path is DefaultDataPath; note that Load
// only keeps it for the fs backend: a memory backend whose path is not
// configured explicitly is fully ephemeral.
func Default() Config {
	name, _ := os.Hostname()
	if len(name) > MaxNameLen {
		name = name[:MaxNameLen]
	}
	return Config{
		Server: ServerConfig{
			Address:         DefaultAddress,
			Name:            name,
			Region:          DefaultRegion,
			MaxObjectSize:   DefaultMaxObjectSize,
			PublicInfo:      true,
			ShutdownTimeout: DefaultShutdownTimeout,
			MaxConnections:  DefaultMaxConnections,
		},
		Storage: StorageConfig{
			Backend:   BackendFS,
			Path:      DefaultDataPath,
			FsyncData: true,
			Replication: ReplicationConfig{
				Workers:        DefaultReplicationWorkers,
				HealthInterval: DefaultHealthInterval,
				CutoverTimeout: DefaultCutoverTimeout,
			},
		},
		UI:    UIConfig{Enabled: defaultUIEnabled, RedirectRoot: true},
		Admin: AdminConfig{Enabled: true},
		CORS: CORSConfig{
			AllowedOrigins: []string{"*"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  append([]string(nil), DefaultExposeHeaders...),
			MaxAgeSeconds:  DefaultCORSMaxAge,
		},
		Discovery: discovery.DefaultConfig(),
		Logging:   LoggingConfig{Level: "info", Format: "text", AccessLog: true},
		Lifecycle: LifecycleConfig{Enabled: true, Interval: DefaultLifecycleEvery},
		Metrics:   MetricsConfig{Enabled: true},
		Compression: CompressionConfig{
			Mode:             CompressionOptional,
			Level:            DefaultCompressionLevel,
			MinSize:          DefaultCompressionMinSize,
			SkipContentTypes: slices.Clone(DefaultSkipContentTypes),
		},
	}
}

// Override sets one configuration key (a YAML key such as "server.address",
// see EnvVars) to a value in environment-variable syntax. The CLI turns its
// flags into Overrides, which Load applies last.
type Override struct {
	Key   string
	Value string
}

// Load returns the configuration from defaults < JSON/YAML file < environment
// (GOS3_*, with MinIO aliases) < overrides (CLI flags). path is the config file ("" =
// $GOS3_CONFIG, and no file when that is unset too). Unknown fields are
// rejected. The result is normalized (see Normalize) but not validated:
// call Validate before using it.
//
// storage.path defaults to DefaultDataPath for the fs backend only; with the
// memory and s3 backends it stays empty (system state is ephemeral) unless
// set explicitly.
//
// After the overrides, the empty fields of every s3 backing in
// storage.backings are filled from its GOS3_BACKING_<NAME>_* variables
// (see BackingEnv).
func Load(path string, overrides ...Override) (Config, error) {
	return load(path, os.LookupEnv, overrides)
}

func load(path string, lookup func(string) (string, bool), overrides []Override) (Config, error) {
	cfg := Default()
	// Normalize fills the backend-dependent default once all layers are in.
	cfg.Storage.Path = ""
	if path == "" {
		path, _ = lookup(EnvConfig)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		if err := decodeFile(path, data, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, lookup); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	for _, o := range overrides {
		if err := cfg.Set(o.Key, o.Value); err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
	}
	if err := cfg.Storage.applyBackingEnv(lookup); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}

// decodeYAML decodes a YAML document over cfg, rejecting unknown fields and
// multiple documents. An empty document leaves cfg unchanged.
func decodeYAML(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		var te *yaml.TypeError
		if errors.As(err, &te) {
			msgs := make([]string, len(te.Errors))
			for i, m := range te.Errors {
				msgs[i] = unknownFieldRe.ReplaceAllStringFunc(m, describeUnknownField)
			}
			return errors.New(strings.Join(msgs, "; "))
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("only one YAML document is allowed")
	}
	return nil
}

// unknownFieldRe matches yaml.v3's KnownFields message, e.g. "field adress
// not found in type config.ServerConfig".
var unknownFieldRe = regexp.MustCompile(`field (\S+) not found in type (\S+)`)

// yamlSections maps the Go types of Config sections to their YAML keys.
var yamlSections = map[string]string{
	"config.AdminConfig":       "admin",
	"config.ServerConfig":      "server",
	"config.StorageConfig":     "storage",
	"config.BackingConfig":     "storage.backings",
	"config.S3Config":          "s3",
	"config.ReplicationConfig": "storage.replication",
	"config.AuthConfig":        "auth",
	"config.UIConfig":          "ui",
	"config.CORSConfig":        "cors",
	"discovery.Config":         "discovery",
	"config.LoggingConfig":     "logging",
	"config.LifecycleConfig":   "lifecycle",
	"config.MetricsConfig":     "metrics",
	"config.CompressionConfig": "compression",
}

// describeUnknownField rewrites an unknown-field message in terms of YAML
// keys: `unknown key "adress" in section "server"`.
func describeUnknownField(m string) string {
	sub := unknownFieldRe.FindStringSubmatch(m)
	if sec, ok := yamlSections[sub[2]]; ok {
		return fmt.Sprintf("unknown key %q in section %q", sub[1], sec)
	}
	if sub[2] == "config.Config" {
		return fmt.Sprintf("unknown top-level key %q (expected server, storage, auth, ui, admin, cors, discovery, logging, lifecycle, metrics or compression)", sub[1])
	}
	return fmt.Sprintf("unknown key %q", sub[1])
}

// Normalize fills derived defaults (the data path of the fs backend, the
// address, region and maximum object size when empty or zero), lower-cases
// enumerations and trims list entries, dropping empty ones.
func (c *Config) Normalize() {
	if !c.Admin.Enabled {
		c.UI.Enabled = false
	}
	c.Storage.Backend = strings.ToLower(strings.TrimSpace(c.Storage.Backend))
	if c.Storage.Backend == "" {
		c.Storage.Backend = BackendFS
	}
	if c.Storage.Backend == BackendFS && c.Storage.Path == "" {
		c.Storage.Path = DefaultDataPath
	}
	for i := range c.Storage.Backings {
		b := &c.Storage.Backings[i]
		b.Name = strings.TrimSpace(b.Name)
		b.Backend = strings.ToLower(strings.TrimSpace(b.Backend))
		if b.Backend == "" {
			b.Backend = BackendFS
		}
	}
	c.Storage.DefaultBacking = strings.TrimSpace(c.Storage.DefaultBacking)
	c.normalizeS3()
	rp := &c.Storage.Replication
	rp.DefaultMode = strings.ToLower(strings.TrimSpace(rp.DefaultMode))
	rp.DefaultReplicas = trimList(rp.DefaultReplicas)
	if rp.Workers == 0 {
		rp.Workers = DefaultReplicationWorkers
	}
	if rp.HealthInterval == 0 {
		rp.HealthInterval = DefaultHealthInterval
	}
	if rp.CutoverTimeout == 0 {
		rp.CutoverTimeout = DefaultCutoverTimeout
	}
	if c.Server.Region == "" {
		c.Server.Region = DefaultRegion
	}
	if c.Server.Address == "" {
		c.Server.Address = DefaultAddress
	}
	if c.Server.MaxObjectSize == 0 {
		c.Server.MaxObjectSize = DefaultMaxObjectSize
	}
	c.Server.Domains = trimList(c.Server.Domains)
	c.Server.TrustedProxies = trimList(c.Server.TrustedProxies)
	c.Server.AllowedHosts = trimList(c.Server.AllowedHosts)
	for i, h := range c.Server.AllowedHosts {
		c.Server.AllowedHosts[i] = strings.TrimSuffix(strings.ToLower(h), ".")
	}
	c.CORS.AllowedOrigins = trimList(c.CORS.AllowedOrigins)
	c.CORS.AllowedHeaders = trimList(c.CORS.AllowedHeaders)
	c.CORS.ExposeHeaders = trimList(c.CORS.ExposeHeaders)
	c.Discovery.Seeds = trimList(c.Discovery.Seeds)
	c.Discovery.DNSSRV = trimList(c.Discovery.DNSSRV)
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
	c.Compression.Mode = strings.ToLower(strings.TrimSpace(c.Compression.Mode))
	if c.Compression.Mode == "" {
		c.Compression.Mode = CompressionOptional
	}
	if c.Compression.Level == 0 {
		c.Compression.Level = DefaultCompressionLevel
	}
	c.Compression.SkipContentTypes = trimList(c.Compression.SkipContentTypes)
}

func trimList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Validate checks the static configuration, including the runtime Settings
// derived from it (ValidateSettings). Errors name the offending YAML key,
// e.g. "server.address: ...".
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Server.Address)
	if err != nil || strings.ContainsAny(host, "/ ") {
		return fmt.Errorf("server.address: %q must be [host]:port (e.g. \":9000\")", c.Server.Address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("server.address: port %q must be 0-65535", port)
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return errors.New("server.tls_cert and server.tls_key must be set together")
	}
	if c.Server.MaxObjectSize < 0 || c.Server.MaxObjectSize > MaxMaxObjectSize {
		return fmt.Errorf("server.max_object_size: must be between 1 and %d bytes (5 TiB; 0 = default 5 GiB), got %d",
			MaxMaxObjectSize, c.Server.MaxObjectSize)
	}
	if c.Server.ShutdownTimeout < 0 {
		return fmt.Errorf("server.shutdown_timeout: must not be negative, got %s", c.Server.ShutdownTimeout)
	}
	if len(c.Server.TrustedProxies) > MaxListEntries {
		return fmt.Errorf("server.trusted_proxies: at most %d entries allowed", MaxListEntries)
	}
	if _, err := clientip.ParsePrefixes(c.Server.TrustedProxies); err != nil {
		return fmt.Errorf("server.trusted_proxies%w", err)
	}
	if c.Server.TrustProxyHeaders && len(c.Server.TrustedProxies) == 0 {
		return errors.New("server.trust_proxy_headers is on but server.trusted_proxies is empty: list the addresses " +
			"of your reverse proxies (e.g. [\"127.0.0.1\", \"10.0.0.5\"], GOS3_TRUSTED_PROXIES); otherwise any client " +
			"that reaches gos3 directly could choose its own aws:SourceIp and aws:SecureTransport with " +
			"X-Forwarded-For / X-Forwarded-Proto")
	}
	if err := validateAllowedHosts(c.Server.AllowedHosts); err != nil {
		return err
	}
	if c.Server.MaxConnections < 0 {
		return fmt.Errorf("server.max_connections: must not be negative (0 = no limit), got %d", c.Server.MaxConnections)
	}
	switch c.Storage.Backend {
	case BackendFS:
		if c.Storage.Path == "" {
			return errors.New("storage.path: required for the fs backend")
		}
	case BackendMemory:
	case BackendS3:
		if c.Storage.S3 == nil {
			return errNoS3
		}
	default:
		return fmt.Errorf("storage.backend: %q is invalid (expected fs, memory or s3)", c.Storage.Backend)
	}
	if c.Storage.Backend != BackendS3 && c.Storage.S3 != nil && !c.Storage.S3.IsZero() {
		return fmt.Errorf("storage.s3: only used with storage.backend s3 (storage.backend is %q)", c.Storage.Backend)
	}
	if err := c.validateBackings(); err != nil {
		return err
	}
	if err := validateRootCredentials(c.Auth.RootAccessKey, c.Auth.RootSecretKey); err != nil {
		return err
	}
	if IsDemoSecret(c.Auth.RootSecretKey) && !c.Auth.InsecureDemoCredentials {
		return fmt.Errorf("auth.root_secret_key: %q is a published example secret: anyone can sign requests with it, "+
			"including any website you visit (through your browser, which can reach this server). Choose your own "+
			"(\"gos3 keygen\" prints a random pair), or set %s=true for a throwaway local demo",
			c.Auth.RootSecretKey, EnvInsecureDemoCredentials)
	}
	if iv := c.Lifecycle.Interval; iv != 0 {
		if lo, hi := MinLifecycleSeconds*time.Second, MaxLifecycleSeconds*time.Second; iv < lo || iv > hi || iv%time.Second != 0 {
			return fmt.Errorf("lifecycle.interval: must be a whole number of seconds between %s and %s, got %s", lo, hi, iv)
		}
	}
	if err := ValidateSettings(SettingsFrom(c)); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return fmt.Errorf("%s: %s", yamlField(fe.Field), fe.Msg)
		}
		return err
	}
	return nil
}

// settingsYAMLKeys maps Settings JSON field paths to the YAML keys of the
// Config fields they are derived from (see SettingsFrom). Paths that are the
// same in both forms (e.g. "logging.level", "discovery.seeds") are absent.
var settingsYAMLKeys = map[string]string{
	"name":                         "server.name",
	"region":                       "server.region",
	"publicUrl":                    "server.public_url",
	"domains":                      "server.domains",
	"cors.allowedOrigins":          "cors.allowed_origins",
	"cors.allowedHeaders":          "cors.allowed_headers",
	"cors.exposeHeaders":           "cors.expose_headers",
	"cors.maxAgeSeconds":           "cors.max_age_seconds",
	"discovery.beaconGroup":        "discovery.beacon_group",
	"discovery.beaconInterval":     "discovery.beacon_interval",
	"discovery.peerTtl":            "discovery.peer_ttl",
	"discovery.dnsSrv":             "discovery.dns_srv",
	"discovery.maxScanHosts":       "discovery.max_scan_hosts",
	"lifecycle.intervalSeconds":    "lifecycle.interval",
	"compression.minSize":          "compression.min_size",
	"compression.skipContentTypes": "compression.skip_content_types",
}

// yamlField translates a FieldError path ("cors.allowedOrigins[2]") to the
// YAML key it came from ("cors.allowed_origins[2]").
func yamlField(field string) string {
	base, index := field, ""
	if i := strings.IndexByte(field, '['); i >= 0 {
		base, index = field[:i], field[i:]
	}
	if k, ok := settingsYAMLKeys[base]; ok {
		return k + index
	}
	return field
}

// validateRootCredentials applies iam's rules for supplied keys: both or
// neither; access key 3..128 of [A-Za-z0-9._-]; secret 8..128 printable
// ASCII without spaces.
func validateRootCredentials(ak, sk string) error {
	if ak == "" && sk == "" {
		return nil
	}
	if ak == "" || sk == "" {
		return errors.New("auth: root_access_key and root_secret_key (GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY) must be set together")
	}
	if len(ak) < 3 || len(ak) > 128 || strings.IndexFunc(ak, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) >= 0 {
		return errors.New("auth.root_access_key: must be 3-128 characters of A-Z a-z 0-9 . _ -")
	}
	if len(sk) < 8 || len(sk) > 128 || strings.IndexFunc(sk, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return errors.New("auth.root_secret_key: must be 8-128 printable ASCII characters without spaces")
	}
	return nil
}

// EnvInsecureDemoCredentials is the environment variable of
// auth.insecure_demo_credentials.
const EnvInsecureDemoCredentials = "GOS3_INSECURE_DEMO_CREDENTIALS"

// demoSecrets are root secret keys published in gos3's own documentation
// and demo files (README, docs, docker-compose.yml) and in those of other
// S3 servers, compared case-insensitively.
var demoSecrets = []string{
	"change-me-now",    // README / docs quickstart
	"gos3admin-secret", // docker-compose.yml demo
	"minioadmin",       // MinIO default
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", // AWS documentation example
	"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	"changeme", "change-me", "password", "secretkey", "secret-key",
}

// IsDemoSecret reports whether secret is a root secret key published in
// documentation or demo files (auth.insecure_demo_credentials is required
// to use one).
func IsDemoSecret(secret string) bool {
	for _, d := range demoSecrets {
		if strings.EqualFold(secret, d) {
			return true
		}
	}
	return false
}

// validateAllowedHosts checks server.allowed_hosts: "*" alone, or host
// names, "*.<domain>" wildcards and IP addresses (no scheme or port).
func validateAllowedHosts(hosts []string) error {
	if len(hosts) > MaxListEntries {
		return fmt.Errorf("server.allowed_hosts: at most %d entries allowed", MaxListEntries)
	}
	for i, h := range hosts {
		if h == "*" {
			if len(hosts) > 1 {
				return errors.New("server.allowed_hosts: \"*\" (any host) must be the only entry")
			}
			continue
		}
		ip := h
		if strings.HasPrefix(ip, "[") && strings.HasSuffix(ip, "]") {
			ip = ip[1 : len(ip)-1]
		}
		if a, err := netip.ParseAddr(ip); err == nil && a.Zone() == "" {
			continue
		}
		if name := strings.TrimPrefix(h, "*."); !ValidHostname(name) {
			return fmt.Errorf("server.allowed_hosts[%d]: %q is not a host name, \"*.<domain>\" wildcard or IP address (no scheme or port)", i, h)
		}
	}
	return nil
}

// SettingsFrom derives the initial runtime Settings from a static Config,
// substituting discovery defaults for zero values so the result passes
// ValidateSettings whenever the config is valid.
func SettingsFrom(c Config) Settings {
	d := c.Discovery
	def := discovery.DefaultConfig()
	if d.BeaconGroup == "" {
		d.BeaconGroup = def.BeaconGroup
	}
	if d.BeaconInterval == 0 {
		d.BeaconInterval = def.BeaconInterval
	}
	if d.PeerTTL == 0 {
		d.PeerTTL = max(3*d.BeaconInterval, def.PeerTTL)
	}
	if d.MaxScanHosts == 0 {
		d.MaxScanHosts = def.MaxScanHosts
	}
	interval := c.Lifecycle.Interval
	if interval == 0 {
		interval = DefaultLifecycleEvery
	}
	region := c.Server.Region
	if region == "" {
		region = DefaultRegion
	}
	comp := CompressionSettings{
		Mode:             c.Compression.Mode,
		Level:            c.Compression.Level,
		MinSize:          c.Compression.MinSize,
		SkipContentTypes: c.Compression.SkipContentTypes,
	}
	if comp.Mode == "" {
		comp.Mode = CompressionOptional
	}
	if comp.Level == 0 {
		comp.Level = DefaultCompressionLevel
	}
	s := Settings{
		Name:          c.Server.Name,
		Region:        region,
		PublicURL:     c.Server.PublicURL,
		Domains:       c.Server.Domains,
		PublicInfo:    c.Server.PublicInfo,
		UIRedirect:    c.UI.RedirectRoot,
		CORS:          c.CORS,
		Discovery:     d,
		Logging:       c.Logging,
		Lifecycle:     LifecycleSettings{Enabled: c.Lifecycle.Enabled, IntervalSeconds: int64(interval / time.Second)},
		MetricsPublic: c.Metrics.Public,
		Compression:   comp,
	}
	return normalizeSettings(cloneSettings(s))
}

// normalizeSettings replaces nil slices with empty ones so the JSON form uses
// [] rather than null.
func normalizeSettings(s Settings) Settings {
	nn := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	s.Domains = nn(s.Domains)
	s.CORS.AllowedOrigins = nn(s.CORS.AllowedOrigins)
	s.CORS.AllowedHeaders = nn(s.CORS.AllowedHeaders)
	s.CORS.ExposeHeaders = nn(s.CORS.ExposeHeaders)
	s.Discovery.Seeds = nn(s.Discovery.Seeds)
	s.Discovery.DNSSRV = nn(s.Discovery.DNSSRV)
	s.Compression.SkipContentTypes = nn(s.Compression.SkipContentTypes)
	return s
}

// SystemDir returns the system state directory (<data>/.gos3), or "" when the
// configuration is fully ephemeral (memory or s3 backend without a data
// path).
func (c Config) SystemDir() string {
	if c.Storage.Path == "" {
		return ""
	}
	return filepath.Join(c.Storage.Path, SystemDirName)
}

// SettingsPath returns the settings.json path, or "" when ephemeral.
func (c Config) SettingsPath() string {
	if d := c.SystemDir(); d != "" {
		return filepath.Join(d, SettingsFileName)
	}
	return ""
}

// ClientIP returns the client address resolver configured by
// server.trust_proxy_headers and server.trusted_proxies. It fails only for
// a configuration that does not pass Validate.
func (c Config) ClientIP() (clientip.Resolver, error) {
	proxies, err := clientip.ParsePrefixes(c.Server.TrustedProxies)
	if err != nil {
		return clientip.Resolver{}, fmt.Errorf("server.trusted_proxies%w", err)
	}
	return clientip.New(c.Server.TrustProxyHeaders, proxies), nil
}

// TLSEnabled reports whether the listener serves HTTPS.
func (c Config) TLSEnabled() bool { return c.Server.TLSCert != "" }

// RedactedValue is the placeholder Config.Redacted substitutes for secrets.
const RedactedValue = "<redacted>"

// Redacted returns a deep copy with secrets (the root secret key and the
// secret and encryption keys of s3 backings) replaced by "<redacted>", for
// printing and logging.
func (c Config) Redacted() Config {
	if c.Auth.RootSecretKey != "" {
		c.Auth.RootSecretKey = RedactedValue
	}
	c.Storage.S3 = c.Storage.S3.redacted()
	c.Storage.Backings = slices.Clone(c.Storage.Backings)
	for i := range c.Storage.Backings {
		c.Storage.Backings[i].S3 = c.Storage.Backings[i].S3.redacted()
	}
	c.Storage.Replication.DefaultReplicas = slices.Clone(c.Storage.Replication.DefaultReplicas)
	c.Server.Domains = slices.Clone(c.Server.Domains)
	c.Server.TrustedProxies = slices.Clone(c.Server.TrustedProxies)
	c.Server.AllowedHosts = slices.Clone(c.Server.AllowedHosts)
	c.CORS = cloneCORS(c.CORS)
	c.Discovery.Seeds = slices.Clone(c.Discovery.Seeds)
	c.Discovery.DNSSRV = slices.Clone(c.Discovery.DNSSRV)
	return c
}

// YAML renders the configuration as YAML (secrets included: call Redacted
// first for display).
func (c Config) YAML() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseLogLevel parses debug|info|warn|error.
func ParseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q (expected debug, info, warn or error)", s)
}

// sizeUnits maps size suffixes (upper case) to multipliers (powers of 1024).
var sizeUnits = map[string]int64{
	"": 1, "B": 1,
	"K": 1 << 10, "KB": 1 << 10, "KIB": 1 << 10,
	"M": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
	"G": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30,
	"T": 1 << 40, "TB": 1 << 40, "TIB": 1 << 40,
}

// ParseSize parses a byte size: a plain integer, or a (possibly fractional)
// number with a unit suffix K, M, G or T (powers of 1024; "KB"/"KiB" forms
// are accepted too), e.g. "5368709120", "5GiB", "1.5G".
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	num, unit := t, ""
	if i := strings.IndexFunc(t, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.') }); i >= 0 {
		num, unit = strings.TrimSpace(t[:i]), strings.ToUpper(strings.TrimSpace(t[i:]))
	}
	mult, ok := sizeUnits[unit]
	bad := fmt.Errorf("invalid size %q (expected bytes, or a number with a K, M, G or T suffix)", s)
	if !ok || num == "" {
		return 0, bad
	}
	if strings.Contains(num, ".") {
		f, err := strconv.ParseFloat(num, 64)
		if err != nil || f < 0 || f*float64(mult) >= math.MaxInt64 {
			return 0, bad
		}
		return int64(f * float64(mult)), nil
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, bad
	}
	return n * mult, nil
}

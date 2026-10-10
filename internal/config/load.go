package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seanmmitchell/gos3/internal/discovery"
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

	// BackendFS and BackendMemory are the StorageConfig.Backend values.
	BackendFS     = "fs"
	BackendMemory = "memory"

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
	"x-amz-abort-date", "x-amz-abort-rule-id",
}

// Default returns the default configuration. Name defaults to the host name;
// Storage.Path is filled in by Normalize (fs backend only), so that a memory
// backend without an explicit path is fully ephemeral.
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
		},
		Storage: StorageConfig{
			Backend:   BackendFS,
			FsyncData: true,
		},
		UI: UIConfig{Enabled: true, RedirectRoot: true},
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
	}
}

// Load returns the configuration from defaults < YAML file < environment.
// path is the YAML file ("" = $GOS3_CONFIG, and no file when that is unset
// too). Unknown YAML fields are rejected. The result is normalized (see
// Normalize) but not validated: callers apply flag overrides and then call
// Validate.
func Load(path string) (Config, error) {
	return load(path, os.LookupEnv)
}

func load(path string, lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	if path == "" {
		path, _ = lookup(EnvConfig)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		if err := decodeYAML(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, lookup); err != nil {
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
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("only one YAML document is allowed")
	}
	return nil
}

// Normalize fills derived defaults: the data path of the fs backend
// (DefaultDataPath), the host name as server name and the default region when
// empty, and trims list entries.
func (c *Config) Normalize() {
	c.Storage.Backend = strings.ToLower(strings.TrimSpace(c.Storage.Backend))
	if c.Storage.Backend == "" {
		c.Storage.Backend = BackendFS
	}
	if c.Storage.Backend == BackendFS && c.Storage.Path == "" {
		c.Storage.Path = DefaultDataPath
	}
	if c.Server.Region == "" {
		c.Server.Region = DefaultRegion
	}
	if c.Server.Address == "" {
		c.Server.Address = DefaultAddress
	}
	c.Server.Domains = trimList(c.Server.Domains)
	c.CORS.AllowedOrigins = trimList(c.CORS.AllowedOrigins)
	c.CORS.AllowedHeaders = trimList(c.CORS.AllowedHeaders)
	c.CORS.ExposeHeaders = trimList(c.CORS.ExposeHeaders)
	c.Discovery.Seeds = trimList(c.Discovery.Seeds)
	c.Discovery.DNSSRV = trimList(c.Discovery.DNSSRV)
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
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
// derived from it (ValidateSettings).
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Address); err != nil {
		return fmt.Errorf("server.address: %q must be [host]:port (e.g. \":9000\")", c.Server.Address)
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return errors.New("server.tls_cert and server.tls_key must be set together")
	}
	if c.Server.MaxObjectSize < 0 || c.Server.MaxObjectSize > MaxMaxObjectSize {
		return fmt.Errorf("server.max_object_size: must be between 0 (default 5 GiB) and %d bytes", MaxMaxObjectSize)
	}
	if c.Server.ShutdownTimeout < 0 {
		return errors.New("server.shutdown_timeout must not be negative")
	}
	switch c.Storage.Backend {
	case BackendFS:
		if c.Storage.Path == "" {
			return errors.New("storage.path is required for the fs backend")
		}
	case BackendMemory:
	default:
		return fmt.Errorf("storage.backend: %q is invalid (expected fs or memory)", c.Storage.Backend)
	}
	if err := validateRootCredentials(c.Auth.RootAccessKey, c.Auth.RootSecretKey); err != nil {
		return err
	}
	if c.Lifecycle.Interval != 0 && c.Lifecycle.Interval%time.Second != 0 {
		return errors.New("lifecycle.interval must be a whole number of seconds")
	}
	if err := ValidateSettings(SettingsFrom(c)); err != nil {
		return fmt.Errorf("invalid settings: %w", err)
	}
	return nil
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
	return s
}

// SystemDir returns the system state directory (<data>/.gos3), or "" when the
// configuration is fully ephemeral (memory backend without a data path).
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

// TLSEnabled reports whether the listener serves HTTPS.
func (c Config) TLSEnabled() bool { return c.Server.TLSCert != "" }

// Redacted returns a copy with secrets replaced by "<redacted>", for display.
func (c Config) Redacted() Config {
	if c.Auth.RootSecretKey != "" {
		c.Auth.RootSecretKey = "<redacted>"
	}
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

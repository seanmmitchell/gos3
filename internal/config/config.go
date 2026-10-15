// Package config holds gos3 configuration: static configuration loaded from
// defaults < JSON/YAML file < environment (GOS3_*) < CLI flags, plus a runtime
// Settings overlay that the admin API can change and that is persisted to
// <data>/.gos3/settings.json (it wins over static config when admin is enabled).
//
// Entry points: Load (CLI flags passed as Overrides) then Config.Validate;
// ResolveRootCredentials fills in generated root credentials; SettingsFrom
// derives the initial Settings and LoadStore overlays the persisted
// settings.json. ValidateSettings is the single settings validator, shared
// by Store.Update, Config.Validate and the admin API. EnvVars documents the
// GOS3_* environment variables.
//
// CONTRACT: exported types and signatures here are fixed. Loading/persisting
// is implemented by the integration work item (it may add files).
package config

import (
	"slices"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
)

// Config is the full static configuration.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Storage     StorageConfig     `yaml:"storage"`
	Auth        AuthConfig        `yaml:"auth"`
	UI          UIConfig          `yaml:"ui"`
	Admin       AdminConfig       `yaml:"admin"`
	CORS        CORSConfig        `yaml:"cors"`
	Discovery   discovery.Config  `yaml:"discovery"`
	Logging     LoggingConfig     `yaml:"logging"`
	Lifecycle   LifecycleConfig   `yaml:"lifecycle"`
	Metrics     MetricsConfig     `yaml:"metrics"`
	Compression CompressionConfig `yaml:"compression"`
}

// ServerConfig configures the HTTP listener and identity.
type ServerConfig struct {
	Address string `yaml:"address"` // default ":9000"
	Name    string `yaml:"name"`    // default hostname
	Region  string `yaml:"region"`  // default "us-east-1"
	// PublicURL is the externally reachable base URL advertised in /_gos3/info
	// and discovery (default derived from listener + primary IP).
	PublicURL string `yaml:"public_url"`
	// Domains enable virtual-hosted-style requests: Host "<bucket>.<domain>".
	Domains []string `yaml:"domains"`
	TLSCert string   `yaml:"tls_cert"`
	TLSKey  string   `yaml:"tls_key"`
	// MaxObjectSize for single PUT (default 5 GiB).
	MaxObjectSize int64 `yaml:"max_object_size"`
	// PublicInfo exposes GET /_gos3/info without auth (default true).
	PublicInfo bool `yaml:"public_info"`
	// ShutdownTimeout for graceful shutdown (default 30s).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// TrustProxyHeaders derives the client address (aws:SourceIp, access
	// log) from X-Forwarded-For and aws:SecureTransport from
	// X-Forwarded-Proto. Enable it only behind a reverse proxy (default
	// false). See package clientip for the exact rules.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// TrustedProxies lists the reverse proxies (CIDR prefixes or single IP
	// addresses) whose forwarding headers are honoured with
	// TrustProxyHeaders: requests from other addresses are taken at face
	// value, and X-Forwarded-For is walked from the right past these
	// addresses to find the client. Required (non-empty) when
	// TrustProxyHeaders is set.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// AllowedHosts lists extra host names (Host header, without port) that
	// clients use to reach this server, besides the ones it knows itself:
	// IP addresses, localhost, the OS host name (and <name>.local), the
	// hosts of public_url and address, and the virtual-host domains.
	// "*.example.com" allows every subdomain; "*" alone turns the Host
	// check off. Empty: requests for unknown hosts are served only when
	// they are signed. Non-empty: requests for unknown hosts are refused.
	// See docs/security.md (DNS rebinding).
	AllowedHosts []string `yaml:"allowed_hosts"`
	// MaxConnections caps the number of simultaneously open client
	// connections (0 = no limit, the default). At the cap a new connection
	// closes the longest-idle keep-alive connection, or is closed itself
	// when none is idle.
	MaxConnections int `yaml:"max_connections"`
}

// StorageConfig selects the backend.
type StorageConfig struct {
	Backend string `yaml:"backend"` // "fs" (default), "memory" or "s3"
	// Path is the data directory for the fs backend AND for system state
	// (<path>/.gos3/{iam.json,settings.json,server-id,placements.json,
	// backings.json}). With the memory and s3 backends it holds system
	// state only, and empty means that state is ephemeral.
	Path string `yaml:"path"` // default "./data" for fs, "" (ephemeral) for memory and s3
	// S3 configures the remote service of the s3 backend (required with
	// storage.backend s3, not allowed otherwise; see S3Config).
	S3 *S3Config `yaml:"s3,omitempty"`
	// FsyncData controls fsync of object data files on commit (default true),
	// for every fs backing.
	FsyncData bool `yaml:"fsync_data"`
	// Backings are additional named storage backings (e.g. one directory
	// per disk). The backing configured by Backend, Path and S3 above is
	// named DefaultBackingName. Every bucket lives on one backing and may
	// be replicated to, or moved to, others (see internal/storage/multi).
	Backings []BackingConfig `yaml:"backings"`
	// DefaultBacking names the backing new buckets are created on
	// ("" = DefaultBackingName).
	DefaultBacking string `yaml:"default_backing"`
	// Replication configures bucket replication between backings.
	Replication ReplicationConfig `yaml:"replication"`
}

// BackingConfig is one additional storage backing.
type BackingConfig struct {
	// Name identifies the backing in placements and the admin API:
	// 1-32 characters of [a-z0-9_-], starting with a letter or digit, not
	// DefaultBackingName.
	Name    string `yaml:"name"`
	Backend string `yaml:"backend"` // "fs" (default), "memory" or "s3"
	// Path is the data directory of an fs backing (required for fs; must
	// differ from every other backing's path and from storage.path).
	// Memory and s3 backings have no path.
	Path string `yaml:"path"`
	// S3 configures the remote service of an s3 backing (required for s3,
	// not allowed otherwise; see S3Config and docs/backings.md).
	S3 *S3Config `yaml:"s3,omitempty"`
}

// ReplicationConfig configures bucket replication between backings.
type ReplicationConfig struct {
	// DefaultMode is the replication mode of new buckets: "single"
	// (default; "active-passive" when DefaultReplicas is set),
	// "active-passive" or "active-active".
	DefaultMode string `yaml:"default_mode"`
	// DefaultReplicas are the backings new buckets are replicated to.
	DefaultReplicas []string `yaml:"default_replicas"`
	// AutoFailover promotes a synced replica of an active-passive bucket
	// when the primary's backing fails (changes not yet copied are lost).
	// Active-active buckets always fail over. Default false.
	AutoFailover bool `yaml:"auto_failover"`
	// Workers is the number of buckets copied concurrently (default 2).
	Workers int `yaml:"workers"`
	// HealthInterval is how often every backing is checked (default 10s).
	HealthInterval time.Duration `yaml:"health_interval"`
	// CutoverTimeout bounds how long a move's or promotion's cutover waits
	// for writes in flight on the bucket (default 60s).
	CutoverTimeout time.Duration `yaml:"cutover_timeout"`
}

// AuthConfig holds root credentials and signature options.
type AuthConfig struct {
	// Root credentials (env GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY).
	// When neither is set, random credentials are generated on first start
	// and stored in <data>/.gos3/root-credentials.json (see
	// ResolveRootCredentials).
	RootAccessKey string `yaml:"root_access_key"`
	RootSecretKey string `yaml:"root_secret_key"`
	StrictRegion  bool   `yaml:"strict_region"`
	// InsecureDemoCredentials allows a root secret key that is published
	// in documentation or demo files (see IsDemoSecret). Without it such a
	// configuration is rejected: anyone, including any website the
	// operator visits, can sign requests with a published key. For
	// throwaway local demos only.
	InsecureDemoCredentials bool `yaml:"insecure_demo_credentials"`
}

// AdminConfig controls the management API and persisted runtime settings.
type AdminConfig struct {
	Enabled bool `yaml:"enabled"` // default true; false also disables the UI
}

// UIConfig configures the embedded web UI served at /_gos3/ui/.
type UIConfig struct {
	Enabled bool `yaml:"enabled"` // default true; false in noui builds
	// RedirectRoot redirects anonymous browser GET / (Accept: text/html) to the UI.
	RedirectRoot bool `yaml:"redirect_root"` // default true
}

// CORSConfig is the server-wide CORS policy applied to the S3 API (when the
// bucket has no matching CORS rule) and to the admin API.
type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowedOrigins"` // "*" allowed
	AllowedHeaders []string `yaml:"allowed_headers" json:"allowedHeaders"` // default ["*"]
	ExposeHeaders  []string `yaml:"expose_headers" json:"exposeHeaders"`   // default "*" + ETag, x-amz-* (see DefaultExposeHeaders)
	MaxAgeSeconds  int      `yaml:"max_age_seconds" json:"maxAgeSeconds"`  // default 3600
}

// LoggingConfig configures slog.
type LoggingConfig struct {
	Level     string `yaml:"level" json:"level"`          // debug|info|warn|error (default info)
	Format    string `yaml:"format" json:"format"`        // text|json (default text)
	AccessLog bool   `yaml:"access_log" json:"accessLog"` // log every request (default true)
}

// LifecycleConfig configures the lifecycle (expiration) worker.
type LifecycleConfig struct {
	Enabled  bool          `yaml:"enabled"`  // default true
	Interval time.Duration `yaml:"interval"` // default 1h
}

// MetricsConfig configures GET /_gos3/metrics (Prometheus text format).
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"` // default true
	// Public exposes metrics without auth (default false: requires admin:GetStats).
	Public bool `yaml:"public"`
}

// Compression modes (CompressionConfig.Mode): the server-wide policy that
// decides whether new object data is stored compressed.
const (
	// CompressionOptional lets each bucket's compression setting decide
	// (off unless enabled for the bucket). The default.
	CompressionOptional = "optional"
	// CompressionEnforced compresses new objects in every bucket; buckets
	// cannot opt out.
	CompressionEnforced = "enforced"
	// CompressionDisabled compresses nothing new and buckets cannot enable
	// compression. Objects already stored compressed stay readable.
	CompressionDisabled = "disabled"
)

// CompressionConfig is the server-wide compression policy set by the
// server administrator (runtime-editable as Settings.Compression).
// Compression is transparent to clients: objects read back byte for byte,
// with the same ETags, checksums and sizes.
type CompressionConfig struct {
	// Mode is CompressionOptional (default), CompressionEnforced or
	// CompressionDisabled.
	Mode string `yaml:"mode"`
	// Level is the DEFLATE level, 1 (fastest, the default) to 9 (smallest).
	Level int `yaml:"level"`
	// MinSize is the object size in bytes below which objects whose size
	// is known up front are stored uncompressed (default 4096).
	MinSize int64 `yaml:"min_size"`
	// SkipContentTypes lists media types that are stored uncompressed
	// because they are compressed already: "type/subtype", "type/*" or a
	// prefix ending in "*" (case-insensitive; parameters are ignored).
	// Default DefaultSkipContentTypes. Objects with a Content-Encoding
	// such as gzip are never compressed again.
	SkipContentTypes []string `yaml:"skip_content_types"`
}

// Settings is the runtime-editable subset of configuration, exposed via the
// admin API (GET/PUT /_gos3/admin/v1/config) and persisted to settings.json.
type Settings struct {
	Name          string              `json:"name"`
	Region        string              `json:"region"`
	PublicURL     string              `json:"publicUrl"`
	Domains       []string            `json:"domains"`
	PublicInfo    bool                `json:"publicInfo"`
	UIRedirect    bool                `json:"uiRedirect"`
	CORS          CORSConfig          `json:"cors"`
	Discovery     discovery.Config    `json:"discovery"`
	Logging       LoggingConfig       `json:"logging"`
	Lifecycle     LifecycleSettings   `json:"lifecycle"`
	MetricsPublic bool                `json:"metricsPublic"`
	Compression   CompressionSettings `json:"compression"`
}

// CompressionSettings is the runtime-editable form of CompressionConfig.
type CompressionSettings struct {
	Mode             string   `json:"mode"`
	Level            int      `json:"level"`
	MinSize          int64    `json:"minSize"`
	SkipContentTypes []string `json:"skipContentTypes"`
}

// LifecycleSettings is the runtime-editable part of LifecycleConfig.
type LifecycleSettings struct {
	Enabled         bool  `json:"enabled"`
	IntervalSeconds int64 `json:"intervalSeconds"`
}

// Store holds the current Settings and notifies subscribers on change.
// Safe for concurrent use.
type Store struct {
	updateMu sync.Mutex // serializes Update (validate, persist, publish)
	mu       sync.RWMutex
	settings Settings
	path     string // settings.json path ("" = not persisted)
	subs     []func(old, new Settings)
}

// NewStore returns a Store with initial settings, persisting to path ("" =
// memory only).
func NewStore(initial Settings, path string) *Store {
	return &Store{settings: initial, path: path}
}

// Get returns a copy of the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSettings(s.settings)
}

// Update applies fn to a copy of the settings; if fn returns nil the result is
// validated (ValidateSettings), persisted (atomically, when the store has a
// path) and published to subscribers. On any error the current settings are
// left unchanged and returned together with the error. Validation errors are
// plain readable errors; persistence errors wrap the underlying
// *fs.PathError / syscall.Errno.
//
// Updates are serialized: subscribers of one update run before the next
// update is applied, so they always observe changes in order.
func (s *Store) Update(fn func(*Settings) error) (Settings, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	s.mu.RLock()
	old := cloneSettings(s.settings)
	s.mu.RUnlock()

	next := cloneSettings(old)
	if err := fn(&next); err != nil {
		return old, err
	}
	if err := ValidateSettings(next); err != nil {
		return old, err
	}
	if s.path != "" {
		if err := writeSettingsFile(s.path, next); err != nil {
			return old, err
		}
	}
	s.mu.Lock()
	s.settings = cloneSettings(next)
	subs := append([]func(old, new Settings){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(cloneSettings(old), cloneSettings(next))
	}
	return cloneSettings(next), nil
}

// Path returns the settings.json path ("" when not persisted).
func (s *Store) Path() string { return s.path }

// Subscribe registers fn to be called (synchronously) after every successful Update.
func (s *Store) Subscribe(fn func(old, new Settings)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

// cloneSettings deep-copies in. Nil and empty slices are preserved as such.
func cloneSettings(in Settings) Settings {
	out := in
	out.Domains = slices.Clone(in.Domains)
	out.CORS = cloneCORS(in.CORS)
	out.Discovery.Seeds = slices.Clone(in.Discovery.Seeds)
	out.Discovery.DNSSRV = slices.Clone(in.Discovery.DNSSRV)
	out.Compression.SkipContentTypes = slices.Clone(in.Compression.SkipContentTypes)
	return out
}

func cloneCORS(in CORSConfig) CORSConfig {
	out := in
	out.AllowedOrigins = slices.Clone(in.AllowedOrigins)
	out.AllowedHeaders = slices.Clone(in.AllowedHeaders)
	out.ExposeHeaders = slices.Clone(in.ExposeHeaders)
	return out
}

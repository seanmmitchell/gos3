// Package config holds gos3 configuration: static configuration loaded from
// defaults < YAML file < environment (GOS3_*) < CLI flags, plus a runtime
// Settings overlay that the admin API can change and that is persisted to
// <data>/.gos3/settings.json (it wins over static config for its fields).
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
	Server    ServerConfig     `yaml:"server"`
	Storage   StorageConfig    `yaml:"storage"`
	Auth      AuthConfig       `yaml:"auth"`
	UI        UIConfig         `yaml:"ui"`
	CORS      CORSConfig       `yaml:"cors"`
	Discovery discovery.Config `yaml:"discovery"`
	Logging   LoggingConfig    `yaml:"logging"`
	Lifecycle LifecycleConfig  `yaml:"lifecycle"`
	Metrics   MetricsConfig    `yaml:"metrics"`
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
	// addresses to find the client. Empty: any peer is trusted and the
	// rightmost X-Forwarded-For entry is the client (a single proxy).
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// StorageConfig selects the backend.
type StorageConfig struct {
	Backend string `yaml:"backend"` // "fs" (default) or "memory"
	// Path is the data directory for the fs backend AND for system state
	// (<path>/.gos3/{iam.json,settings.json,server-id}). Empty with the memory
	// backend means fully ephemeral.
	Path string `yaml:"path"` // default "./data" for fs, "" (ephemeral) for memory
	// FsyncData controls fsync of object data files on commit (default true).
	FsyncData bool `yaml:"fsync_data"`
	// Backings are remote S3 services that can hold the data of individual
	// buckets, chosen per bucket at creation (see docs/backings.md).
	Backings []BackingConfig `yaml:"backings"`
	// DefaultBacking names the backing of buckets created without an
	// explicit choice ("" or "local" = the server's own storage).
	DefaultBacking string `yaml:"default_backing"`
}

// BackingConfig describes one remote S3 service buckets can be stored on.
type BackingConfig struct {
	// Name identifies the backing in bucket records and in the
	// x-gos3-backing header: 1-32 lowercase letters, digits and '-'
	// ("local" is reserved for the server's own storage).
	Name string `yaml:"name" json:"name"`
	// Mode is BackingModeBlob (object data stored as opaque blobs; all
	// metadata stays local, every gos3 feature works) or BackingModeGateway
	// (objects stored natively, readable by other S3 clients; no
	// versioning or object lock).
	Mode string `yaml:"mode" json:"mode"`
	// Endpoint is the service URL, http(s)://host[:port] (no path).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Region is the signing region (default us-east-1).
	Region string `yaml:"region" json:"region"`
	// Bucket is the remote bucket. Required in blob mode. In gateway mode
	// it is optional: when set, every gos3 bucket is a key prefix
	// "<prefix><bucket>/" in it; when empty, every gos3 bucket maps to the
	// remote bucket of the same name (created when missing).
	Bucket string `yaml:"bucket" json:"bucket"`
	// Prefix is prepended to every remote key (e.g. "gos3/").
	Prefix string `yaml:"prefix" json:"prefix"`
	// AccessKey / SecretKey authenticate to the service. When empty they
	// are read from GOS3_BACKING_<NAME>_ACCESS_KEY / _SECRET_KEY (NAME
	// upper-cased, '-' as '_').
	AccessKey string `yaml:"access_key" json:"accessKey"`
	SecretKey string `yaml:"secret_key" json:"secretKey"`
	// Addressing is "path" (<endpoint>/<bucket>/<key>), "virtual"
	// (<bucket>.<host>/<key>) or "auto"/"" (virtual for amazonaws.com
	// endpoints, path otherwise).
	Addressing string `yaml:"addressing" json:"addressing"`
	// PartSize is the multipart part size for uploads to the service and
	// the largest object sent with one PUT (default 16 MiB, minimum 5 MiB).
	PartSize int64 `yaml:"part_size" json:"partSize"`
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
}

// UIConfig configures the embedded web UI served at /_gos3/ui/.
type UIConfig struct {
	Enabled bool `yaml:"enabled"` // default true
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

// Settings is the runtime-editable subset of configuration, exposed via the
// admin API (GET/PUT /_gos3/admin/v1/config) and persisted to settings.json.
type Settings struct {
	Name          string            `json:"name"`
	Region        string            `json:"region"`
	PublicURL     string            `json:"publicUrl"`
	Domains       []string          `json:"domains"`
	PublicInfo    bool              `json:"publicInfo"`
	UIRedirect    bool              `json:"uiRedirect"`
	CORS          CORSConfig        `json:"cors"`
	Discovery     discovery.Config  `json:"discovery"`
	Logging       LoggingConfig     `json:"logging"`
	Lifecycle     LifecycleSettings `json:"lifecycle"`
	MetricsPublic bool              `json:"metricsPublic"`
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
	return out
}

func cloneCORS(in CORSConfig) CORSConfig {
	out := in
	out.AllowedOrigins = slices.Clone(in.AllowedOrigins)
	out.AllowedHeaders = slices.Clone(in.AllowedHeaders)
	out.ExposeHeaders = slices.Clone(in.ExposeHeaders)
	return out
}

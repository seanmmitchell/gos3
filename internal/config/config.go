// Package config holds gos3 configuration: static configuration loaded from
// defaults < YAML file < environment (GOS3_*) < CLI flags, plus a runtime
// Settings overlay that the admin API can change and that is persisted to
// <data>/.gos3/settings.json (it wins over static config for its fields).
//
// CONTRACT: exported types and signatures here are fixed. Loading/persisting
// is implemented by the integration work item (it may add files).
package config

import (
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
}

// StorageConfig selects the backend.
type StorageConfig struct {
	Backend string `yaml:"backend"` // "fs" (default) or "memory"
	// Path is the data directory for the fs backend AND for system state
	// (<path>/.gos3/{iam.json,settings.json,server-id}). Empty with the memory
	// backend means fully ephemeral.
	Path string `yaml:"path"` // default "./data"
	// FsyncData controls fsync of object data files on commit (default true).
	FsyncData bool `yaml:"fsync_data"`
}

// AuthConfig holds root credentials and signature options.
type AuthConfig struct {
	RootAccessKey string `yaml:"root_access_key"` // env GOS3_ROOT_ACCESS_KEY; default "gos3admin"
	RootSecretKey string `yaml:"root_secret_key"` // env GOS3_ROOT_SECRET_KEY; default "gos3admin-secret" (warned)
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
	ExposeHeaders  []string `yaml:"expose_headers" json:"exposeHeaders"`   // default ETag, x-amz-* commonly used
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
// validated, persisted and published to subscribers. CONTRACT: persistence +
// validation are implemented by the integration work item.
func (s *Store) Update(fn func(*Settings) error) (Settings, error) {
	s.mu.Lock()
	old := cloneSettings(s.settings)
	next := cloneSettings(s.settings)
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return old, err
	}
	s.settings = next
	subs := append([]func(old, new Settings){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cloneSettings(next))
	}
	return cloneSettings(next), nil
}

// Subscribe registers fn to be called (synchronously) after every successful Update.
func (s *Store) Subscribe(fn func(old, new Settings)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func cloneSettings(in Settings) Settings {
	out := in
	out.Domains = append([]string(nil), in.Domains...)
	out.CORS.AllowedOrigins = append([]string(nil), in.CORS.AllowedOrigins...)
	out.CORS.AllowedHeaders = append([]string(nil), in.CORS.AllowedHeaders...)
	out.CORS.ExposeHeaders = append([]string(nil), in.CORS.ExposeHeaders...)
	out.Discovery.Seeds = append([]string(nil), in.Discovery.Seeds...)
	out.Discovery.DNSSRV = append([]string(nil), in.Discovery.DNSSRV...)
	return out
}

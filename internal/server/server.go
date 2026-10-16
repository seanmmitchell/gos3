// Package server wires gos3 together: it opens the storage backend, system
// state (<data>/.gos3: server-id, root credentials, iam.json,
// settings.json), IAM, the S3 and admin API handlers, discovery, the
// lifecycle worker, the background jobs (copy and mirror jobs, whole-bucket
// restic snapshots; jobs.go) and the embedded web UI, and serves them on
// one listener with the top-level routing described in
// docs/ARCHITECTURE.md:
//
//  1. virtual-hosted-style requests (Host "<bucket>.<domain>") → S3, always;
//  2. /_gos3/admin/v1 → admin API, /_gos3/ui/ → web UI, /_gos3/info,
//     /_gos3/health, /_gos3/ready, /_gos3/metrics; any other /_gos3/ path →
//     404 JSON;
//  3. an anonymous browser GET / (Accept: text/html) → 302 to the UI;
//  4. everything else → S3.
//
// Typical use (see cmd/gos3):
//
//	srv, err := server.New(cfg, logger, server.WithLogControl(ctl))
//	if err != nil { ... }
//	err = srv.Run(ctx) // returns after ctx is done and shutdown completed
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/admin"
	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/lifecycle"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/resticsnap"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/storage"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
	"github.com/seanmmitchell/gos3/internal/version"
	"github.com/seanmmitchell/gos3/internal/webui"
)

// ServerIDFileName is the file in the system directory holding the
// persistent server ID.
const ServerIDFileName = "server-id"

// Timeouts of the HTTP listener and the public probes. There is deliberately
// no global read/write timeout: object transfers may legitimately take
// hours. Instead request body reads (bodyguard.go, s3 countingBody) and
// response writes (writeGuard) must make progress.
const (
	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 1 << 20
	readyTimeout      = 5 * time.Second
	readyCacheTTL     = 2 * time.Second
	metricsTimeout    = 5 * time.Second
	urlCacheTTL       = 30 * time.Second
)

// Option customizes New.
type Option func(*options)

type options struct {
	uiFS       fs.FS
	logControl *LogControl
	version    string
}

// WithUIFS serves the web UI from fsys instead of the embedded build (tests,
// or a UI built separately).
func WithUIFS(fsys fs.FS) Option { return func(o *options) { o.uiFS = fsys } }

// WithLogControl lets the server apply the logging settings (level, format)
// to the logger at start and whenever they change through the admin API.
// ctl must control the logger passed to New (see NewLogger).
func WithLogControl(ctl *LogControl) Option { return func(o *options) { o.logControl = ctl } }

// WithVersion overrides the version reported in /_gos3/info and the Server
// header (default version.Short()).
func WithVersion(v string) Option { return func(o *options) { o.version = v } }

// Server is an assembled gos3 server. Create it with New, then call Run (or
// Serve with your own listener). It is an http.Handler, so tests may also
// drive Handler() directly.
type Server struct {
	cfg        config.Config
	log        *slog.Logger
	logControl *LogControl
	version    string

	backend     storage.Backend // the multi-backing router over backings
	backings    []multi.Backing // opened backings (owned by backend once it is set)
	backendName string
	dataPath    string // absolute data directory ("" when system state is ephemeral)
	systemDir   string
	iam         *iam.Manager
	verifier    *auth.Verifier
	metrics     *metrics.Registry
	settings    *config.Store
	clientIP    clientip.Resolver // client address rules (server.trust_proxy_headers, server.trusted_proxies)
	s3          http.Handler
	admin       http.Handler
	ui          http.Handler // nil when the UI is disabled
	uiBuilt     bool
	disc        *discovery.Service
	lifecycle   *lifecycle.Worker
	jobs        *jobs.Manager       // background jobs (jobs.go)
	copyJobs    *copyjob.Service    // copy and mirror job runners
	snapshots   *resticsnap.Service // whole-bucket restic snapshots
	tlsConfig   *tls.Config
	bodyIdle    time.Duration // idle read timeout of bodies sent to gos3 endpoints
	writeIdle   time.Duration // idle write timeout of responses (see writeGuard)
	hosts       *hostPolicy   // which Host headers name this server
	dirLock     *os.File      // exclusive lock on <systemDir>/lock (nil when ephemeral)

	tempMu   sync.Mutex
	tempTag  string   // server ID in temporary directory names ("" when unknown)
	tempDirs []string // temporary directories of open s3 backings

	// claims are the remote locations in use by the s3 backings, shared
	// by all of them (s3account.NewClaims).
	claims s3account.Claims

	serverID    string
	startedAt   time.Time
	credsSource string

	addr     atomic.Value // string: the bound listen address once serving
	serving  atomic.Bool
	draining atomic.Bool
	ready    chan struct{}
	// limiter is the connection limiter while serving with
	// server.max_connections (nil otherwise); tests inspect it.
	limiter atomic.Pointer[connLimiter]

	lifeCtx    context.Context // background work (discovery, lifecycle)
	lifeCancel context.CancelFunc

	urlMu      sync.Mutex
	urlKey     string
	urlExpires time.Time
	urls       []string

	readyMu sync.Mutex // serializes backendReady
	readyAt time.Time  // when backendReady last checked (zero: never)
	readyOK bool

	closeOnce sync.Once
	closeErr  error
}

// New assembles a server from a configuration loaded with config.Load. It
// validates cfg, opens the backend and the system state and fills in root
// credentials (config.ResolveRootCredentials), but does not listen yet.
// Close (or Run/Serve, which close on return) releases the resources.
// A nil logger discards logs.
func New(cfg config.Config, logger *slog.Logger, opts ...Option) (_ *Server, err error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if o.version == "" {
		o.version = version.Short()
	}
	cfg.Normalize()
	if err := cfg.LoadBackings(); err != nil {
		return nil, fmt.Errorf("loading backing configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	// Deprecated s3 keys (bucket, prefix, gateway) keep working, mapped
	// to their replacements; say so, so that the configuration is updated.
	for _, d := range cfg.Deprecations() {
		logger.Warn(d.String()+"; update the configuration (gos3 config print shows the new keys)",
			"backing", d.Backing, "key", d.Key, "use", d.Use)
	}

	s := &Server{
		cfg:        cfg,
		log:        logger,
		logControl: o.logControl,
		version:    o.version,
		systemDir:  cfg.SystemDir(),
		startedAt:  time.Now().UTC().Truncate(time.Second),
		ready:      make(chan struct{}),
		bodyIdle:   systemBodyIdleTimeout,
		writeIdle:  responseWriteIdle,
		hosts:      newHostPolicy(cfg),
		claims:     s3account.NewClaims(),
	}
	if s.clientIP, err = cfg.ClientIP(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if len(cfg.Server.TrustedProxies) > 0 && !cfg.Server.TrustProxyHeaders {
		logger.Warn("server.trusted_proxies has no effect: X-Forwarded-For / X-Forwarded-Proto are ignored unless server.trust_proxy_headers is true")
	}
	if !cfg.TLSEnabled() && !loopbackAddress(cfg.Server.Address) && !cfg.Server.TrustProxyHeaders {
		logger.Warn("serving plain HTTP on a non-loopback address: secret keys in admin API responses and signed " +
			"requests (which can be replayed for up to 15 minutes) are visible to the network; set server.tls_cert / " +
			"server.tls_key, or put gos3 behind a TLS-terminating reverse proxy")
	}
	s.lifeCtx, s.lifeCancel = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	if cfg.TLSEnabled() {
		cert, err := tls.LoadX509KeyPair(cfg.Server.TLSCert, cfg.Server.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
		s.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	// Storage first: the fs backend locks its directory, so a second
	// process on the same data path fails before touching any state.
	s.tempTag = peekServerID(s.systemDir)
	if err := s.openBackend(); err != nil {
		return nil, err
	}
	if s.systemDir != "" {
		if err := os.MkdirAll(s.systemDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating system directory: %w", err)
		}
		// It holds the root credentials and the IAM secret keys.
		if err := config.RestrictDir(s.systemDir); err != nil {
			logger.Warn("could not restrict access to the system directory to this user; "+
				"make sure other local users cannot read it", "dir", s.systemDir, "err", err)
		}
		tightenSystemPerms(s.systemDir, logger)
		// The fs backend's metadata database is locked already, but the
		// system state (iam.json, settings.json, ...) is used by every
		// backend: lock it too.
		if s.dirLock, err = lockSystemDir(s.systemDir); err != nil {
			return nil, err
		}
		s.removeStaleTempDirs()
	}
	if err := s.openRouter(); err != nil {
		return nil, err
	}
	if s.serverID, err = loadServerID(s.systemDir, logger); err != nil {
		return nil, err
	}
	s.credsSource = credentialsSource(&cfg, s.systemDir)
	if err := config.ResolveRootCredentials(&cfg, s.systemDir, logger); err != nil {
		return nil, err
	}
	s.cfg = cfg

	if s.iam, err = iam.Open(iam.Options{
		Dir:           s.systemDir,
		RootAccessKey: cfg.Auth.RootAccessKey,
		RootSecretKey: cfg.Auth.RootSecretKey,
	}); err != nil {
		return nil, fmt.Errorf("opening IAM: %w", err)
	}
	if s.settings, err = cfg.LoadSettings(); err != nil {
		return nil, err
	}
	st := s.settings.Get()
	// settings.json wins over the static configuration for runtime keys:
	// say so for every value that was set explicitly (config file, GOS3_*
	// variable or flag) and is therefore ignored.
	for _, o := range config.PersistedOverrides(cfg, st) {
		if o.Explicit {
			logger.Warn("configured value overridden by settings.json (saved from the web UI or admin API); change it there or remove the key from the file",
				"key", o.Key, "configured", o.Configured, "effective", o.Persisted, "file", cfg.SettingsPath())
		}
	}
	s.logControl.Set(st.Logging.Level, st.Logging.Format)

	// Background jobs need the router and the settings (region and
	// compression policy of the copies); they resume at once.
	if err := s.openJobs(); err != nil {
		return nil, err
	}

	s.verifier = auth.NewVerifier(s.iam, auth.Options{Region: st.Region, StrictRegion: cfg.Auth.StrictRegion})
	s.metrics = metrics.NewRegistry()
	s.s3 = s3.NewHandler(s3.Options{
		Backend:       s.backend,
		Verifier:      s.verifier,
		IAM:           s.iam,
		Metrics:       s.metrics,
		Settings:      s.settings,
		Logger:        logger,
		MaxObjectSize: cfg.Server.MaxObjectSize,
		Version:       s.version,
		ClientIP:      s.clientIP,
	})
	s.disc = discovery.New(st.Discovery, s.Info, logger)
	if cfg.Admin.Enabled {
		s.admin = s.instrumentAdmin(admin.NewHandler(admin.Deps{
			Backend:     s.backend,
			Verifier:    s.verifier,
			IAM:         s.iam,
			Metrics:     s.metrics,
			Settings:    s.settings,
			Discovery:   s.disc,
			Info:        s.Info,
			BackendName: s.backendName,
			DataPath:    s.dataPath,
			Logger:      logger,
			Jobs:        s.jobs,
			CopyJobs:    s.copyJobs,
			Snapshots:   s.snapshots,

			TrustProxyHeaders: s.clientIP,
		}))
	}
	s.lifecycle = lifecycle.NewWorker(s.backend, lifecycle.Options{
		Interval: time.Duration(st.Lifecycle.IntervalSeconds) * time.Second,
		Logger:   logger.With("component", "lifecycle"),
	})
	s.lifecycle.Reconfigure(time.Duration(st.Lifecycle.IntervalSeconds)*time.Second, st.Lifecycle.Enabled)

	if cfg.UI.Enabled {
		fsys := o.uiFS
		if fsys == nil {
			fsys = webui.FS()
		}
		if fsys == nil {
			return nil, fmt.Errorf("ui.enabled: this binary was built without UI assets; disable ui.enabled or use a build with the UI")
		}
		s.ui = webui.NewHandler(fsys)
		s.uiBuilt = webui.IsBuilt(fsys)
		if !s.uiBuilt {
			logger.Warn("the web UI is not built into this binary; /_gos3/ui/ shows a placeholder (build with `make build`)")
		}
	}

	s.settings.Subscribe(s.applySettings)
	return s, nil
}

// openBackend opens the configured storage backings: the default one
// (storage.backend, storage.path or storage.s3) and storage.backings.
// openRouter then puts the router in front of them.
func (s *Server) openBackend() error {
	st := s.cfg.Storage
	switch st.Backend {
	case config.BackendMemory, config.BackendS3:
		// storage.path holds system state only.
		if st.Path != "" {
			abs, err := filepath.Abs(st.Path)
			if err != nil {
				return fmt.Errorf("data path: %w", err)
			}
			s.dataPath = abs
		}
		s.backendName = st.Backend
		if st.Backend == config.BackendMemory {
			s.backings = append(s.backings, multi.Backing{Name: config.DefaultBackingName, Kind: config.BackendMemory, Backend: memoryBackend()})
			break
		}
		if st.S3 == nil {
			return errors.New("opening storage: storage.s3 is required for the s3 backend")
		}
		b, err := s.openS3(config.DefaultBackingName, *st.S3)
		if err != nil {
			return fmt.Errorf("opening storage: %w", err)
		}
		s.backings = append(s.backings, b)
		if s.systemDir == "" {
			s.log.Warn("the default backing is s3 without storage.path: system state (IAM users and policies, settings, " +
				"bucket placements, live backing configuration, generated root credentials) is ephemeral and lost on " +
				"restart; the buckets survive in the remote service. Set storage.path (GOS3_DATA) to keep it")
		}
	default:
		b, err := s.openFS(config.DefaultBackingName, st.Path)
		if err != nil {
			return fmt.Errorf("opening storage: %w", err)
		}
		s.backings = append(s.backings, b)
		s.backendName, s.dataPath = config.BackendFS, b.Path
	}
	for _, bc := range st.Backings {
		var (
			b   multi.Backing
			err error
		)
		switch bc.Backend {
		case config.BackendMemory:
			b = multi.Backing{Name: bc.Name, Kind: config.BackendMemory, Backend: memoryBackend()}
		case config.BackendS3:
			if bc.S3 == nil {
				return fmt.Errorf("opening storage backing %q: s3 settings are required", bc.Name)
			}
			b, err = s.openS3(bc.Name, *bc.S3)
		default:
			b, err = s.openFS(bc.Name, bc.Path)
		}
		if err != nil {
			return fmt.Errorf("opening storage backing %q: %w", bc.Name, err)
		}
		s.backings = append(s.backings, b)
	}
	s.waitS3Startup(s.backings)
	return nil
}

// memoryBackend returns a new memory backend.
func memoryBackend() storage.Backend { return memory.New() }

// openFS opens the fs backing name on directory path.
func (s *Server) openFS(name, path string) (multi.Backing, error) {
	log := s.log.With("component", "storage")
	if name != config.DefaultBackingName {
		log = log.With("backing", name)
	}
	b, err := fsbackend.Open(path, fsbackend.Options{FsyncData: s.cfg.Storage.FsyncData, Logger: log})
	if err != nil {
		return multi.Backing{}, err
	}
	return multi.Backing{Name: name, Kind: config.BackendFS, Path: b.Root(), Backend: b}, nil
}

// PlacementsFileName is the file in the system directory holding the
// bucket placements (which backings hold which buckets).
const PlacementsFileName = "placements.json"

// openRouter puts the multi-backing router in front of the backings.
func (s *Server) openRouter() error {
	st := s.cfg.Storage
	state := ""
	if s.systemDir != "" {
		state = filepath.Join(s.systemDir, PlacementsFileName)
	} else if len(st.Backings) > 0 {
		s.log.Warn("bucket placements are not persisted without a data directory (memory or s3 default backend " +
			"without storage.path): after a restart buckets are found where they are, but replication settings and " +
			"moves in progress are lost, and backings added live are forgotten")
	}
	mode := multi.Mode(st.Replication.DefaultMode)
	r, err := multi.Open(s.lifeCtx, multi.Options{
		Backings:          s.backings,
		Default:           st.DefaultBacking,
		NewBucketMode:     mode,
		NewBucketReplicas: st.Replication.DefaultReplicas,
		StatePath:         state,
		Workers:           st.Replication.Workers,
		HealthInterval:    st.Replication.HealthInterval,
		AutoFailover:      st.Replication.AutoFailover,
		CutoverTimeout:    st.Replication.CutoverTimeout,
		Logger:            s.log.With("component", "placement"),
	})
	s.backings = nil // closed by the router, also when Open fails
	if err != nil {
		return fmt.Errorf("opening storage: %w", err)
	}
	s.backend = r
	if s.cfg.Admin.Enabled {
		r.EnableBackingManagement(s.cfg.Storage, s.cfg.BackingsPath(), multi.BackingOpener{
			Open:        s.openLiveBacking,
			Reconfigure: s.reconfigureBacking,
		})
	}
	return nil
}

// credentialsSource predicts where config.ResolveRootCredentials takes the
// root credentials from (one of the config.Credentials* constants).
func credentialsSource(cfg *config.Config, systemDir string) string {
	switch {
	case cfg.Auth.RootAccessKey != "" || cfg.Auth.RootSecretKey != "":
		return config.CredentialsConfigured
	case systemDir == "":
		return config.CredentialsEphemeral
	}
	if _, err := os.Stat(filepath.Join(systemDir, config.RootCredentialsFileName)); err == nil {
		return config.CredentialsStored
	}
	return config.CredentialsGenerated
}

// loadServerID returns the persistent server ID from <dir>/server-id,
// creating it on first start. With dir "" (ephemeral) a new ID is returned.
// An unreadable ID file is replaced (with a warning) rather than fatal: the
// ID only identifies the server to discovery peers.
func loadServerID(dir string, log *slog.Logger) (string, error) {
	if dir == "" {
		return discovery.NewServerID(), nil
	}
	file := filepath.Join(dir, ServerIDFileName)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		if id := strings.TrimSpace(string(data)); validServerID(id) {
			return id, nil
		}
		log.Warn("server-id file is invalid, generating a new server ID", "file", file)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading server ID: %w", err)
	}
	id := discovery.NewServerID()
	if err := config.WriteFileAtomic(file, []byte(id+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("storing server ID: %w", err)
	}
	return id, nil
}

// tightenSystemPerms removes the group and other permission bits of the
// system directory and of the secret files in it (root-credentials.json,
// iam.json, settings.json) when they are set. MkdirAll and
// config.WriteFileAtomic apply 0700/0600 only when they create a path; a
// directory pre-created by an operator or a container volume, or restored
// from a backup that did not keep modes, would otherwise stay readable by
// other local users. Failures are logged, not fatal. Windows has no mode
// bits (config.RestrictDir sets an ACL instead).
func tightenSystemPerms(dir string, log *slog.Logger) {
	if runtime.GOOS == "windows" {
		return
	}
	for _, p := range []string{
		dir,
		filepath.Join(dir, config.RootCredentialsFileName),
		filepath.Join(dir, iam.FileName),
		filepath.Join(dir, config.SettingsFileName),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		perm := fi.Mode().Perm()
		if perm&0o077 == 0 {
			continue
		}
		if err := os.Chmod(p, perm&^0o077); err != nil {
			log.Warn("cannot restrict permissions; other local users may read it", "path", p, "mode", perm.String(), "err", err)
			continue
		}
		log.Warn("restricted permissions that allowed access by other local users", "path", p, "was", perm.String(), "now", (perm &^ 0o077).String())
	}
}

// loopbackAddress reports whether a listen address ([host]:port) binds to
// loopback only ("localhost", 127.0.0.0/8, ::1).
func loopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// validServerID accepts the IDs discovery accepts: 1-64 characters of
// [A-Za-z0-9._-].
func validServerID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Close stops background work and releases the backend and IAM. It is
// idempotent and is called by Run/Serve on return; call it yourself only
// when the server was never served. Background jobs are stopped (and
// saved, to resume at the next start) before the storage closes.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		if s.lifeCancel != nil {
			s.lifeCancel()
		}
		if s.disc != nil {
			s.disc.Stop()
		}
		var errs []error
		if err := s.closeJobs(); err != nil {
			errs = append(errs, err)
		}
		if s.lifecycle != nil {
			s.lifecycle.Stop()
		}
		if s.backend != nil {
			if err := s.backend.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing storage: %w", err))
			}
		}
		for _, b := range s.backings { // opened, but the router never took them
			if err := b.Backend.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing storage backing %q: %w", b.Name, err))
			}
		}
		s.removeTempDirs()
		if s.iam != nil {
			if err := s.iam.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing IAM: %w", err))
			}
		}
		if s.dirLock != nil {
			_ = s.dirLock.Close() // releases the lock
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// Handler returns the top-level HTTP handler (the server itself).
func (s *Server) Handler() http.Handler { return s }

// Addr returns the address the server listens on once Run/Serve bound it
// (e.g. "127.0.0.1:41234"), else "".
func (s *Server) Addr() string {
	a, _ := s.addr.Load().(string)
	return a
}

// Ready is closed once the server is listening (Addr and URL are then set).
func (s *Server) Ready() <-chan struct{} { return s.ready }

// URL returns the base URL for reaching the server from this host
// ("http://127.0.0.1:9000"; https with TLS). A wildcard listen host is
// replaced by the loopback address. Before Run/Serve bound the listener it
// is derived from the configured address.
func (s *Server) URL() string {
	addr := s.Addr()
	if addr == "" {
		addr = s.cfg.Server.Address
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return s.scheme() + "://" + net.JoinHostPort(host, port)
}

func (s *Server) scheme() string {
	if s.tlsConfig != nil {
		return "https"
	}
	return "http"
}

// RootCredentials returns the root access and secret key (configured,
// stored or generated).
func (s *Server) RootCredentials() (accessKey, secretKey string) {
	return s.cfg.Auth.RootAccessKey, s.cfg.Auth.RootSecretKey
}

// CredentialsSource tells where the root credentials came from: one of
// config.CredentialsConfigured, CredentialsStored, CredentialsGenerated or
// CredentialsEphemeral.
func (s *Server) CredentialsSource() string { return s.credsSource }

// RootCredentialsFile returns the file holding generated root credentials
// ("" when they are configured or ephemeral).
func (s *Server) RootCredentialsFile() string {
	if s.systemDir == "" || s.credsSource == config.CredentialsConfigured {
		return ""
	}
	return filepath.Join(s.systemDir, config.RootCredentialsFileName)
}

// Config returns the effective static configuration, including the
// resolved root credentials (use Redacted before printing it).
func (s *Server) Config() config.Config { return s.cfg }

// Settings returns the runtime settings store (changes are applied live).
func (s *Server) Settings() *config.Store { return s.settings }

// Discovery returns the discovery service.
func (s *Server) Discovery() *discovery.Service { return s.disc }

// Metrics returns the request metrics registry.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// IAM returns the IAM manager.
func (s *Server) IAM() *iam.Manager { return s.iam }

// Backend returns the storage backend.
func (s *Server) Backend() storage.Backend { return s.backend }

// BackendName returns the kind of the default backing: "fs", "memory" or
// "s3".
func (s *Server) BackendName() string { return s.backendName }

// DataPath returns the absolute data directory: the fs default backing's
// directory, or storage.path holding only system state for the memory and
// s3 backends ("" when that state is ephemeral).
func (s *Server) DataPath() string { return s.dataPath }

// ServerID returns the server's discovery ID.
func (s *Server) ServerID() string { return s.serverID }

// UIEnabled reports whether /_gos3/ui/ is served; UIBuilt whether it is a
// real UI build (not the placeholder).
func (s *Server) UIEnabled() bool { return s.ui != nil }

// UIBuilt reports whether the served UI is a real build.
func (s *Server) UIBuilt() bool { return s.ui != nil && s.uiBuilt }

// RunLifecycleOnce runs one lifecycle pass synchronously (tests and
// operators), independent of the lifecycle schedule and enabled flag.
func (s *Server) RunLifecycleOnce(ctx context.Context) (lifecycle.Stats, error) {
	return s.lifecycle.RunOnce(ctx)
}

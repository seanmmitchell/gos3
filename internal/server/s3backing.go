package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/localstate"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// s3 backings (docs/backings.md).
//
// An s3 backing is a connection to a remote S3 service (endpoint and
// credentials), an s3account.Backend: it holds native buckets (one remote
// bucket each, adopted or created by gos3) and wrapped buckets (gos3's
// format inside a container: the backing's home, a dedicated remote bucket
// or an existing remote bucket under a prefix). Its records live in its
// home (home_bucket/home_prefix) or, without a home, in a local state
// database in the system directory (S3RecordsDir).
//
// Every s3 backing of the server, those added live included, shares the
// server's claims registry (Server.claims), so that no remote location is
// used twice: a native bucket's remote bucket is used by nothing else, and
// homes and containers never nest.
//
// s3account.Open does no remote I/O that can fail it: a backing with a home
// loads in the background and answers storage.ErrBackingUnavailable until
// it is loaded; one keeping its records locally is ready at once. At
// startup the server waits up to s3StartupWait for the s3 backings to
// load, so a reachable remote is healthy from the first request; a backing
// that fails to load does not stop the server (the router starts it
// unhealthy and its health probe notices when it loads). A backing added
// live must load within s3LiveOpenTimeout, else the change is refused with
// the reason.

// Timeouts of loading s3 backings.
const (
	s3StartupWait     = 15 * time.Second
	s3LiveOpenTimeout = 30 * time.Second
)

// S3RecordsDir returns the directory holding the records of the s3 backing
// name when it has no home: <systemDir>/s3/<name>.
func S3RecordsDir(systemDir, name string) string {
	return filepath.Join(systemDir, "s3", name)
}

// newS3Client returns a client for the service of c.
func newS3Client(c config.S3Config) (*s3client.Client, error) {
	return s3client.New(s3client.Config{
		Endpoint:  c.Endpoint,
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		PathStyle: c.PathStyle(),
	})
}

// s3Connection returns the connection of the s3 settings c (validated):
// a client, the service description restic uses, the part size and the
// key-encryption key.
func s3Connection(c config.S3Config) (s3store.Connection, error) {
	kek, err := c.KEK()
	if err != nil {
		return s3store.Connection{}, err
	}
	client, err := newS3Client(c)
	if err != nil {
		return s3store.Connection{}, err
	}
	return s3store.Connection{
		Client: client, Endpoint: c.Endpoint, Region: c.Region, AccessKey: c.AccessKey,
		SecretKey: c.SecretKey, PathStyle: c.PathStyle(), PartSize: c.PartSize, KEK: kek,
	}, nil
}

// sameConnection reports whether a and b connect the same way (endpoint,
// region, addressing, part size, credentials and encryption key).
func sameConnection(a, b config.S3Config) bool {
	return a.Endpoint == b.Endpoint && a.Region == b.Region && a.PathStyle() == b.PathStyle() &&
		a.PartSize == b.PartSize && a.AccessKey == b.AccessKey && a.SecretKey == b.SecretKey &&
		a.EncryptionKey == b.EncryptionKey
}

// openS3 opens the s3 backing name described by c (validated). It does no
// remote I/O that can fail it (see waitS3Startup and openLiveBacking).
func (s *Server) openS3(name string, c config.S3Config) (_ multi.Backing, err error) {
	conn, err := s3Connection(c)
	if err != nil {
		return multi.Backing{}, err
	}
	opts := s3account.Options{
		Name:        name,
		Conn:        conn,
		DefaultKind: storage.BucketKind(c.EffectiveDefaultKind()),
		Claims:      s.claims,
		Logger:      s.log.With("component", "storage"), // s3account names the backing
		Tune: func(o *s3store.Options) {
			o.MaxRestoreSpoolBytes = s.cfg.Restic.RestoreSpoolBytes
			o.MaxRestores = s.cfg.Restic.MaxRestores
		},
	}
	var records string // where the records are, for the log
	switch {
	case c.HasHome():
		opts.Home = &s3account.Location{Bucket: c.HomeBucket, Prefix: c.HomePrefix}
		records = "home " + c.HomeBucket + "/" + c.HomePrefix
	case s.systemDir == "":
		// Refused by config.Validate already.
		return multi.Backing{}, fmt.Errorf("s3 backing %q has no home_bucket: its records need the system directory; set storage.path or home_bucket", name)
	default:
		opts.LocalDir = S3RecordsDir(s.systemDir, name)
		records = opts.LocalDir
	}
	tmp, err := s.newTempDir(name)
	if err != nil {
		return multi.Backing{}, err
	}
	// The backing uses its temporary directory until it is closed.
	release := s.holdTempDir(tmp)
	defer func() {
		if err != nil {
			release()
		}
	}()
	opts.TempDir = tmp
	acct, err := s3account.Open(opts)
	if err != nil {
		return multi.Backing{}, err
	}
	// The home's URL, or "local:" for records in the system directory.
	location := acct.Location()
	s.log.Info("s3 storage backing", "backing", name, "endpoint", c.Endpoint, "records", records,
		"default_kind", c.EffectiveDefaultKind())
	return multi.Backing{Name: name, Kind: config.BackendS3, Location: location,
		Backend: &s3Backing{Backend: acct, release: release, settings: c}}, nil
}

// s3Backing is the storage.Backend of an s3 backing handed to the router:
// the s3account backend, whose Close also releases the server's resources
// of the backing (its temporary directory). Embedding keeps every optional
// interface of the backend, s3account.RemoteManager included (the admin API
// asserts it on multi.Router.BackingBackend).
type s3Backing struct {
	*s3account.Backend
	release func()

	mu       sync.Mutex
	settings config.S3Config // the settings last applied (secrets included)
}

// Close closes the backend, then releases the backing's resources.
func (b *s3Backing) Close() error {
	err := b.Backend.Close()
	b.release()
	return err
}

// current returns the settings last applied.
func (b *s3Backing) current() config.S3Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settings
}

// applied records that c is applied.
func (b *s3Backing) applied(c config.S3Config) {
	b.mu.Lock()
	b.settings = c
	b.mu.Unlock()
}

// accountOf returns the s3 backing of be, or nil for other backends.
func accountOf(be storage.Backend) *s3Backing {
	b, _ := be.(*s3Backing)
	return b
}

// waitS3Startup waits (concurrently, at most s3StartupWait) for the s3
// backings among backings to load, logging those that do not.
func (s *Server) waitS3Startup(backings []multi.Backing) {
	ctx, cancel := context.WithTimeout(s.lifeCtx, s3StartupWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, b := range backings {
		acct := accountOf(b.Backend)
		if acct == nil {
			continue
		}
		wg.Go(func() {
			if err := acct.WaitReady(ctx); err != nil {
				s.log.Warn("s3 storage backing is not available yet; starting without it (its buckets answer 503 "+
					"until it loads, which is retried in the background)", "backing", b.Name, "location", b.Location, "err", err)
				return
			}
			// The records are loaded; let the other containers finish
			// loading too, so their wrapped buckets serve right away.
			if loading := acct.WaitContainers(ctx); len(loading) > 0 {
				s.log.Warn("s3 storage backing: containers still loading; their buckets answer 503 until they do",
					"backing", b.Name, "containers", loading)
			}
		})
	}
	wg.Wait()
}

// openLiveBacking is BackingOpener.Open: it opens a backing added (or
// replaced) through the admin API. An s3 backing must load within
// s3LiveOpenTimeout, so that wrong settings are reported to the caller; a
// backing keeping its records locally loads at once, so its credentials
// are checked against the service instead.
func (s *Server) openLiveBacking(def config.BackingDefinition) (multi.Backing, error) {
	bc := def.Config()
	switch bc.Backend {
	case config.BackendMemory:
		return multi.Backing{Name: bc.Name, Kind: config.BackendMemory, Backend: memoryBackend()}, nil
	case config.BackendS3:
		if bc.S3 == nil {
			return multi.Backing{}, fmt.Errorf("%w: backing %q: s3 settings are required", multi.ErrInvalidPlacement, bc.Name)
		}
		b, err := s.openS3(bc.Name, *bc.S3)
		if err != nil {
			if class := s3ErrorClass(err); class != nil {
				return multi.Backing{}, fmt.Errorf("%w: %w", class, err)
			}
			return multi.Backing{}, fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
		}
		acct := accountOf(b.Backend)
		ctx, cancel := context.WithTimeout(s.lifeCtx, s3LiveOpenTimeout)
		defer cancel()
		err = acct.WaitReady(ctx)
		if err == nil && !bc.S3.HasHome() {
			err = s.checkS3Credentials(ctx, acct, *bc.S3)
		}
		if err != nil {
			_ = b.Backend.Close()
			return multi.Backing{}, classifyS3Error(err)
		}
		return b, nil
	}
	return s.openFS(bc.Name, bc.Path)
}

// checkS3Credentials checks that the service accepts the settings c of the
// open s3 backing acct, which keeps its records locally (so loading it did
// not reach the service): the connection is prepared as for a change of
// settings, and the prepared change dropped.
func (s *Server) checkS3Credentials(ctx context.Context, acct *s3Backing, c config.S3Config) error {
	conn, err := s3Connection(c)
	if err != nil {
		return err
	}
	_, err = acct.PrepareConnection(ctx, conn)
	return err
}

// s3ErrorClass returns the multi error class of an error of an s3 backing
// that a configuration change must fix, or nil: multi.ErrConflict for a
// remote location in use (claimed by another backing, owned by another
// gos3 server, local records locked by another process), and
// multi.ErrInvalidPlacement for wrong settings (both answered with 4xx by
// the admin API).
func s3ErrorClass(err error) error {
	switch {
	case errors.Is(err, s3account.ErrClaimed), errors.Is(err, s3store.ErrInUse), errors.Is(err, s3store.ErrFenced),
		errors.Is(err, localstate.ErrLocked):
		return multi.ErrConflict
	case errors.Is(err, storage.ErrInvalidTarget), errors.Is(err, s3store.ErrWrongKEK), errors.Is(err, s3store.ErrMarkerMismatch),
		errors.Is(err, s3store.ErrPrefixNotEmpty), errors.Is(err, s3store.ErrUnmanagedPrefix),
		errors.Is(err, s3store.ErrNoConditionalWrites):
		// ErrMarkerMismatch covers ErrMarkerMissing.
		return multi.ErrInvalidPlacement
	}
	for _, code := range s3SettingsErrorCodes {
		if s3client.IsCode(err, code) {
			return multi.ErrInvalidPlacement
		}
	}
	return nil
}

// classifyS3Error marks the errors of an s3 backing that a configuration
// change must fix (see s3ErrorClass); others are returned unchanged (an
// unreachable service: storage.ErrBackingUnavailable, answered with 503).
func classifyS3Error(err error) error {
	if class := s3ErrorClass(err); class != nil {
		return fmt.Errorf("%w: %w", class, err)
	}
	return err
}

// s3SettingsErrorCodes are the S3 error codes of the remote service that
// mean wrong settings (credentials, bucket, region) rather than an outage.
var s3SettingsErrorCodes = []string{
	"AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "NoSuchBucket",
	"AuthorizationHeaderMalformed", "PermanentRedirect", "InvalidBucketName",
}

// reconfigureBacking is BackingOpener.Reconfigure: it prepares a change of
// the settings of the open s3 backing b that keep its identity (see
// config.BackingDefinition.Identity) to those of def: the connection
// (endpoint, region, credentials, addressing, part size, encryption key)
// and the default bucket kind. A new connection is verified first
// (s3account.Backend.PrepareConnection: the home's and every loaded
// container's marker and encryption key, or, without a home, the
// credentials and the native buckets' remote buckets); a change of the
// default kind alone needs no remote I/O.
func (s *Server) reconfigureBacking(ctx context.Context, b multi.Backing, def config.BackingDefinition) (func(), error) {
	bc := def.Config()
	if bc.S3 == nil {
		return nil, fmt.Errorf("%w: backing %q is not an s3 backing", multi.ErrInvalidPlacement, b.Name)
	}
	acct := accountOf(b.Backend)
	if acct == nil {
		return nil, fmt.Errorf("%w: backing %q cannot be reconfigured", multi.ErrConflict, b.Name)
	}
	c := *bc.S3
	kind := storage.BucketKind(c.EffectiveDefaultKind())
	if kind == storage.BucketKindWrapped && !c.HasHome() {
		// Refused by config validation already.
		return nil, fmt.Errorf("%w: backing %q: wrapped buckets as the default kind need a home", multi.ErrInvalidPlacement, b.Name)
	}
	finish := func() {
		if err := acct.SetDefaultKind(kind); err != nil {
			s.log.Error("changing the default bucket kind of an s3 backing failed", "backing", b.Name, "kind", kind, "err", err)
		}
		acct.applied(c)
	}
	if sameConnection(acct.current(), c) {
		return finish, nil
	}
	conn, err := s3Connection(c)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
	}
	commit, err := acct.PrepareConnection(ctx, conn)
	if err != nil {
		return nil, classifyS3Error(err)
	}
	return func() {
		commit()
		finish()
	}, nil
}

// Temporary directories of s3 backings are named
// gos3-<server ID>~<backing>-<random> under os.TempDir(), so that the
// directories a previous run of this server left behind (a crash) can be
// told apart from those of other servers and removed at startup
// (removeStaleTempDirs). Without a persistent server ID (no storage.path,
// or the first start) the "<server ID>~" part is omitted and nothing is
// cleaned up.

// tempDirPrefix is the name prefix of this server's temporary directories.
func (s *Server) tempDirPrefix() string {
	if s.tempTag == "" {
		return "gos3-"
	}
	return "gos3-" + s.tempTag + "~"
}

// newTempDir creates a temporary directory for backing name under
// os.TempDir() (spool and restic files; never the data directory). It is
// removed when the backing is closed (holdTempDir), at the latest by
// Close.
func (s *Server) newTempDir(name string) (string, error) {
	dir, err := os.MkdirTemp("", s.tempDirPrefix()+name+"-")
	if err != nil {
		return "", fmt.Errorf("creating a temporary directory for backing %q: %w", name, err)
	}
	s.tempMu.Lock()
	s.tempDirs = append(s.tempDirs, dir)
	s.tempMu.Unlock()
	return dir, nil
}

// holdTempDir records that an s3 backing uses the temporary directory dir
// and returns the (idempotent) function releasing it: it removes the
// directory.
func (s *Server) holdTempDir(dir string) (release func()) {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.tempMu.Lock()
			for i, d := range s.tempDirs {
				if d == dir {
					s.tempDirs = append(s.tempDirs[:i], s.tempDirs[i+1:]...)
					break
				}
			}
			s.tempMu.Unlock()
			if err := os.RemoveAll(dir); err != nil {
				s.log.Warn("could not remove a temporary directory", "dir", dir, "err", err)
			}
		})
	}
}

// removeStaleTempDirs removes the temporary directories a previous run of
// this server left behind (see tempDirPrefix). It runs once the system
// directory is locked, so no other process with this server ID uses them.
func (s *Server) removeStaleTempDirs() {
	if s.tempTag == "" {
		return
	}
	stale, err := filepath.Glob(filepath.Join(os.TempDir(), s.tempDirPrefix()+"*"))
	if err != nil {
		return
	}
	s.tempMu.Lock()
	current := map[string]bool{}
	for _, d := range s.tempDirs {
		current[filepath.Clean(d)] = true
	}
	s.tempMu.Unlock()
	for _, d := range stale {
		if current[filepath.Clean(d)] {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			s.log.Warn("could not remove a stale temporary directory", "dir", d, "err", err)
			continue
		}
		s.log.Debug("removed a stale temporary directory", "dir", d)
	}
}

// peekServerID returns the server ID stored in the system directory dir,
// or "" when there is none (yet) or it is invalid.
func peekServerID(dir string) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, ServerIDFileName))
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(data)); validServerID(id) {
		return id
	}
	return ""
}

// removeTempDirs removes every directory created by newTempDir (Close,
// after the backings are closed).
func (s *Server) removeTempDirs() {
	s.tempMu.Lock()
	dirs := s.tempDirs
	s.tempDirs = nil
	s.tempMu.Unlock()
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			s.log.Warn("could not remove a temporary directory", "dir", d, "err", err)
		}
	}
}

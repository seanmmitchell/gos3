package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/gateway"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// s3 backings (docs/backings.md).
//
// An s3 backing is an s3store on <bucket>/<prefix> of the remote service.
// A gateway backing (s3.gateway) is a routed backend: an s3store opened in
// gateway mode keeps the bucket records, configuration documents and the
// gateway's multipart state in <bucket>/<prefix>, and a gateway store
// keeps the objects natively in remote buckets named like the gos3
// buckets.
//
// s3store.Open does no remote I/O: the backing loads in the background
// and answers storage.ErrBackingUnavailable until it is loaded. At
// startup the server waits up to s3StartupWait for the s3 backings to
// load, so a reachable remote is healthy from the first request; a
// backing that fails to load does not stop the server (the router starts
// it unhealthy and its health probe notices when it loads). A backing
// added live must load within s3LiveOpenTimeout, else the change is
// refused with the reason.

// Timeouts of loading s3 backings.
const (
	s3StartupWait     = 15 * time.Second
	s3LiveOpenTimeout = 30 * time.Second
)

// backingModeGateway is the multi.Backing Mode of an s3 gateway backing.
const backingModeGateway = "gateway"

// gatewayAuxNamespace is the s3store aux namespace of the gateway store's
// multipart state.
const gatewayAuxNamespace = "gateway"

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

// openS3 opens the s3 backing name described by c (validated). It does
// no remote I/O that can fail it (see waitS3Loaded).
func (s *Server) openS3(name string, c config.S3Config) (_ multi.Backing, err error) {
	kek, err := c.KEK()
	if err != nil {
		return multi.Backing{}, err
	}
	client, err := newS3Client(c)
	if err != nil {
		return multi.Backing{}, err
	}
	tmp, err := s.newTempDir(name)
	if err != nil {
		return multi.Backing{}, err
	}
	// The backing reserves its remote bucket (see reservedRemoteBucket)
	// and its temporary directory until it is closed.
	release := s.holdS3Resources(tmp, c.Bucket)
	defer func() {
		if err != nil {
			release()
		}
	}()
	storeTmp := filepath.Join(tmp, "store")
	if err := os.MkdirAll(storeTmp, 0o700); err != nil {
		return multi.Backing{}, fmt.Errorf("creating temporary directory: %w", err)
	}
	log := s.log.With("component", "storage") // s3store and gateway name the backing
	meta, err := s3store.Open(s3store.Options{
		Name:      name,
		Client:    client,
		Endpoint:  c.Endpoint,
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		PathStyle: c.PathStyle(),
		Bucket:    c.Bucket,
		Prefix:    c.Prefix,
		PartSize:  c.PartSize,
		KEK:       kek,
		Gateway:   c.Gateway,
		TempDir:   storeTmp,
		Logger:    log,
	})
	if err != nil {
		return multi.Backing{}, err
	}
	b := multi.Backing{Name: name, Kind: config.BackendS3, Location: meta.Location(),
		Backend: &s3Backing{Backend: meta, release: release}}
	if !c.Gateway {
		return b, nil
	}
	gw := newGatewayStore(gateway.Options{
		Name:     name,
		Client:   client,
		Bucket:   "", // per-bucket mode: gos3 bucket b is remote bucket b
		Prefix:   "",
		PartSize: c.PartSize,
		State:    meta.AuxState(gatewayAuxNamespace),
		// The bucket of every open s3 backing, this one's included, at
		// the time of each bucket creation.
		Reserved: s.reservedRemoteBucket,
		SpoolDir: filepath.Join(tmp, "spool"),
		Logger:   log,
	}, meta)
	b.Mode = backingModeGateway
	b.Backend = &gatewayBacking{
		Backend: routed.New(routed.Options{Primary: meta, Store: gw, Logger: log}),
		meta:    meta, gw: gw, release: release,
	}
	return b, nil
}

// s3Backing is the storage.Backend of an s3 backing handed to the router:
// the s3store, whose Close also releases the server's resources of the
// backing (holdS3Resources). Embedding keeps every optional interface of
// the s3store.
type s3Backing struct {
	*s3store.Backend
	release func()
}

// Close closes the s3store, then releases the backing's resources.
func (b *s3Backing) Close() error {
	err := b.Backend.Close()
	b.release()
	return err
}

// gatewayBacking is the storage.Backend of a gateway backing: the routed
// backend over the metadata s3store and the gateway store, whose Close
// also releases the server's resources of the backing. Embedding keeps
// every optional interface of the routed backend.
type gatewayBacking struct {
	*routed.Backend
	meta    *s3store.Backend
	gw      *gatewayStore
	release func()
}

// Close closes the routed backend (gateway and metadata stores), then
// releases the backing's resources.
func (b *gatewayBacking) Close() error {
	err := b.Backend.Close()
	b.release()
	return err
}

// s3storeOf returns the s3store of an s3 backing (the metadata store of a
// gateway backing), or nil for other backends.
func s3storeOf(be storage.Backend) *s3store.Backend {
	switch v := be.(type) {
	case *s3Backing:
		return v.Backend
	case *gatewayBacking:
		return v.meta
	}
	return nil
}

// waitS3Loaded waits until st is loaded and serving. It gives up early,
// returning the reason, when the backing failed to load (the backing keeps
// retrying in the background), and when ctx ends.
func waitS3Loaded(ctx context.Context, st *s3store.Backend) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-st.Ready():
			return nil
		case <-ctx.Done():
			if err := st.Status().Err; err != nil {
				return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, err)
			}
			return fmt.Errorf("%w: not loaded in time (%s)", storage.ErrBackingUnavailable, st.Status().State)
		case <-tick.C:
			if status := st.Status(); status.State == s3store.StateFailed && status.Err != nil {
				return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, status.Err)
			}
		}
	}
}

// waitS3Startup waits (concurrently, at most s3StartupWait) for the s3
// backings among backings to load, logging those that do not.
func (s *Server) waitS3Startup(backings []multi.Backing) {
	ctx, cancel := context.WithTimeout(s.lifeCtx, s3StartupWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, b := range backings {
		st := s3storeOf(b.Backend)
		if st == nil {
			continue
		}
		wg.Go(func() {
			if err := waitS3Loaded(ctx, st); err != nil {
				s.log.Warn("s3 storage backing is not available yet; starting without it (its buckets answer 503 "+
					"until it loads, which is retried in the background)", "backing", b.Name, "location", b.Location, "err", err)
			}
		})
	}
	wg.Wait()
}

// openLiveBacking is BackingOpener.Open: it opens a backing added (or
// replaced) through the admin API. An s3 backing must load within
// s3LiveOpenTimeout, so that wrong settings are reported to the caller.
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
			return multi.Backing{}, fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
		}
		ctx, cancel := context.WithTimeout(s.lifeCtx, s3LiveOpenTimeout)
		defer cancel()
		if err := waitS3Loaded(ctx, s3storeOf(b.Backend)); err != nil {
			_ = b.Backend.Close()
			return multi.Backing{}, classifyS3Error(err)
		}
		return b, nil
	}
	return s.openFS(bc.Name, bc.Path)
}

// classifyS3Error marks the errors of an s3 backing that a configuration
// change must fix: a prefix owned by another server is a conflict, wrong
// settings are invalid (both answered with 4xx by the admin API).
func classifyS3Error(err error) error {
	switch {
	case errors.Is(err, s3store.ErrInUse):
		return fmt.Errorf("%w: %w", multi.ErrConflict, err)
	case errors.Is(err, s3store.ErrWrongKEK), errors.Is(err, s3store.ErrMarkerMismatch),
		errors.Is(err, s3store.ErrUnmanagedPrefix), errors.Is(err, s3store.ErrNoConditionalWrites):
		return fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
	}
	for _, code := range s3SettingsErrorCodes {
		if s3client.IsCode(err, code) {
			return fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
		}
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
// the connection settings (endpoint, region, credentials, addressing, part
// size, encryption key) of the open s3 backing b to those of def.
//
// A gateway backing is reconfigured in place only while it holds no
// buckets: the gateway store keeps its own client, and swapping it under
// running object operations (multipart uploads in particular) is not
// safe; with buckets, change the settings in the configuration (or
// backings.json) and restart. Whether it holds buckets is read from its
// metadata store; while that is not loaded (e.g. because wrong settings
// keep it from loading), from the router's placements: a backing no
// bucket is placed on can be reconfigured, else the change is refused.
func (s *Server) reconfigureBacking(ctx context.Context, b multi.Backing, def config.BackingDefinition) (func(), error) {
	bc := def.Config()
	if bc.S3 == nil {
		return nil, fmt.Errorf("%w: backing %q is not an s3 backing", multi.ErrInvalidPlacement, b.Name)
	}
	c := *bc.S3
	kek, err := c.KEK()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
	}
	client, err := newS3Client(c)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", multi.ErrInvalidPlacement, err)
	}
	conn := s3store.Connection{
		Client: client, Endpoint: c.Endpoint, Region: c.Region, AccessKey: c.AccessKey,
		SecretKey: c.SecretKey, PathStyle: c.PathStyle(), PartSize: c.PartSize, KEK: kek,
	}
	switch be := b.Backend.(type) {
	case *s3Backing:
		commit, err := be.PrepareConnection(ctx, conn)
		if err != nil {
			return nil, classifyS3Error(err)
		}
		return commit, nil
	case *gatewayBacking:
		if err := s.checkGatewayIdle(ctx, b.Name, be.meta); err != nil {
			return nil, err
		}
		commit, err := be.meta.PrepareConnection(ctx, conn)
		if err != nil {
			return nil, classifyS3Error(err)
		}
		return func() {
			commit()
			be.gw.reconnect(client, c.PartSize)
		}, nil
	}
	return nil, fmt.Errorf("%w: backing %q cannot be reconfigured", multi.ErrConflict, b.Name)
}

// errGatewayInUse refuses connection changes of a gateway backing with
// buckets (see reconfigureBacking).
func errGatewayInUse(name string) error {
	return fmt.Errorf("%w: the connection settings of gateway backing %q cannot change while it holds "+
		"buckets; change them in the configuration and restart the server", multi.ErrConflict, name)
}

// checkGatewayIdle returns nil when the gateway backing name (metadata
// store meta) holds no buckets (see reconfigureBacking).
func (s *Server) checkGatewayIdle(ctx context.Context, name string, meta *s3store.Backend) error {
	select {
	case <-meta.Ready():
		buckets, err := meta.ListBuckets(ctx)
		if err == nil {
			if len(buckets) > 0 {
				return errGatewayInUse(name)
			}
			return nil
		}
	default:
	}
	// Not loaded: no bucket may be placed on it.
	r, ok := s.backend.(*multi.Router)
	if !ok {
		return errGatewayInUse(name)
	}
	for _, st := range r.Backings() {
		if st.Name == name && st.Buckets == 0 && st.Replicas == 0 {
			return nil
		}
	}
	return errGatewayInUse(name)
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
// removed when the backing is closed (holdS3Resources), at the latest by
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

// holdS3Resources records that an s3 backing uses the temporary directory
// dir and the remote bucket bucket, and returns the (idempotent) function
// releasing both: it removes the directory and stops reserving the bucket.
func (s *Server) holdS3Resources(dir, bucket string) (release func()) {
	key := strings.ToLower(bucket)
	s.tempMu.Lock()
	if s.s3Buckets == nil {
		s.s3Buckets = map[string]int{}
	}
	s.s3Buckets[key]++
	s.tempMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.tempMu.Lock()
			if s.s3Buckets[key]--; s.s3Buckets[key] <= 0 {
				delete(s.s3Buckets, key)
			}
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

// reservedRemoteBucket reports whether bucket is the remote bucket of an
// open s3 backing: the default one, configured ones and ones added live,
// gateway metadata buckets included (case-insensitive). A gateway backing
// must not map a gos3 bucket onto it, or it would adopt that backing's
// data and metadata as objects. Endpoints are not compared, as one service
// is often reachable under several names.
func (s *Server) reservedRemoteBucket(bucket string) bool {
	s.tempMu.Lock()
	defer s.tempMu.Unlock()
	return s.s3Buckets[strings.ToLower(bucket)] > 0
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

// gatewayStore is the routed.DataStore of a gateway backing: a
// gateway.Store opened once the metadata store (whose aux state holds the
// gateway's multipart state, read by gateway.Open) has loaded. Until then
// it answers storage.ErrBackingUnavailable; the router rarely gets that
// far, as the metadata store refuses the bucket lookup first.
type gatewayStore struct {
	opts gateway.Options
	meta *s3store.Backend
	done chan struct{}

	mu     sync.Mutex
	st     *gateway.Store
	closed bool
}

var _ routed.DataStore = (*gatewayStore)(nil)

// newGatewayStore returns the store, which opens the gateway store as soon
// as meta has loaded.
func newGatewayStore(opts gateway.Options, meta *s3store.Backend) *gatewayStore {
	g := &gatewayStore{opts: opts, meta: meta, done: make(chan struct{})}
	go func() {
		select {
		case <-meta.Ready():
			if _, err := g.store(); err != nil && !errors.Is(err, errGatewayClosed) {
				g.logger().Warn("gateway backing: opening the gateway store failed; retrying on use", "err", err)
			}
		case <-g.done:
		}
	}()
	return g
}

var errGatewayClosed = errors.New("gateway backing closed")

func (g *gatewayStore) logger() *slog.Logger {
	if g.opts.Logger != nil {
		return g.opts.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// store returns the gateway store, opening it when the metadata store has
// loaded.
func (g *gatewayStore) store() (*gateway.Store, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, errGatewayClosed
	}
	if g.st != nil {
		return g.st, nil
	}
	select {
	case <-g.meta.Ready():
	default:
		return nil, fmt.Errorf("%w: gateway backing %q has not loaded yet", storage.ErrBackingUnavailable, g.opts.Name)
	}
	st, err := gateway.Open(g.opts)
	if err != nil {
		return nil, err
	}
	g.st = st
	return st, nil
}

// opened reports whether the gateway store was opened.
func (g *gatewayStore) opened() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st != nil
}

// reconnect makes the store use client and partSize: the gateway store is
// closed and reopened with them on next use (see reconfigureBacking for
// when this is safe).
func (g *gatewayStore) reconnect(client *s3client.Client, partSize int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.st != nil {
		_ = g.st.Close()
		g.st = nil
	}
	g.opts.Client, g.opts.PartSize = client, partSize
}

// Close closes the gateway store (the metadata store is the router's).
func (g *gatewayStore) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	close(g.done)
	if g.st != nil {
		return g.st.Close()
	}
	return nil
}

func (g *gatewayStore) AttachBucket(ctx context.Context, info storage.BucketInfo) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.AttachBucket(ctx, info)
}

func (g *gatewayStore) CheckEmpty(ctx context.Context, bucket string) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.CheckEmpty(ctx, bucket)
}

func (g *gatewayStore) DetachBucket(ctx context.Context, bucket string) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.DetachBucket(ctx, bucket)
}

func (g *gatewayStore) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	st, err := g.store()
	if err != nil {
		return nil, err
	}
	return st.NewObjectWriter(ctx, bucket, key)
}

func (g *gatewayStore) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	st, err := g.store()
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return st.GetObjectInfo(ctx, bucket, key, versionID)
}

func (g *gatewayStore) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	st, err := g.store()
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return st.OpenObject(ctx, bucket, key, versionID)
}

func (g *gatewayStore) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	st, err := g.store()
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return st.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
}

func (g *gatewayStore) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	st, err := g.store()
	if err != nil {
		return storage.DeleteResult{}, err
	}
	return st.DeleteObject(ctx, bucket, key, versionID)
}

func (g *gatewayStore) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	st, err := g.store()
	if err != nil {
		return storage.DeleteResult{}, err
	}
	return st.DeleteObjectIf(ctx, bucket, key, versionID, check)
}

func (g *gatewayStore) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (g *gatewayStore) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

func (g *gatewayStore) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	st, err := g.store()
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return st.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
}

func (g *gatewayStore) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	st, err := g.store()
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return st.GetMultipartUpload(ctx, bucket, key, uploadID)
}

func (g *gatewayStore) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	st, err := g.store()
	if err != nil {
		return nil, err
	}
	return st.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
}

func (g *gatewayStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	st, err := g.store()
	if err != nil {
		return nil, err
	}
	return st.ListParts(ctx, bucket, key, uploadID)
}

func (g *gatewayStore) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	st, err := g.store()
	if err != nil {
		return nil, err
	}
	return st.ListMultipartUploads(ctx, bucket)
}

func (g *gatewayStore) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
}

func (g *gatewayStore) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	st, err := g.store()
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return st.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
}

func (g *gatewayStore) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	st, err := g.store()
	if err != nil {
		return err
	}
	return st.AbortMultipartUpload(ctx, bucket, key, uploadID)
}

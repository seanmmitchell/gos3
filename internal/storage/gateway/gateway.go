// Package gateway implements the gateway (pass-through) backing: a
// routed.DataStore that stores gos3 objects as native objects in a remote
// S3 service, so other S3 clients can read and write them too (see
// docs/backings.md, "Gateway mode").
//
// gos3 bucket b maps either to the remote bucket b (Options.Bucket empty;
// created by AttachBucket when missing, adopted with its objects when it
// exists) or to the key prefix Prefix+b+"/" of one shared remote bucket.
// Object key k is remote key Prefix+k (per-bucket mode) or
// Prefix+b+"/"+k (shared mode). The router (internal/storage/routed)
// keeps bucket records and configuration documents in the primary
// backend; this package handles objects and multipart uploads only.
//
// Semantics are those of an unversioned storage.Backend bucket: every
// object is the null version. Differences that follow from storing data
// in someone else's service:
//
//   - Readers fail (with an error, never mixed content) when the object is
//     replaced or deleted on the remote while being read and the reader
//     has to issue another request (after a Seek, or a broken connection).
//     The first request of a reader is a plain GET whose response is read
//     to the end when nothing else happens, so a sequential read of the
//     whole object sees exactly one version.
//   - Listings (WalkObjects/WalkVersions) report what ListObjectsV2 knows:
//     key, size, remote ETag, last-modified time and storage class; no
//     user metadata, tags, checksums etc. (see listedInfo).
//   - Conditional writes and conditional deletes are atomic against other
//     users of the same Store (an in-process lock per key) and, as far as
//     the remote honours If-Match / If-None-Match, against everybody.
//   - Metadata changes (UpdateObjectMeta) rewrite the object onto itself
//     with a server-side copy pinned to its ETag.
//
// In-progress multipart uploads are tracked locally (persistently with
// Options.StateDir); see multipart.go for how parts are mapped to remote
// multipart uploads.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
)

// Options configures Open.
type Options struct {
	// Name is the backing name (used in errors and logs).
	Name string
	// Client talks to the remote service. Required.
	Client *s3client.Client
	// Bucket "" maps each gos3 bucket to the remote bucket of the same
	// name. When set, all gos3 buckets share this remote bucket and gos3
	// bucket b lives under the key prefix Prefix + b + "/".
	Bucket string
	// Prefix is prepended to every remote key ("" allowed).
	Prefix string
	// PartSize is the upload part size and the largest object sent with a
	// single PUT (0 = s3client.DefaultPartSize).
	PartSize int64
	// StateDir holds persistent local state: the multipart upload records
	// (gateway.db) and the staged small parts (parts/). "" keeps the
	// records in memory and stages parts in a temporary directory.
	StateDir string
	// SpoolDir holds temporary files buffering uploads ("" =
	// os.TempDir()). When set it is dedicated to this store: stale spool
	// files in it are removed by Open.
	SpoolDir string
	// Logger receives non-fatal errors. nil discards logs.
	Logger *slog.Logger
}

// Store is a gateway backing. Create it with Open.
type Store struct {
	name     string
	c        *s3client.Client
	bucket   string
	prefix   string
	partSize int64
	spoolDir string
	log      *slog.Logger

	// stageDir holds staged parts; stageTemp marks a temporary directory
	// removed by Close.
	stageDir  string
	stageTemp bool
	db        *bolt.DB // nil without StateDir

	keys keyLocks // per-object and per-part locks

	mu       sync.Mutex
	gens     map[string]uint64    // bucket -> detach generation
	uploads  map[string]*upload   // upload ID -> upload
	byBucket map[string][]*upload // bucket -> uploads in listing order
	closed   bool
	closeErr error
}

var _ routed.DataStore = (*Store)(nil)

// errClosed is returned by operations after Close.
var errClosed = errors.New("gateway: store closed")

// errNoRemoteBucket is wrapped (with storage.ErrBackingUnavailable) into
// errors caused by the remote bucket having vanished.
var errNoRemoteBucket = errors.New("the remote bucket does not exist")

// Open returns a store for opts, creating StateDir and SpoolDir when
// missing and loading the multipart upload records kept in StateDir.
func Open(opts Options) (*Store, error) {
	if opts.Client == nil {
		return nil, errors.New("gateway: Options.Client is required")
	}
	s := &Store{
		name:     opts.Name,
		c:        opts.Client,
		bucket:   opts.Bucket,
		prefix:   opts.Prefix,
		partSize: opts.PartSize,
		spoolDir: opts.SpoolDir,
		log:      opts.Logger,
		gens:     map[string]uint64{},
		uploads:  map[string]*upload{},
		byBucket: map[string][]*upload{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.spoolDir != "" {
		if err := os.MkdirAll(s.spoolDir, 0o700); err != nil {
			return nil, fmt.Errorf("gateway %q: create spool directory: %w", s.name, err)
		}
		// The spool directory is dedicated to this store: files left by a
		// previous process are garbage.
		if stale, err := filepath.Glob(filepath.Join(s.spoolDir, "gos3-spool-*")); err == nil {
			for _, f := range stale {
				_ = os.Remove(f)
			}
		}
	}
	if opts.StateDir == "" {
		dir, err := os.MkdirTemp(s.spoolDir, "gos3-gateway-parts-*")
		if err != nil {
			return nil, fmt.Errorf("gateway %q: create part staging directory: %w", s.name, err)
		}
		s.stageDir, s.stageTemp = dir, true
		return s, nil
	}
	s.stageDir = filepath.Join(opts.StateDir, "parts")
	if err := os.MkdirAll(s.stageDir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway %q: create state directory: %w", s.name, err)
	}
	if err := s.openState(filepath.Join(opts.StateDir, "gateway.db")); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the state database (and removes the temporary part
// staging directory of a store without StateDir). It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
	}
	if s.stageTemp {
		errs = append(errs, os.RemoveAll(s.stageDir))
	}
	s.closeErr = errors.Join(errs...)
	return s.closeErr
}

// --- naming ---

// maxKeyLength is the S3 limit on (remote) key lengths.
const maxKeyLength = 1024

// remote returns the remote bucket of gos3 bucket b and the prefix of its
// keys there.
func (s *Store) remote(bucket string) (rbucket, rprefix string) {
	if s.bucket == "" {
		return bucket, s.prefix
	}
	return s.bucket, s.prefix + bucket + "/"
}

// remoteKey returns the remote location of bucket/key.
func (s *Store) remoteKey(bucket, key string) (rbucket, rkey string) {
	rb, rp := s.remote(bucket)
	return rb, rp + key
}

// checkKey reports whether key can be stored: non-empty UTF-8 whose remote
// key fits the S3 limit of 1024 bytes (the prefix counts against it).
func (s *Store) checkKey(bucket, key string) error {
	_, rk := s.remoteKey(bucket, key)
	if !xmlSafe(key) {
		return fmt.Errorf("%w: object key %q (gateway %q cannot store characters that XML cannot represent: S3 listings could not return the key)",
			storage.ErrInvalidName, key, s.name)
	}
	if key == "" || !utf8.ValidString(key) || len(rk) > maxKeyLength {
		return fmt.Errorf("%w: object key %q (gateway %q stores keys of at most %d bytes)",
			storage.ErrInvalidName, key, s.name, maxKeyLength-(len(rk)-len(key)))
	}
	return nil
}

// xmlSafe reports whether every character of s can appear in an XML 1.0
// document (S3 services refuse keys that cannot be listed).
func xmlSafe(s string) bool {
	for _, r := range s {
		switch {
		case r == '\t', r == '\n', r == '\r':
		case r < 0x20, r == 0xFFFE, r == 0xFFFF:
			return false
		}
	}
	return true
}

// --- errors ---

// remoteErr maps a remote failure of op on bucket/key to the storage
// errors the S3 layer understands.
func (s *Store) remoteErr(op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}
	where := bucket
	if key != "" {
		where += "/" + key
	}
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return err
	case s3client.IsCode(err, "NoSuchBucket"):
		rb, _ := s.remote(bucket)
		return fmt.Errorf("gateway %q: %s %s: remote bucket %q: %w: %w", s.name, op, where, rb, errNoRemoteBucket, storage.ErrBackingUnavailable)
	case s3client.IsUnavailable(err):
		return fmt.Errorf("gateway %q: %s %s: %w: %w", s.name, op, where, storage.ErrBackingUnavailable, err)
	case s3client.IsPreconditionFailed(err):
		return fmt.Errorf("gateway %q: %s %s: %w: %w", s.name, op, where, storage.ErrPreconditionFailed, err)
	}
	return fmt.Errorf("gateway %q: %s %s: %w", s.name, op, where, err)
}

// statusOf returns the HTTP status of a remote error answer (0 if none).
func statusOf(err error) int {
	var e *s3client.Error
	if errors.As(err, &e) {
		return e.StatusCode
	}
	return 0
}

// notFound returns the error for a missing object addressed by versionID.
func notFound(bucket, key, versionID string) error {
	if versionID == "" {
		return fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, bucket, key)
	}
	return fmt.Errorf("%w: %s/%s version %q", storage.ErrVersionNotFound, bucket, key, versionID)
}

// checkVersion accepts the version IDs that address the (only) null
// version: "" and "null".
func checkVersion(bucket, key, versionID string) error {
	if versionID == "" || versionID == storage.NullVersionID {
		return nil
	}
	return notFound(bucket, key, versionID)
}

// --- buckets ---

// AttachBucket prepares the remote side of a new gos3 bucket. Per-bucket
// mode: an accessible remote bucket of the same name is adopted with its
// objects, a missing one is created (in the client's region). Shared
// mode: the shared remote bucket must exist.
func (s *Store) AttachBucket(ctx context.Context, info storage.BucketInfo) error {
	rb, _ := s.remote(info.Name)
	err := s.c.HeadBucket(ctx, rb)
	switch {
	case err == nil:
		return nil
	case s.bucket != "" && s3client.IsNotFound(err):
		return fmt.Errorf("gateway %q: the shared remote bucket %q does not exist: %w", s.name, rb, storage.ErrBackingUnavailable)
	case s.bucket == "" && s3client.IsNotFound(err):
		err = s.c.CreateBucket(ctx, rb)
		if s3client.IsCode(err, "BucketAlreadyExists") {
			return fmt.Errorf("gateway %q: the remote bucket name %q is taken by another account: %w", s.name, rb, storage.ErrBucketExists)
		}
		return s.remoteErr("CreateBucket", info.Name, "", err)
	case statusOf(err) == http.StatusForbidden:
		return fmt.Errorf("gateway %q: the remote bucket %q exists but is not accessible with the backing's credentials (HTTP 403): %w", s.name, rb, storage.ErrBackingUnavailable)
	}
	return s.remoteErr("HeadBucket", info.Name, "", err)
}

// CheckEmpty reports storage.ErrBucketNotEmpty when the bucket holds an
// object. A remote bucket that no longer exists counts as empty, so the
// gos3 bucket can still be deleted.
func (s *Store) CheckEmpty(ctx context.Context, bucket string) error {
	found := false
	err := s.WalkObjects(ctx, bucket, "", "", func(storage.ObjectInfo) error {
		found = true
		return storage.ErrStopWalk
	})
	switch {
	case errors.Is(err, errNoRemoteBucket):
		return nil
	case err != nil:
		return err
	case found:
		return fmt.Errorf("%w: %q", storage.ErrBucketNotEmpty, bucket)
	}
	return nil
}

// DetachBucket forgets a deleted bucket: writers created before fail at
// Commit and its in-progress uploads are aborted (remote uploads best
// effort). Remote objects and buckets are left alone.
func (s *Store) DetachBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	s.gens[bucket]++
	ups := append([]*upload(nil), s.byBucket[bucket]...)
	s.mu.Unlock()
	var errs []error
	for _, u := range ups {
		u.op.Lock()
		if !s.isGone(u) {
			if rid := s.remoteUploadID(u); rid != "" {
				rb, rk := s.remoteKey(u.bucket, u.key)
				if err := s.c.AbortMultipartUpload(ctx, rb, rk, rid); err != nil {
					errs = append(errs, s.remoteErr("AbortMultipartUpload", u.bucket, u.key, err))
				}
			}
			if err := s.dropUpload(u); err != nil {
				errs = append(errs, err)
			}
		}
		u.op.Unlock()
	}
	return errors.Join(errs...)
}

// gen returns the detach generation of bucket.
func (s *Store) gen(bucket string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gens[bucket]
}

// --- locks ---

// keyLocks is a set of mutexes by name, created on demand and dropped when
// unused.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// lock locks name and returns the unlock function.
func (k *keyLocks) lock(name string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	l := k.m[name]
	if l == nil {
		l = &keyLock{}
		k.m[name] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, name)
		}
		k.mu.Unlock()
	}
}

// lockObject serializes conditional operations on one object.
func (s *Store) lockObject(bucket, key string) func() {
	return s.keys.lock("o\x00" + bucket + "\x00" + key)
}

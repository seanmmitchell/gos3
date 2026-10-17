package native

// Store layout
//
// A Store serves the objects of every native bucket of one s3 backing. It
// keeps no bucket records: every operation asks Options.Resolve for the
// bucket's binding (remote bucket and region) and recorded settings
// (versioning, object lock), so a change made through the s3 account
// backing takes effect at once. Requests go through the client of the
// binding's region (Client.ForRegion); a remote answer saying the bucket
// lives in another region makes the store learn that region, retry once
// and report the drift (Options.OnDrift).
//
// Version IDs, delete markers, retention and legal holds are the remote
// bucket's (see the package documentation). Objects, metadata and overlay
// records are described in meta.go and overlay.go; multipart uploads in
// multipart.go; walks in walk.go.
//
// Atomicity: conditional writes, conditional deletes and metadata updates
// are serialized per object by an in-process lock, so they are atomic
// against other users of the same Store and, as far as the remote honours
// If-Match / If-None-Match, against everybody.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Store holds the objects of native buckets. Create it with Open.
type Store struct {
	name             string
	c                *s3client.Client
	resolve          func(ctx context.Context, bucket string) (Bucket, error)
	onDrift          func(bucket, reason string)
	partSize         int64
	spoolDir         string
	forbidCompliance bool
	state            StateStore
	log              *slog.Logger

	keys keyLocks // per-object and per-part locks

	mu       sync.Mutex
	gens     map[string]uint64                 // bucket -> detach generation
	uploads  map[string]*upload                // upload ID -> upload
	byBucket map[string][]*upload              // bucket -> uploads in listing order
	overlays map[bucketKey]map[string]*overlay // by key, then version ID
	// Hidden overlays whose durable deletion failed; retried by the sweep.
	pendingOverlays map[overlayID]bool
	// bound maps remote buckets (lower case) to the gos3 bucket bound to
	// them, as learned from Resolve and AttachBucket.
	bound map[string]string
	// regions holds the regions of remote buckets learned from
	// wrong-region answers (until the bucket is refreshed or detached).
	regions map[string]string
	// noTagging marks remote buckets that do not keep native tags.
	noTagging map[string]bool
	// listPage is the page size of listings (0 = the service's default;
	// tests lower it).
	listPage int
	// pendingStaged holds staged blobs whose deletion failed (a reader
	// still had one open, a transient error): retried by cleanupStaged.
	pendingStaged map[string]bool
	// swept is when each bucket's overlays were last swept, sweptAfter
	// the last key of an unfinished sweep (overlay.go).
	swept      map[string]time.Time
	sweptAfter map[string]string
	closed     bool

	stopSweep context.CancelFunc
	sweepWG   sync.WaitGroup
}

// Open returns a store for opts, creating SpoolDir when missing and
// loading the state kept in opts.State. Besides reading the state records
// it does no I/O that can fail it: the removal of stale state (staged
// parts no record refers to) is best effort.
func Open(opts Options) (*Store, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("native: Options.Client is required")
	case opts.State == nil:
		return nil, errors.New("native: Options.State is required")
	case opts.Resolve == nil:
		return nil, errors.New("native: Options.Resolve is required")
	}
	s := &Store{
		name:             opts.Name,
		c:                opts.Client,
		resolve:          opts.Resolve,
		onDrift:          opts.OnDrift,
		partSize:         opts.PartSize,
		spoolDir:         opts.SpoolDir,
		forbidCompliance: opts.ForbidCompliance,
		state:            opts.State,
		log:              opts.Logger,
		gens:             map[string]uint64{},
		uploads:          map[string]*upload{},
		byBucket:         map[string][]*upload{},
		overlays:         map[bucketKey]map[string]*overlay{},
		bound:            map[string]string{},
		regions:          map[string]string{},
		noTagging:        map[string]bool{},
		pendingStaged:    map[string]bool{},
		swept:            map[string]time.Time{},
		sweptAfter:       map[string]string{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.spoolDir != "" {
		if err := os.MkdirAll(s.spoolDir, 0o700); err != nil {
			return nil, fmt.Errorf("native %q: create spool directory: %w", s.name, err)
		}
		// The spool directory is dedicated to this store: files left by a
		// previous process are garbage.
		if stale, err := filepath.Glob(filepath.Join(s.spoolDir, "gos3-spool-*")); err == nil {
			for _, f := range stale {
				_ = os.Remove(f)
			}
		}
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopSweep = cancel
	s.sweepWG.Add(1)
	go s.sweeper(ctx)
	return s, nil
}

// Close marks the store closed: uploads can no longer be created. Staged
// blobs whose deletion failed are deleted once more (best effort; Open
// removes those still left). The StateStore is not closed (it belongs to
// the caller). It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stopSweep()
	s.sweepWG.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	s.cleanupStaged(ctx)
	return nil
}

// SetClient makes later operations use client c (another endpoint or
// credentials of the same remote service; buckets in other regions are
// reached through c.ForRegion) and partSize as the upload part size (0 =
// s3client.DefaultPartSize). Operations in flight, and readers already
// open, finish with the client they started with. The s3 account
// backing calls it when the backing's connection settings change.
func (s *Store) SetClient(c *s3client.Client, partSize int64) {
	if c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c, s.partSize = c, partSize
}

// client returns the current client of the backing's region.
func (s *Store) client() *s3client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// uploadPartSize returns the current upload part size (0 = default).
func (s *Store) uploadPartSize() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partSize
}

func (s *Store) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// --- buckets and clients ---

// target is a gos3 bucket resolved for one operation.
type target struct {
	bucket string // gos3 bucket name
	b      Bucket // binding and recorded settings
	region string // region requests are signed for
	c      *s3client.Client
	// attaching: the bucket is being bound, there are no recorded
	// settings to drift from yet.
	attaching bool
}

// versioned reports whether the bucket's versioning was ever enabled
// (Enabled or Suspended), as recorded.
func (t *target) versioned() bool { return t.b.Versioning != storage.VersioningUnversioned }

// nullWrites reports whether a write replaces the key's null version
// (versioning Unversioned or Suspended).
func (t *target) nullWrites() bool { return t.b.Versioning != storage.VersioningEnabled }

// target resolves bucket for one operation. Unknown buckets yield the
// error of Options.Resolve (wrapping storage.ErrBucketNotFound).
func (s *Store) target(ctx context.Context, bucket string) (*target, error) {
	b, err := s.resolve(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if b.Remote == "" {
		return nil, fmt.Errorf("native %q: bucket %q has no remote bucket: %w", s.name, bucket, storage.ErrBucketUnavailable)
	}
	s.mu.Lock()
	if _, ok := s.bound[strings.ToLower(b.Remote)]; !ok {
		s.bound[strings.ToLower(b.Remote)] = bucket
	}
	region := b.Region
	if r, ok := s.regions[b.Remote]; ok {
		region = r
	}
	s.mu.Unlock()
	return &target{bucket: bucket, b: b, region: region, c: s.client().ForRegion(region)}, nil
}

// call runs fn with t's client. When the remote answers that the bucket
// lives in another region, the store learns that region (reporting the
// drift) and runs fn once more with a client for it; a second
// wrong-region answer is returned (remoteErr maps it to
// storage.ErrBucketUnavailable).
func (s *Store) call(ctx context.Context, t *target, fn func(c *s3client.Client) error) error {
	err := fn(t.c)
	region, ok := s.wrongRegion(ctx, t, err)
	if !ok {
		return err
	}
	s.learnRegion(t, region)
	return fn(t.c)
}

// wrongRegion reports whether err says that t's remote bucket lives in
// another region than t's client signs for, and returns that region.
// Answers to HEAD requests have no body: a 400 there may be a signature
// for the wrong region, which AWS reports with x-amz-bucket-region only
// and other services not at all, so the bucket's region is looked up.
func (s *Store) wrongRegion(ctx context.Context, t *target, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	region, ok := s3client.IsWrongRegion(err)
	if !ok {
		var e *s3client.Error
		if !errors.As(err, &e) || e.Err != nil || e.StatusCode != http.StatusBadRequest || e.Code != "" && e.Region == "" {
			return "", false
		}
		region = e.Region
	}
	if region == "" {
		r, rerr := t.c.BucketRegion(ctx, t.b.Remote)
		if rerr != nil || r == "" {
			return "", false
		}
		region = r
	}
	return region, region != t.c.Region()
}

// learnRegion switches t (and later operations on its remote bucket) to
// region.
func (s *Store) learnRegion(t *target, region string) {
	s.mu.Lock()
	s.regions[t.b.Remote] = region
	s.mu.Unlock()
	if !t.attaching {
		s.log.Info("native: remote bucket is in another region", "backing", s.name, "bucket", t.bucket,
			"remote", t.b.Remote, "recorded", t.b.Region, "region", region)
		s.drift(t.bucket, fmt.Sprintf("the remote bucket %q is in region %q", t.b.Remote, region))
	}
	t.region, t.c = region, s.client().ForRegion(region)
}

// drift reports an answer of the remote that contradicts the recorded
// settings of bucket (Options.OnDrift).
func (s *Store) drift(bucket, reason string) {
	s.log.Warn("native: remote bucket differs from its recorded settings", "backing", s.name, "bucket", bucket, "reason", reason)
	if s.onDrift != nil {
		s.onDrift(bucket, reason)
	}
}

// tagging reports whether t's remote bucket keeps native tags (assumed
// until an answer shows otherwise).
func (s *Store) tagging(t *target) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.noTagging[t.b.Remote]
}

// noNativeTags records that t's remote bucket does not keep native tags.
func (s *Store) noNativeTags(t *target, why string) {
	s.mu.Lock()
	known := s.noTagging[t.b.Remote]
	s.noTagging[t.b.Remote] = true
	s.mu.Unlock()
	if !known {
		s.log.Warn("native: the remote bucket does not keep object tags; gos3 keeps them itself", "backing", s.name,
			"bucket", t.bucket, "remote", t.b.Remote, "reason", why)
	}
}

// gen returns the detach generation of bucket.
func (s *Store) gen(bucket string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gens[bucket]
}

// --- keys ---

// maxKeyLength is the S3 limit on key lengths.
const maxKeyLength = 1024

// checkKey reports whether key can be stored: non-empty UTF-8 of at most
// 1024 bytes that XML can represent (S3 listings could not return other
// keys).
func (s *Store) checkKey(key string) error {
	if !xmlSafe(key) {
		return fmt.Errorf("%w: object key %q (native buckets of %q cannot store characters that XML cannot represent: S3 listings could not return the key)",
			storage.ErrInvalidName, key, s.name)
	}
	if key == "" || !utf8.ValidString(key) || len(key) > maxKeyLength {
		return fmt.Errorf("%w: object key %q (native buckets of %q store keys of at most %d bytes)",
			storage.ErrInvalidName, key, s.name, maxKeyLength)
	}
	return nil
}

// xmlSafe reports whether every character of s can appear in an XML 1.0
// document.
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

// lockObject serializes the writes, conditional deletes and metadata
// updates of one object.
func (s *Store) lockObject(bucket, key string) func() {
	return s.keys.lock("o\x00" + bucket + "\x00" + key)
}

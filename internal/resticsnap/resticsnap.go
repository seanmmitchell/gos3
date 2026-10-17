// Package resticsnap takes scheduled whole-bucket restic snapshots: each
// snapshot holds a bucket's objects as files below /objects, the manifest
// /gos3-manifest.jsonl (one line per key with its version, metadata and
// file path; it is authoritative, paths are never decoded) and the bucket's
// description and configuration documents (/gos3-bucket.json), so that
// `restic restore` alone recovers a bucket's content. It also applies the
// snapshots' retention, prunes their repositories and restores snapshots
// into buckets.
//
// The work runs as background jobs (internal/jobs) of three kinds, whose
// runners Service.Runner returns: KindSnapshot, KindRestore and KindPrune.
// Service.Start runs the scheduler that submits snapshot jobs when they are
// due (see scheduler.go); the admin API calls the Service's other methods.
//
// # Snapshots
//
// A bucket's schedule is its configuration document
// storage.ConfigResticSnapshots (Config). Snapshots go to the restic
// repository of the bucket itself (target "bucket": fs buckets and wrapped
// buckets of s3 backings, deduplicated with the bucket's per-version
// archive) or to an aux repository on a backing (target "backing": any
// bucket), see restic.Host.
//
// A run (run.go) stages the bucket below a persistent stage directory and
// backs the stage up with `restic backup . --parent <newest complete
// snapshot> --ignore-inode --ignore-ctime`. Keys unchanged since the parent
// snapshot are staged as sparse placeholders of the parent's size and
// modification time (restic.MakePlaceholder), which restic does not read:
// it takes their content from the parent. Only new and changed objects are
// read from the bucket. When the staged data reaches the spool budget
// (Options.SpoolBytes), the run saves a partial snapshot (tag TagPartial),
// turns the staged files into placeholders and continues on top of it.
// Every backup is tagged TagPartial until it is verified: restic must not
// have read more files than the run wrote since the previous backup;
// otherwise the run compares the snapshot with its parent, forgets it,
// materializes the files restic read and tries again. The verified last
// backup is then promoted (backed up once more, reading nothing, without
// TagPartial), so that only verified snapshots are ever parents or
// restored, even when a failed snapshot cannot be forgotten. Snapshots
// lists a promoted snapshot with the summary of the backup its run
// verified.
//
// Snapshots are tagged TagSnapshot, restic.BucketTag(bucket) and, when the
// bucket's ID is known, restic.BucketIDTag(id), with host SnapshotHost.
// Retention and Forget only ever forget snapshots with those tags and host,
// never per-version archive snapshots (restic.TagObject).
package resticsnap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Job kinds run by the Service's runners (see Service.Runner).
const (
	// KindSnapshot takes a whole-bucket snapshot (spec {"bucket": "b"}).
	KindSnapshot = "restic-snapshot"
	// KindRestore restores a snapshot into a bucket (see Service.Restore).
	KindRestore = "restic-restore"
	// KindPrune prunes a bucket's snapshot repository (spec {"bucket": "b"}).
	KindPrune = "restic-prune"
)

// Snapshot tags and host.
const (
	// TagSnapshot marks whole-bucket snapshots.
	TagSnapshot = "gos3-snapshot"
	// TagPartial marks the partial snapshots a run chains its backups on
	// when the spool budget is reached; the run forgets them once its
	// complete snapshot is saved.
	TagPartial = "gos3-partial"
	// SnapshotHost is the host name of whole-bucket snapshots.
	SnapshotHost = "gos3-snapshot"
)

// Names in a snapshot's tree.
const (
	// ManifestName is the manifest at the snapshot's root.
	ManifestName = "gos3-manifest.jsonl"
	// BucketDocName is the bucket's description and configuration.
	BucketDocName = "gos3-bucket.json"
	// ObjectsDir holds the objects' files.
	ObjectsDir = "objects"
)

// Defaults of the Options.
const (
	// DefaultSpoolBytes is the default spool budget: the object data a run
	// stages before it saves a partial snapshot.
	DefaultSpoolBytes int64 = 8 << 30
	// DefaultWorkers is the default number of snapshot, restore and prune
	// jobs that run restic at once.
	DefaultWorkers = 1
)

// Options configure a Service.
type Options struct {
	// Router holds the buckets (required).
	Router *multi.Router
	// Runtime runs restic (nil: a new Runtime logging to Logger).
	Runtime *restic.Runtime
	// SystemDir keeps the scheduler state (restic-snapshots.json) and,
	// unless StageDir is set, the stage directories ("": no persistent
	// state; the stage is a private temporary directory of the process,
	// removed by Stop).
	SystemDir string
	// StageDir is where buckets are staged for backups ("":
	// <SystemDir>/restic-stage). The stage of a bucket is kept between runs
	// (mostly sparse placeholders): only changed objects are written to it.
	// It must be a directory private to the server's user (mode 0700; on
	// Windows its ACL is restricted like the system directory's).
	StageDir string
	// SpoolBytes bounds the object data a run stages before it saves a
	// partial snapshot and turns the staged files into placeholders
	// (default DefaultSpoolBytes). A single larger object is staged whole.
	SpoolBytes int64
	// Incremental stages objects unchanged since the parent snapshot as
	// placeholders, which restic takes from the parent without reading
	// them. Without it every run reads every object (the server passes
	// restic.snapshots.incremental, true by default: the zero value of
	// this field is a full run).
	Incremental bool
	// Workers is the number of jobs of this package that run restic at
	// once (default DefaultWorkers); further runs wait for a free worker.
	Workers int
	// Logger receives the runs' events (default slog.Default()).
	Logger *slog.Logger
	// Now returns the current time (default time.Now).
	Now func() time.Time
}

// Service runs whole-bucket snapshots. Create it with New; all methods are
// safe for concurrent use.
type Service struct {
	router *multi.Router
	rt     *restic.Runtime
	sysDir string
	// stageMu guards stageDir (empty until first used when tempStage) and
	// stageChecked.
	stageMu      sync.Mutex
	stageDir     string
	tempStage    bool // the stage is a temporary directory of this process
	stageChecked map[string]bool
	spool        int64
	incremental  bool
	log          *slog.Logger
	now          func() time.Time
	sem          chan struct{}

	// submitMu serializes the check for an active job of a bucket with the
	// submission of a new one.
	submitMu sync.Mutex

	mu         sync.Mutex
	mgr        *jobs.Manager
	state      map[string]*bucketState // by bucket name (see scheduler.go)
	configs    map[string]Config       // scheduler's view of the configs
	dirty      map[string]bool         // buckets whose config must be read again
	loadedAt   time.Time               // last full read of the configs
	cache      map[string]cachedSnapshots
	stageLocks map[string]*sync.Mutex

	saveMu sync.Mutex // serializes writes of the state file

	sched *scheduler // nil until Start
}

// New returns a Service. It loads the scheduler state from SystemDir; a
// state file of an unknown format version is refused.
func New(opts Options) (*Service, error) {
	if opts.Router == nil {
		return nil, errors.New("resticsnap: a router is required")
	}
	s := &Service{
		router:       opts.Router,
		rt:           opts.Runtime,
		sysDir:       opts.SystemDir,
		stageDir:     opts.StageDir,
		stageChecked: map[string]bool{},
		spool:        opts.SpoolBytes,
		incremental:  opts.Incremental,
		log:          opts.Logger,
		now:          opts.Now,
		state:        map[string]*bucketState{},
		configs:      map[string]Config{},
		dirty:        map[string]bool{},
		cache:        map[string]cachedSnapshots{},
		stageLocks:   map[string]*sync.Mutex{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rt == nil {
		s.rt = &restic.Runtime{Log: s.log}
	}
	if s.spool <= 0 {
		s.spool = DefaultSpoolBytes
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	s.sem = make(chan struct{}, workers)
	if s.stageDir == "" {
		if s.sysDir != "" {
			s.stageDir = filepath.Join(s.sysDir, "restic-stage")
		} else {
			// Not a predictable shared path: a private directory of this
			// process, made on first use (ensureStageRoot).
			s.tempStage = true
		}
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	return s, nil
}

// Runner returns the runner of the job kind kind (KindSnapshot, KindRestore
// or KindPrune), or nil for other kinds. The server registers them with the
// jobs.Manager it then passes to Start.
func (s *Service) Runner(kind string) jobs.Runner {
	switch kind {
	case KindSnapshot:
		return snapshotRunner{s}
	case KindRestore:
		return restoreRunner{s}
	case KindPrune:
		return pruneRunner{s}
	}
	return nil
}

// errNotStarted is returned by the methods that submit jobs before Start.
var errNotStarted = errors.New("resticsnap: the snapshot service is not started")

// manager returns the jobs.Manager given to Start.
func (s *Service) manager() (*jobs.Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mgr == nil {
		return nil, errNotStarted
	}
	return s.mgr, nil
}

// acquire waits for a free worker; release frees it.
func (s *Service) acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stageLock returns the in-process lock of the stage directory dir.
func (s *Service) stageLock(dir string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.stageLocks[dir]
	if l == nil {
		l = &sync.Mutex{}
		s.stageLocks[dir] = l
	}
	return l
}

// ensureStageRoot returns the stage root, creating it (without a system
// directory or StageDir: a new private temporary directory), after
// checking that it is a directory (not a link) private to the server's
// user: on Unix owned by it and mode 0700 (a looser mode is tightened),
// on Windows with the system directory's restricted ACL (applied once per
// process).
func (s *Service) ensureStageRoot() (string, error) {
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	if s.stageDir == "" {
		dir, err := os.MkdirTemp("", "gos3-restic-stage-")
		if err != nil {
			return "", fmt.Errorf("resticsnap: create the stage directory: %w", err)
		}
		s.stageDir = dir
	}
	root := s.stageDir
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("resticsnap: create the stage directory: %w", err)
	}
	if err := checkPrivateDir(root, !s.stageChecked[root]); err != nil {
		return "", fmt.Errorf("resticsnap: stage directory %s: %w", root, err)
	}
	s.stageChecked[root] = true
	return root, nil
}

// removeTempStage removes the temporary stage root of a Service without a
// system directory (Stop).
func (s *Service) removeTempStage() {
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	if !s.tempStage || s.stageDir == "" {
		return
	}
	if err := os.RemoveAll(s.stageDir); err != nil {
		s.log.Warn("resticsnap: could not remove the temporary stage directory", "path", s.stageDir, "err", err)
	}
	delete(s.stageChecked, s.stageDir)
	s.stageDir = ""
}

// stagePath returns the stage directory of bucket (with ID id, "" when
// unknown) for the repository repo below the stage root root:
// <root>/<sha256(repo)[:16]>/<id or bucket>.
func stagePath(root, repo, id, bucket string) string {
	sum := sha256.Sum256([]byte(repo))
	name := id
	if name == "" {
		name = bucket
	}
	return filepath.Join(root, hex.EncodeToString(sum[:])[:16], safeName(name))
}

// safeName returns name when it is a safe file name ([A-Za-z0-9._-],
// 1-128 characters, not "." or ".."), and else a hash of it.
func safeName(name string) string {
	ok := name != "" && len(name) <= 128 && name != "." && name != ".."
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
	}
	if ok {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "h-" + hex.EncodeToString(sum[:16])
}

// snapshotGuard returns the guard of restic.Runtime.Forget for the
// whole-bucket snapshots of bucket: host SnapshotHost, tags TagSnapshot and
// the bucket's tag, never a per-version archive snapshot.
func snapshotGuard(bucket string) func(restic.Snapshot) bool {
	tag := restic.BucketTag(bucket)
	return func(s restic.Snapshot) bool {
		return s.Host == SnapshotHost && s.HasTag(TagSnapshot) && s.HasTag(tag) && !s.HasTag(restic.TagObject)
	}
}

// invalidSpec returns an error wrapping jobs.ErrInvalidSpec.
func invalidSpec(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{jobs.ErrInvalidSpec}, args...)...)
}

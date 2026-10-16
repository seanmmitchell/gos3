package resticsnap

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// A restore (KindRestore) writes one of a bucket's snapshots into a target
// bucket (created when the request says so):
//
//  1. the target is created (on the backing, kind and container asked; with
//     object lock when asked, or else when the snapshot's bucket had it) and
//     gets the snapshot bucket's versioning; with configs "safe" the
//     snapshot's configuration documents are copied as a copy job copies
//     them (never replication or restic-snapshots);
//  2. the snapshot's manifest is indexed in a temporary bbolt database (by
//     file path, for the keys with the request's prefix);
//  3. `restic dump --archive tar` of the deepest directory holding those
//     files is streamed, and each file is written with copier.WriteVersion:
//     targets that import versions keep the version IDs and LastModified
//     times (and the manifest's delete markers are imported as such), other
//     targets get plain writes;
//  4. a file of the manifest missing from the archive fails the job.
//
// A retried run (and a run into a bucket that already had objects) skips
// the keys whose version the target already holds.

// restoreRequest is the body of POST .../snapshots/{id}/restore.
type restoreRequest struct {
	Target     restoreTarget `json:"target"`
	Prefix     string        `json:"prefix,omitempty"`
	OnExisting string        `json:"onExisting,omitempty"`
	Configs    string        `json:"configs,omitempty"`
}

// restoreTarget is the bucket a restore writes to.
type restoreTarget struct {
	Bucket string      `json:"bucket"`
	Create *createSpec `json:"create,omitempty"`
}

// createSpec creates a restore's target bucket (as CopyJobSpec's
// target.create).
type createSpec struct {
	Backing      string `json:"backing,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Container    string `json:"container,omitempty"`
	RemoteBucket string `json:"remoteBucket,omitempty"`
	ObjectLock   *bool  `json:"objectLock,omitempty"`
}

// restoreSpec is the spec of a KindRestore job.
type restoreSpec struct {
	Bucket     string        `json:"bucket"`
	Snapshot   string        `json:"snapshot"`
	Target     restoreTarget `json:"target"`
	Prefix     string        `json:"prefix,omitempty"`
	OnExisting string        `json:"onExisting"`
	Configs    string        `json:"configs"`
}

// restoreCheckpoint is a restore's progress across runs. It is saved as
// the job's checkpoint and, since checkpoints are written to disk lazily,
// synchronously to <job state directory>/restore.json too: a crash right
// after the job created or started writing to the target must not make
// its next run refuse the (non-empty) target as another's.
type restoreCheckpoint struct {
	// Created: the job created the target bucket.
	Created bool `json:"created,omitempty"`
	// Started: the target was prepared and writing started.
	Started bool `json:"started,omitempty"`
}

// merge adds o's progress to cp.
func (cp *restoreCheckpoint) merge(o restoreCheckpoint) {
	cp.Created = cp.Created || o.Created
	cp.Started = cp.Started || o.Started
}

// durableCheckpointName is the restore's checkpoint in its job's state
// directory.
const durableCheckpointName = "restore.json"

// loadDurableCheckpoint reads the checkpoint of the job's state directory
// (none without one).
func loadDurableCheckpoint(h *jobs.Handle) restoreCheckpoint {
	var cp restoreCheckpoint
	dir, err := h.StateDir()
	if err != nil {
		return cp
	}
	if raw, err := os.ReadFile(filepath.Join(dir, durableCheckpointName)); err == nil {
		_ = json.Unmarshal(raw, &cp)
	}
	return cp
}

// saveCheckpoint saves the run's progress: synchronously in the job's
// state directory (when there is one), and as the job's checkpoint.
func (r *restoreRun) saveCheckpoint() error {
	if dir, err := r.h.StateDir(); err == nil {
		raw, _ := json.Marshal(r.cp)
		if err := config.WriteFileAtomic(filepath.Join(dir, durableCheckpointName), raw, 0o600); err != nil {
			return fmt.Errorf("resticsnap: save the restore's progress: %w", err)
		}
	} else if !errors.Is(err, jobs.ErrNoStateDir) {
		return err
	}
	return r.h.SaveCheckpoint(r.cp)
}

// On-existing modes and config modes of a restore.
const (
	onExistingFail  = "fail"
	onExistingMerge = "merge"
	configsSafe     = "safe"
	configsNone     = "none"
)

// restoreRunner runs KindRestore jobs.
type restoreRunner struct{ s *Service }

// validBucketName reports whether name is a plausible bucket name (3-63
// lower-case letters, digits, '-' and '.', starting and ending with a
// letter or digit); the router has the final word.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 || strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (c != '-' && c != '.' || i == 0 || i == len(name)-1) {
			return false
		}
	}
	return true
}

// Validate implements jobs.Runner: the snapshot must be a complete
// whole-bucket snapshot of the bucket, and the target must exist (and be
// empty with onExisting "fail") or be created.
func (r restoreRunner) Validate(ctx context.Context, raw json.RawMessage) (json.RawMessage, map[string]string, error) {
	s := r.s
	var spec restoreSpec
	if err := decodeStrict(raw, &spec); err != nil {
		return nil, nil, invalidSpec("restore spec: %v", err)
	}
	switch {
	case spec.Bucket == "":
		return nil, nil, invalidSpec("restore spec: no bucket")
	case !restic.ValidSnapshot(spec.Snapshot):
		return nil, nil, invalidSpec("invalid snapshot ID %q (want 64 hex digits)", spec.Snapshot)
	case !validBucketName(spec.Target.Bucket):
		return nil, nil, invalidSpec("invalid target bucket name %q", spec.Target.Bucket)
	case len(spec.Prefix) > maxPrefix:
		return nil, nil, invalidSpec("prefix is longer than %d bytes", maxPrefix)
	}
	switch spec.OnExisting {
	case "":
		spec.OnExisting = onExistingFail
	case onExistingFail, onExistingMerge:
	default:
		return nil, nil, invalidSpec("onExisting %q: want %q or %q", spec.OnExisting, onExistingFail, onExistingMerge)
	}
	switch spec.Configs {
	case "":
		spec.Configs = configsSafe
	case configsSafe, configsNone:
	default:
		return nil, nil, invalidSpec("configs %q: want %q or %q", spec.Configs, configsSafe, configsNone)
	}
	if c := spec.Target.Create; c != nil {
		switch storage.BucketKind(c.Kind) {
		case "", storage.BucketKindNative, storage.BucketKindWrapped:
		default:
			return nil, nil, invalidSpec("target.create.kind %q: want %q or %q", c.Kind, storage.BucketKindNative, storage.BucketKindWrapped)
		}
		if c.Backing != "" {
			if _, ok := s.router.BackingBackend(c.Backing); !ok {
				return nil, nil, invalidSpec("unknown backing %q", c.Backing)
			}
		}
	}
	_, loc, ok, err := s.repositoryOf(ctx, spec.Bucket)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("%w: bucket %q has no snapshot repository", restic.ErrSnapshotNotFound, spec.Bucket)
	}
	if _, err := s.findSnapshot(ctx, loc, spec.Bucket, spec.Snapshot); err != nil {
		return nil, nil, err
	}
	_, err = s.router.GetBucket(ctx, spec.Target.Bucket)
	switch {
	case err == nil:
		if spec.OnExisting == onExistingFail {
			empty, err := s.bucketEmpty(ctx, spec.Target.Bucket)
			if err != nil {
				return nil, nil, err
			}
			if !empty {
				return nil, nil, fmt.Errorf("%w: target bucket %q is not empty (restore with onExisting %q to add to it)",
					jobs.ErrConflict, spec.Target.Bucket, onExistingMerge)
			}
		}
	case errors.Is(err, storage.ErrBucketNotFound):
		if spec.Target.Create == nil {
			return nil, nil, invalidSpec("target bucket %q does not exist (set target.create to create it)", spec.Target.Bucket)
		}
	default:
		return nil, nil, err
	}
	norm, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, err
	}
	return norm, map[string]string{"source": spec.Bucket, "target": spec.Target.Bucket}, nil
}

// findSnapshot returns the snapshot id of the repository loc, which must be
// a complete whole-bucket snapshot of bucket.
func (s *Service) findSnapshot(ctx context.Context, loc restic.Location, bucket, id string) (restic.Snapshot, error) {
	list, err := s.rt.Snapshots(ctx, loc, restic.SnapshotFilter{IDs: []string{id}})
	if err != nil {
		return restic.Snapshot{}, err
	}
	if len(list) == 0 {
		return restic.Snapshot{}, fmt.Errorf("%w: %s", restic.ErrSnapshotNotFound, id)
	}
	sn := list[0]
	if !snapshotGuard(bucket)(sn) {
		return restic.Snapshot{}, invalidSpec("snapshot %s is not a whole-bucket snapshot of bucket %q", id, bucket)
	}
	if sn.HasTag(TagPartial) || slices.Contains(s.stateOf(bucket).Unverified, id) {
		return restic.Snapshot{}, invalidSpec("snapshot %s is a partial or unverified snapshot", id)
	}
	return sn, nil
}

// bucketEmpty reports whether bucket has no versions or delete markers.
func (s *Service) bucketEmpty(ctx context.Context, bucket string) (bool, error) {
	empty := true
	err := s.router.WalkVersions(ctx, bucket, "", "", "", func(storage.ObjectInfo) error {
		empty = false
		return storage.ErrStopWalk
	})
	return empty, err
}

// Run implements jobs.Runner.
func (r restoreRunner) Run(ctx context.Context, h *jobs.Handle) error {
	s := r.s
	var spec restoreSpec
	if err := h.Spec(&spec); err != nil {
		return jobs.Permanent(err)
	}
	var cp restoreCheckpoint
	if _, err := h.Checkpoint(&cp); err != nil {
		return jobs.Permanent(err)
	}
	cp.merge(loadDurableCheckpoint(h))
	h.SetPhase("waiting")
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	rr := &restoreRun{s: s, h: h, spec: spec, cp: cp, retry: cp.Started}
	err = rr.run(ctx)
	if err == nil {
		h.Log().Info("resticsnap: snapshot restored", "bucket", spec.Bucket, "snapshot", spec.Snapshot,
			"target", spec.Target.Bucket, "keys", rr.status.Keys, "bytes", rr.status.Bytes)
	}
	return err
}

// restoreRun is one run of a KindRestore job.
type restoreRun struct {
	s      *Service
	h      *jobs.Handle
	spec   restoreSpec
	cp     restoreCheckpoint
	retry  bool // an earlier run started writing
	check  bool // compare each key with the target before writing it
	loc    restic.Location
	status jobStatus
	db     *bbolt.DB
	done   [][]byte // index paths restored, to delete from the index
}

// Index buckets of the temporary database.
var (
	idxFiles   = []byte("files")   // path -> entry
	idxMarkers = []byte("markers") // key -> entry
)

// restoreFailure marks errors that retrying cannot fix as permanent.
func restoreFailure(err error) error {
	for _, e := range []error{copier.ErrRetention, copier.ErrPlaintext, copier.ErrEncryptionUnsupported,
		copier.ErrNotVersioned, storage.ErrInvalidName, storage.ErrBucketNotFound, storage.ErrInvalidBacking,
		storage.ErrInvalidTarget, jobs.ErrInvalidSpec, restic.ErrSnapshotNotFound} {
		if errors.Is(err, e) {
			return jobs.Permanent(err)
		}
	}
	return err
}

// run restores the snapshot.
func (r *restoreRun) run(ctx context.Context) error {
	s, spec := r.s, r.spec
	r.h.SetPhase("preparing")
	info, loc, ok, err := s.repositoryOf(ctx, spec.Bucket)
	if err != nil {
		return restoreFailure(err)
	}
	if !ok {
		return jobs.Permanent(fmt.Errorf("%w: bucket %q has no snapshot repository", restic.ErrSnapshotNotFound, spec.Bucket))
	}
	r.loc = loc
	r.status.Repository, r.status.Snapshot = info.Repository, spec.Snapshot
	if _, err := s.findSnapshot(ctx, loc, spec.Bucket, spec.Snapshot); err != nil {
		return restoreFailure(err)
	}
	doc, err := r.bucketDoc(ctx)
	if err != nil {
		return err
	}
	if err := r.prepareTarget(ctx, doc); err != nil {
		return restoreFailure(err)
	}
	r.check = r.retry || spec.OnExisting == onExistingMerge

	r.h.SetPhase("indexing")
	dir, err := r.tempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	files, top, err := r.index(ctx, dir)
	if r.db != nil {
		defer r.db.Close()
	}
	if err != nil {
		return err
	}

	r.h.SetPhase("restoring")
	if files > 0 {
		if err := r.restoreFiles(ctx, top); err != nil {
			return err
		}
	}
	if err := r.restoreMarkers(ctx); err != nil {
		return err
	}
	_ = r.h.SetStatus(r.status)
	r.h.SetPhase("done")
	return nil
}

// tempDir creates the run's temporary directory: in the job's state
// directory, or else in the stage directory.
func (r *restoreRun) tempDir() (string, error) {
	parent, err := r.h.StateDir()
	if err != nil {
		if parent, err = r.s.ensureStageRoot(); err != nil {
			return "", err
		}
	}
	dir, err := os.MkdirTemp(parent, "restore-")
	if err != nil {
		return "", fmt.Errorf("resticsnap: %w", err)
	}
	return dir, nil
}

// bucketDoc reads the snapshot's bucket description (nil, with a warning,
// when it has none).
func (r *restoreRun) bucketDoc(ctx context.Context) (*bucketDoc, error) {
	var buf bytes.Buffer
	err := r.s.rt.DumpFile(ctx, r.loc, r.spec.Snapshot, "/"+BucketDocName, &buf)
	if err != nil {
		if ctx.Err() != nil || !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		r.status.Warnings = append(r.status.Warnings, "the snapshot has no bucket description: configurations are not restored")
		return nil, nil
	}
	var doc bucketDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil || doc.Format != bucketDocFormat {
		r.status.Warnings = append(r.status.Warnings, "the snapshot's bucket description cannot be read: configurations are not restored")
		return nil, nil
	}
	return &doc, nil
}

// prepareTarget creates the target bucket if needed (and gives it the
// snapshot bucket's versioning) and copies the configurations.
func (r *restoreRun) prepareTarget(ctx context.Context, doc *bucketDoc) error {
	s, spec := r.s, r.spec
	target := spec.Target.Bucket
	_, err := s.router.GetBucket(ctx, target)
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		c := spec.Target.Create
		if c == nil {
			return jobs.Permanent(invalidSpec("target bucket %q does not exist", target))
		}
		opts := storage.CreateBucketOptions{Backing: c.Backing, Import: storage.BucketKind(c.Kind) == storage.BucketKindWrapped}
		opts.ObjectLockEnabled = doc != nil && doc.Bucket.ObjectLockEnabled
		if c.ObjectLock != nil {
			opts.ObjectLockEnabled = *c.ObjectLock
		}
		if c.Kind != "" || c.Container != "" || c.RemoteBucket != "" {
			opts.Target = &storage.BucketTarget{Kind: storage.BucketKind(c.Kind), Container: c.Container, RemoteBucket: c.RemoteBucket}
		}
		if err := s.router.CreateBucket(ctx, target, opts); err != nil && !errors.Is(err, storage.ErrBucketExists) {
			return err
		}
		r.cp.Created = true
		if err := r.saveCheckpoint(); err != nil {
			return err
		}
	case err != nil:
		return err
	case !r.retry && !r.cp.Created && spec.OnExisting == onExistingFail:
		empty, err := s.bucketEmpty(ctx, target)
		if err != nil {
			return err
		}
		if !empty {
			return jobs.Permanent(fmt.Errorf("%w: target bucket %q is not empty", jobs.ErrConflict, target))
		}
	}
	if !r.retry {
		if r.cp.Created && doc != nil && doc.Bucket.Versioning != "" {
			if err := s.router.SetBucketVersioning(ctx, target, storage.VersioningStatus(doc.Bucket.Versioning)); err != nil {
				return err
			}
		}
		if spec.Configs == configsSafe && doc != nil {
			if err := r.copyConfigs(ctx, doc); err != nil {
				return err
			}
		}
	}
	r.cp.Started = true
	return r.saveCheckpoint()
}

// verbatimConfigs are the configuration documents a "safe" restore copies
// as they are.
var verbatimConfigs = []string{
	storage.ConfigCORS, storage.ConfigTagging, storage.ConfigEncryption, storage.ConfigWebsite,
	storage.ConfigOwnership, storage.ConfigPublicAccessBlock, storage.ConfigACL, storage.ConfigCompression,
	storage.ConfigLifecycle,
}

// copyConfigs copies the snapshot's configuration documents to the target
// the way a one-shot copy job's "safe" mode does: some verbatim, the
// encryption provider and object lock configuration when the target
// supports them, the bucket policy with its ARNs rewritten when it is
// valid for the target; never replication or restic-snapshots. What is
// skipped is reported as a warning.
func (r *restoreRun) copyConfigs(ctx context.Context, doc *bucketDoc) error {
	s := r.s
	src, target := r.spec.Bucket, r.spec.Target.Bucket
	get := func(key string) []byte {
		d, ok := doc.Configs[key]
		if !ok {
			return nil
		}
		data, err := d.bytes()
		if err != nil {
			r.status.Warnings = append(r.status.Warnings, fmt.Sprintf("configuration %s: %v", key, err))
			return nil
		}
		return data
	}
	put := func(key string, data []byte) error {
		return s.router.PutBucketConfig(ctx, target, key, data)
	}
	for _, key := range verbatimConfigs {
		if data := get(key); data != nil {
			if err := put(key, data); err != nil {
				return err
			}
		}
	}
	if data := get(storage.ConfigEncryptionProvider); data != nil {
		set, err := storage.ParseEncryptionSettings(data)
		if err == nil {
			err = s.router.ValidateEncryptionSettings(ctx, target, set)
		}
		if err == nil {
			err = put(storage.ConfigEncryptionProvider, data)
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			r.status.Warnings = append(r.status.Warnings, "encryption provider not restored: "+err.Error())
		}
	}
	if data := get(storage.ConfigObjectLock); data != nil {
		info, err := s.router.GetBucket(ctx, target)
		if err != nil {
			return err
		}
		if info.ObjectLockEnabled {
			if err := put(storage.ConfigObjectLock, data); err != nil {
				return err
			}
		} else {
			r.status.Warnings = append(r.status.Warnings, "object lock configuration not restored: the target bucket has no object lock")
		}
	}
	if data := get(storage.ConfigPolicy); data != nil {
		policy := rewritePolicy(string(data), src, target)
		if _, err := iam.ParseBucketPolicy([]byte(policy), target); err != nil {
			r.status.Warnings = append(r.status.Warnings, "bucket policy not restored: "+err.Error())
		} else if err := put(storage.ConfigPolicy, []byte(policy)); err != nil {
			return err
		}
	}
	return nil
}

// rewritePolicy rewrites the ARNs of bucket src (and its objects) in a
// bucket policy to bucket dst.
func rewritePolicy(policy, src, dst string) string {
	old, repl := "arn:aws:s3:::"+src, "arn:aws:s3:::"+dst
	var b strings.Builder
	for {
		i := strings.Index(policy, old)
		if i < 0 {
			b.WriteString(policy)
			return b.String()
		}
		b.WriteString(policy[:i])
		rest := policy[i+len(old):]
		if rest == "" || rest[0] == '"' || rest[0] == '/' {
			b.WriteString(repl)
		} else {
			b.WriteString(old)
		}
		policy = rest
	}
}

// index dumps the snapshot's manifest into dir and indexes its entries
// with the request's prefix: files by path, delete markers by key. It
// returns the number of files and the deepest directory holding them all.
func (r *restoreRun) index(ctx context.Context, dir string) (files int64, top string, err error) {
	name := filepath.Join(dir, "manifest.jsonl")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("resticsnap: %w", err)
	}
	err = r.s.rt.DumpFile(ctx, r.loc, r.spec.Snapshot, "/"+ManifestName, f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		if ctx.Err() == nil && strings.Contains(err.Error(), "not found") {
			return 0, "", jobs.Permanent(fmt.Errorf("resticsnap: snapshot %s has no manifest", r.spec.Snapshot))
		}
		return 0, "", err
	}
	if err := checkManifest(name); err != nil {
		if errors.Is(err, errBadManifest) {
			return 0, "", jobs.Permanent(err)
		}
		return 0, "", err
	}
	r.db, err = bbolt.Open(filepath.Join(dir, "index.db"), 0o600, &bbolt.Options{Timeout: time.Second, NoSync: true, NoFreelistSync: true})
	if err != nil {
		return 0, "", fmt.Errorf("resticsnap: %w", err)
	}
	in, err := openJSONL(name)
	if err != nil {
		return 0, "", fmt.Errorf("resticsnap: %w", err)
	}
	defer in.close()
	if _, err := readHeader(in); err != nil {
		return 0, "", jobs.Permanent(err)
	}
	type item struct {
		markers bool
		key     []byte
		val     []byte
	}
	var batch []item
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := r.db.Update(func(tx *bbolt.Tx) error {
			for _, it := range batch {
				name := idxFiles
				if it.markers {
					name = idxMarkers
				}
				b, err := tx.CreateBucketIfNotExists(name)
				if err != nil {
					return err
				}
				if err := b.Put(it.key, it.val); err != nil {
					return err
				}
			}
			return nil
		})
		batch = batch[:0]
		return err
	}
	var topParts []string
	for n := 0; ; n++ {
		if n%statusEvery == 0 {
			if err := ctx.Err(); err != nil {
				return 0, "", err
			}
		}
		var e entry
		ok, err := in.next(&e)
		if err != nil {
			return 0, "", jobs.Permanent(fmt.Errorf("resticsnap: manifest: %w", err))
		}
		if !ok {
			break
		}
		if !strings.HasPrefix(e.Key, r.spec.Prefix) {
			continue
		}
		e.prepareWrite()
		val, err := json.Marshal(e)
		if err != nil {
			return 0, "", err
		}
		if e.DeleteMarker {
			batch = append(batch, item{markers: true, key: []byte(e.Key), val: val})
		} else {
			batch = append(batch, item{key: []byte(e.Path), val: val})
			files++
			parts := strings.Split(path.Dir(e.Path), "/")
			if files == 1 {
				topParts = parts
			} else {
				i := 0
				for i < len(topParts) && i < len(parts) && topParts[i] == parts[i] {
					i++
				}
				topParts = topParts[:i]
			}
		}
		if len(batch) >= 10000 {
			if err := flush(); err != nil {
				return 0, "", fmt.Errorf("resticsnap: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return 0, "", fmt.Errorf("resticsnap: %w", err)
	}
	top = strings.Join(topParts, "/")
	if top == "" || top == "." {
		top = ObjectsDir
	}
	return files, top, nil
}

// lookup returns the indexed entry of key in the index bucket name.
func (r *restoreRun) lookup(name, key []byte) (*entry, error) {
	var e *entry
	err := r.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(name)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		e = &entry{}
		if err := json.Unmarshal(v, e); err != nil {
			return err
		}
		return e.finishRead()
	})
	return e, err
}

// markDone removes restored files from the index (in batches).
func (r *restoreRun) markDone(p []byte, force bool) error {
	if p != nil {
		r.done = append(r.done, p)
	}
	if len(r.done) == 0 || (!force && len(r.done) < 1000) {
		return nil
	}
	err := r.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(idxFiles)
		if b == nil {
			return nil
		}
		for _, k := range r.done {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	r.done = r.done[:0]
	return err
}

// restoreFiles streams the tar archive of the snapshot's directory top and
// writes the indexed files to the target.
func (r *restoreRun) restoreFiles(ctx context.Context, top string) error {
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		err := r.s.rt.DumpTar(dctx, r.loc, r.spec.Snapshot, "/"+top, pw)
		pw.CloseWithError(err)
		dumped <- err
	}()
	err := r.readArchive(ctx, tar.NewReader(pr))
	if err != nil {
		cancel()
		pr.CloseWithError(err)
		<-dumped
		return err
	}
	_, _ = io.Copy(io.Discard, pr)
	if err := <-dumped; err != nil {
		return err
	}
	if err := r.markDone(nil, true); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	var missing []string
	n := 0
	err = r.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(idxFiles)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			n++
			if len(missing) < 5 {
				missing = append(missing, string(k))
			}
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if n > 0 {
		return jobs.Permanent(fmt.Errorf("resticsnap: %d files of the manifest are missing from snapshot %s (for example %s)",
			n, r.spec.Snapshot, strings.Join(missing, ", ")))
	}
	return nil
}

// readArchive writes the archive's indexed files to the target.
func (r *restoreRun) readArchive(ctx context.Context, tr *tar.Reader) error {
	for n := 1; ; n++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("resticsnap: read the snapshot archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
		e, err := r.lookup(idxFiles, []byte(name))
		if err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		if e == nil {
			continue
		}
		if hdr.Size != e.Size {
			return jobs.Permanent(fmt.Errorf("resticsnap: %s has %d bytes in snapshot %s, its manifest says %d", e.Key, hdr.Size, r.spec.Snapshot, e.Size))
		}
		if err := r.write(ctx, e, tr); err != nil {
			return err
		}
		if err := r.markDone([]byte(name), false); err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		if n%100 == 0 {
			_ = r.h.SetStatus(r.status)
		}
	}
}

// restoreMarkers writes the indexed delete markers to the target.
func (r *restoreRun) restoreMarkers(ctx context.Context) error {
	var markers []entry
	err := r.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(idxMarkers)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if err := e.finishRead(); err != nil {
				return err
			}
			markers = append(markers, e)
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	for i := range markers {
		if err := r.write(ctx, &markers[i], nil); err != nil {
			return err
		}
	}
	return nil
}

// write writes the version of e (content from body) to the target, unless
// the target already holds it.
func (r *restoreRun) write(ctx context.Context, e *entry, body io.Reader) error {
	if r.check && r.identical(ctx, e) {
		r.status.Keys++
		return nil
	}
	w, err := copier.WriteVersion(ctx, r.s.router, r.spec.Target.Bucket, e.Key, e.objectInfo(), body, copier.Policy{}, copier.WriteImportOrPlain)
	if err != nil {
		return restoreFailure(fmt.Errorf("resticsnap: restore %q: %w", e.Key, err))
	}
	if !e.DeleteMarker && w.Bytes != e.Size {
		return jobs.Permanent(fmt.Errorf("resticsnap: restore %q: wrote %d bytes of %d", e.Key, w.Bytes, e.Size))
	}
	r.status.Keys++
	r.status.Bytes += w.Bytes
	r.status.Materialized++
	return nil
}

// identical reports whether the target already holds e's version: the
// imported version, or (after plain writes) a current version with the
// same size and ETag (a current delete marker for a delete marker).
func (r *restoreRun) identical(ctx context.Context, e *entry) bool {
	target := r.spec.Target.Bucket
	if oi, err := r.s.router.GetObjectInfo(ctx, target, e.Key, copier.MapVersionID(e.VersionID)); err == nil &&
		oi.DeleteMarker == e.DeleteMarker && oi.LastModified.Equal(e.LastModified) && (e.DeleteMarker || oi.Size == e.Size && oi.ETag == e.ETag) {
		return true
	}
	oi, err := r.s.router.GetObjectInfo(ctx, target, e.Key, "")
	if err != nil {
		return false
	}
	if e.DeleteMarker {
		return oi.DeleteMarker
	}
	return !oi.DeleteMarker && oi.Size == e.Size && oi.ETag == e.ETag
}

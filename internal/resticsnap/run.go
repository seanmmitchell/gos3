package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// A snapshot run (KindSnapshot) works in its stage directory
// <StageDir>/<sha256(repository)[:16]>/<bucket ID or name>:
//
//	tree/                  the backed-up tree: objects/…, the manifest, the
//	                       bucket description
//	stamp.json             the snapshot the tree was last backed up as
//	work/                  the run's files: the bucket listing, the parent's
//	                       manifest, the plan, the new manifest, the files
//	                       staged since the last backup
//
// Between runs the tree holds a sparse placeholder for each file of the
// last snapshot (and its manifest), and the stamp names that snapshot. A
// run whose parent is the stamped snapshot only touches what changed; any
// other run (no stamp, another parent, a full run) rebuilds the tree from
// the parent's manifest. The stamp is removed while a run changes the
// tree.
//
// A failed or interrupted run removes the tree and work/ (they would hold
// object data and listings in plain text, and the unstamped tree is not
// reused anyway); the stage of a bucket whose snapshot configuration is
// deleted, or that is deleted, is removed too (see scheduler.go).
//
// Steps:
//
//  1. scan: list the bucket into work/source.jsonl (consistent runs of
//     versioned buckets take each key's newest version at or before the
//     run's start, delete markers included);
//  2. plan: merge-join the listing with the parent's manifest, assign
//     paths (pathAssigner), decide each key (placeholder, materialize,
//     delete marker) and remove the parent's files that are not kept;
//  3. stage: first create the placeholders of all unchanged keys, so that
//     every backup of the run holds them as the parent does; then read the
//     other objects into the tree, saving a partial snapshot whenever the
//     staged data would exceed the spool budget;
//  4. finish: write the manifest and the bucket description and back up;
//     then promote the verified backup (below), turn the staged files into
//     placeholders, stamp the tree and forget the partial snapshots;
//  5. apply the retention, and prune when due.
//
// Each backup is verified (backup): restic must report at most as many new
// and changed files as the run wrote since the previous backup (the
// objects staged, plus the manifest and the bucket description for the
// last one). When it reports more, the run compares the snapshot with its
// parent (restic diff), forgets it, stages the real content of the files
// restic read instead of taking them from the parent, and backs up again,
// at most twice; then the job fails with a hint to disable incremental
// snapshots.
//
// Every backup is tagged TagPartial until it is verified, so that a
// snapshot that failed its verification is never a parent nor restored,
// even when forgetting it fails: the next run forgets it with the other
// stale partial snapshots. The verified last backup is promoted by backing
// up the same tree once more, on it, without TagPartial: restic reads
// nothing (anything it reports new or changed fails the run, and the
// promoted snapshot, if it cannot be forgotten, is recorded as unverified
// in the scheduler state and forgotten by the next run).
//
// A full run (Options.Incremental false) reads every object and gives its
// file a modification time unlike the parent's, so that restic reads it
// rather than taking the parent's content.

// maxVerifyRetries is the number of backups retried after a failed
// verification.
const maxVerifyRetries = 2

// statusEvery is the number of keys between two status updates.
const statusEvery = 1000

// jobStatus is SnapshotJobStatus: the status of KindSnapshot, KindRestore
// and KindPrune jobs.
type jobStatus struct {
	// Keys and Bytes count the keys (and their sizes) of the snapshot (a
	// restore: restored so far).
	Keys  int64 `json:"keys"`
	Bytes int64 `json:"bytes"`
	// Materialized counts the objects read from the bucket (not staged as
	// placeholders); Partials the partial snapshots saved.
	Materialized int64 `json:"materialized"`
	Partials     int   `json:"partials"`
	// Snapshot is the snapshot saved (a restore: restored).
	Snapshot string `json:"snapshot"`
	// Forgotten counts the snapshots forgotten (partial snapshots and
	// retention).
	Forgotten  int    `json:"forgotten"`
	Repository string `json:"repository"`
	// Warnings are problems that did not fail the job (retention, prune,
	// skipped configurations of a restore).
	Warnings []string `json:"warnings,omitempty"`
}

// snapshotRunner runs KindSnapshot jobs.
type snapshotRunner struct{ s *Service }

// Validate implements jobs.Runner: the bucket must exist and have a
// snapshot configuration.
func (r snapshotRunner) Validate(ctx context.Context, raw json.RawMessage) (json.RawMessage, map[string]string, error) {
	var spec bucketSpec
	if err := decodeStrict(raw, &spec); err != nil || spec.Bucket == "" {
		return nil, nil, invalidSpec("snapshot spec: want {\"bucket\": name}")
	}
	switch spec.Trigger {
	case "", "manual", "schedule":
	default:
		return nil, nil, invalidSpec("snapshot spec: unknown trigger %q", spec.Trigger)
	}
	if _, err := r.s.router.GetBucket(ctx, spec.Bucket); err != nil {
		return nil, nil, err
	}
	if _, err := r.s.loadConfig(ctx, spec.Bucket); err != nil {
		if errors.Is(err, storage.ErrConfigNotFound) {
			return nil, nil, invalidSpec("bucket %q has no snapshot configuration", spec.Bucket)
		}
		return nil, nil, err
	}
	norm, _ := json.Marshal(spec)
	return norm, map[string]string{"bucket": spec.Bucket}, nil
}

// Run implements jobs.Runner.
func (r snapshotRunner) Run(ctx context.Context, h *jobs.Handle) error {
	s := r.s
	var spec bucketSpec
	if err := h.Spec(&spec); err != nil {
		return jobs.Permanent(err)
	}
	h.SetPhase("waiting")
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	start := s.now()
	s.updateState(spec.Bucket, func(st *bucketState) { st.LastAttemptAt = start })
	run := &snapRun{s: s, h: h, bucket: spec.Bucket, asOf: start.UTC(), log: h.Log().With("bucket", spec.Bucket)}
	err = run.run(ctx)
	s.invalidate(spec.Bucket)
	switch {
	case err == nil:
		s.updateState(spec.Bucket, func(st *bucketState) {
			st.LastSuccessAt, st.LastSnapshot, st.LastError = s.now(), run.status.Snapshot, ""
			st.Repository = run.loc.Repository
		})
		run.log.Info("resticsnap: snapshot saved", "snapshot", run.status.Snapshot, "keys", run.status.Keys,
			"materialized", run.status.Materialized, "partials", run.status.Partials, "forgotten", run.status.Forgotten)
	case ctx.Err() == nil:
		s.updateState(spec.Bucket, func(st *bucketState) { st.LastError = err.Error() })
	}
	return err
}

// snapRun is one run of a KindSnapshot job.
type snapRun struct {
	s      *Service
	h      *jobs.Handle
	log    *slog.Logger
	bucket string
	// asOf is when the run started: a consistent run takes the versions
	// current then. snapTime is the snapshots' restic time: asOf to the
	// second, but always after the parent's, so that a bucket's snapshots
	// are ordered by time even when runs follow each other within a second.
	asOf, snapTime time.Time

	cfg      Config
	info     storage.BucketInfo
	bucketID string
	repo     restic.RepositoryInfo
	loc      restic.Location
	tags     []string

	stage, tree, objects, work string
	// stageValid: the tree holds the parent snapshot's placeholders.
	stageValid bool
	// parentManifest is the parent's manifest in work/ ("": none).
	parentManifest string
	// chain is the parent of the next backup: the parent snapshot, then
	// the last partial snapshot.
	chain       string
	incremental bool

	man       *jsonlWriter // the new manifest's entries
	manKeys   int64
	manBytes  int64
	chunk     *jsonlWriter // the files staged since the last backup
	chunkN    int64
	chunkSize int64
	// extra counts the files other than objects written since the last
	// backup (the manifest and the bucket description).
	extra int64
	// partials are the run's partial snapshots (the verified last backup
	// included once it is saved).
	partials []string
	// touched is set once the run removed the stamp: from then on a failed
	// run discards the stage.
	touched  bool
	status   jobStatus
	keysSeen int64
}

// warn logs a warning about the run.
func (r *snapRun) warn(msg string, args ...any) { r.log.Warn(msg, args...) }

// setStatus publishes the run's status.
func (r *snapRun) setStatus() { _ = r.h.SetStatus(r.status) }

// file returns the file of the tree path p ("objects/…").
func (r *snapRun) file(p string) string { return filepath.Join(r.tree, filepath.FromSlash(p)) }

// permanentIf marks err permanent when it says the bucket, its
// configuration or its target cannot work.
func permanentIf(err error) error {
	if errors.Is(err, storage.ErrBucketNotFound) || errors.Is(err, storage.ErrConfigNotFound) ||
		errors.Is(err, jobs.ErrInvalidSpec) {
		return jobs.Permanent(err)
	}
	return err
}

// run takes the snapshot.
func (r *snapRun) run(ctx context.Context) (err error) {
	s := r.s
	r.h.SetPhase("preparing")
	cfg, err := s.loadConfig(ctx, r.bucket)
	if err != nil {
		return permanentIf(err)
	}
	r.cfg = cfg
	if r.info, err = s.router.GetBucket(ctx, r.bucket); err != nil {
		return permanentIf(err)
	}
	if r.repo, r.loc, err = s.resolve(ctx, r.bucket, cfg.Target, true); err != nil {
		return permanentIf(targetError(err))
	}
	if cfg.Target.Kind == TargetBucket && restic.ValidTag(restic.BucketIDTag(r.repo.BucketID)) {
		r.bucketID = r.repo.BucketID
	}
	r.tags = []string{TagSnapshot, restic.BucketTag(r.bucket)}
	if r.bucketID != "" {
		r.tags = append(r.tags, restic.BucketIDTag(r.bucketID))
	}
	r.incremental = s.incremental
	r.status.Repository = r.loc.Repository
	r.setStatus()

	root, err := s.ensureStageRoot()
	if err != nil {
		return err
	}
	r.stage = stagePath(root, r.loc.Repository, r.bucketID, r.bucket)
	lock := s.stageLock(r.stage)
	lock.Lock()
	defer lock.Unlock()
	defer func() {
		// Open files would keep the stage from being removed.
		_ = r.man.close()
		_ = r.chunk.close()
		if err != nil && r.touched {
			r.discardStage()
		}
	}()
	r.tree = filepath.Join(r.stage, "tree")
	r.objects = filepath.Join(r.tree, ObjectsDir)
	r.work = filepath.Join(r.stage, "work")
	s.updateState(r.bucket, func(st *bucketState) { st.Stage = r.stage })

	snaps, err := s.listSnapshots(ctx, r.loc, r.bucket)
	if err != nil {
		return err
	}
	unverified := s.stateOf(r.bucket).Unverified
	var parent *restic.Snapshot
	var stale []string
	for i := range snaps {
		if snaps[i].HasTag(TagPartial) || slices.Contains(unverified, snaps[i].ID) {
			stale = append(stale, snaps[i].ID)
		} else {
			parent = &snaps[i] // oldest first: the last one is the newest
		}
	}
	if len(stale) > 0 {
		// Left by an interrupted or failed run (one run per bucket at a
		// time). They are never parents, whether forgetting them works.
		if err := s.forget(ctx, r.loc, stale, snapshotGuard(r.bucket)); err != nil {
			if ctx.Err() != nil {
				return err
			}
			r.warn("resticsnap: could not forget stale snapshots", "snapshots", stale, "err", err)
		} else {
			r.status.Forgotten += len(stale)
			s.updateState(r.bucket, func(st *bucketState) { st.Unverified = nil })
		}
	}
	r.snapTime = r.asOf.Truncate(time.Second)
	if parent != nil {
		r.chain = parent.ID
		if last := parent.Time.Truncate(time.Second); !r.snapTime.After(last) {
			r.snapTime = last.Add(time.Second)
		}
	}
	if err := r.prepareStage(parent); err != nil {
		return err
	}
	if err := r.loadParentManifest(ctx, parent); err != nil {
		return err
	}
	r.h.SetPhase("scanning")
	if err := r.scan(ctx); err != nil {
		return err
	}
	r.h.SetPhase("planning")
	if err := r.plan(ctx); err != nil {
		return err
	}
	r.h.SetPhase("staging")
	if err := r.stageAll(ctx); err != nil {
		r.cleanupPartials(ctx)
		return err
	}
	if err := r.finish(ctx); err != nil {
		r.cleanupPartials(ctx)
		return err
	}
	r.h.SetPhase("retention")
	r.applyRetention(ctx)
	r.maybePrune(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = os.RemoveAll(r.work)
	r.h.SetPhase("done")
	r.setStatus()
	return nil
}

// stamp is stamp.json: the snapshot the tree was backed up as.
type stamp struct {
	Snapshot   string `json:"snapshot"`
	Repository string `json:"repository"`
	Bucket     string `json:"bucket"`
	BucketID   string `json:"bucketId,omitempty"`
}

// prepareStage decides whether the tree can be reused (it was stamped as
// the parent snapshot), removes the stamp, and resets what cannot be
// reused.
func (r *snapRun) prepareStage(parent *restic.Snapshot) error {
	stampPath := filepath.Join(r.stage, "stamp.json")
	var st stamp
	if raw, err := os.ReadFile(stampPath); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	r.stageValid = r.incremental && parent != nil && st.Snapshot == parent.ID &&
		st.Repository == r.loc.Repository && st.Bucket == r.bucket
	if r.stageValid {
		if _, err := os.Stat(filepath.Join(r.tree, ManifestName)); err != nil {
			r.stageValid = false
		}
	}
	r.touched = true
	if err := os.Remove(stampPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if !r.stageValid {
		if err := os.RemoveAll(r.tree); err != nil {
			return fmt.Errorf("resticsnap: reset the stage: %w", err)
		}
	}
	if err := os.RemoveAll(r.work); err != nil {
		return fmt.Errorf("resticsnap: reset the stage: %w", err)
	}
	for _, d := range []string{r.objects, r.work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("resticsnap: create the stage: %w", err)
		}
	}
	return nil
}

// resetTree empties the tree (when the stamped manifest turned out to be
// unusable).
func (r *snapRun) resetTree() error {
	r.stageValid = false
	if err := os.RemoveAll(r.tree); err != nil {
		return fmt.Errorf("resticsnap: reset the stage: %w", err)
	}
	return os.MkdirAll(r.objects, 0o700)
}

// loadParentManifest puts the parent's manifest in work/parent.jsonl: the
// tree's, when the tree is the parent's, else dumped from the parent. A
// parent without a usable manifest has its files read again.
func (r *snapRun) loadParentManifest(ctx context.Context, parent *restic.Snapshot) error {
	if parent == nil {
		return nil
	}
	dst := filepath.Join(r.work, "parent.jsonl")
	if r.stageValid {
		if err := os.Rename(filepath.Join(r.tree, ManifestName), dst); err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
	} else {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		err = r.s.rt.DumpFile(ctx, r.loc, parent.ID, "/"+ManifestName, f)
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("resticsnap: %w", cerr)
		}
		if err != nil {
			if ctx.Err() != nil || !strings.Contains(err.Error(), "not found") {
				return err
			}
			r.warn("resticsnap: the parent snapshot has no manifest; every object is read again", "parent", parent.ID)
			return nil
		}
	}
	if err := checkManifest(dst); err != nil {
		if !errors.Is(err, errBadManifest) {
			return err
		}
		r.warn("resticsnap: the parent snapshot's manifest cannot be used; every object is read again", "parent", parent.ID, "err", err)
		if r.stageValid {
			return r.resetTree()
		}
		return nil
	}
	r.parentManifest = dst
	return nil
}

// scan lists the bucket into work/source.jsonl.
func (r *snapRun) scan(ctx context.Context) error {
	w, err := createJSONL(filepath.Join(r.work, "source.jsonl"))
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	var prev string
	add := func(oi storage.ObjectInfo) error {
		if r.keysSeen > 0 && oi.Key <= prev {
			return fmt.Errorf("resticsnap: the listing of bucket %q is not in key order at %q", r.bucket, oi.Key)
		}
		prev = oi.Key
		e := entryOf(oi)
		r.keysSeen++
		if !e.DeleteMarker {
			r.status.Keys++
			r.status.Bytes += e.Size
		}
		if r.keysSeen%statusEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.setStatus()
		}
		return w.write(&e)
	}
	versioned := r.info.Versioning != storage.VersioningUnversioned
	if r.cfg.Consistent && versioned {
		var cur string
		var have, chosen bool
		err = r.s.router.WalkVersions(ctx, r.bucket, r.cfg.Prefix, "", "", func(oi storage.ObjectInfo) error {
			if !have || oi.Key != cur {
				cur, have, chosen = oi.Key, true, false
			}
			if chosen || oi.LastModified.After(r.asOf) {
				return nil
			}
			chosen = true
			return add(oi)
		})
	} else {
		err = r.s.router.WalkObjects(ctx, r.bucket, r.cfg.Prefix, "", add)
	}
	if cerr := w.close(); err == nil && cerr != nil {
		err = fmt.Errorf("resticsnap: %w", cerr)
	}
	r.setStatus()
	return err
}

// entryStream reads entries one at a time.
type entryStream struct {
	r   *jsonlReader
	cur *entry
}

// advance reads the next entry (cur is nil at the end).
func (st *entryStream) advance() error {
	st.cur = nil
	if st.r == nil {
		return nil
	}
	var e entry
	ok, err := st.r.next(&e)
	if err != nil {
		return err
	}
	if ok {
		st.cur = &e
	}
	return nil
}

// plan merge-joins the listing with the parent's manifest into
// work/plan.jsonl, removing the parent's files the new tree does not keep.
func (r *snapRun) plan(ctx context.Context) error {
	srcR, err := openJSONL(filepath.Join(r.work, "source.jsonl"))
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	defer srcR.close()
	src := &entryStream{r: srcR}
	par := &entryStream{}
	if r.parentManifest != "" {
		pr, err := openJSONL(r.parentManifest)
		if err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		defer pr.close()
		if _, err := readHeader(pr); err != nil {
			return err
		}
		par.r = pr
	}
	out, err := createJSONL(filepath.Join(r.work, "plan.jsonl"))
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	defer out.close()
	as := newPathAssigner()
	if err := src.advance(); err != nil {
		return err
	}
	if err := par.advance(); err != nil {
		return err
	}
	for n := 0; src.cur != nil || par.cur != nil; n++ {
		if n%statusEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		s, p := src.cur, par.cur
		switch {
		case s == nil || (p != nil && p.Key < s.Key):
			if err := r.drop(p); err != nil {
				return err
			}
			err = par.advance()
		case p == nil || s.Key < p.Key:
			if err := r.planOne(out, as, s, nil); err != nil {
				return err
			}
			err = src.advance()
		default:
			if err := r.planOne(out, as, s, p); err != nil {
				return err
			}
			if err = src.advance(); err == nil {
				err = par.advance()
			}
		}
		if err != nil {
			return err
		}
	}
	return out.close()
}

// planOne plans the key listed as s, whose parent entry is p (nil: none).
func (r *snapRun) planOne(out *jsonlWriter, as *pathAssigner, s, p *entry) error {
	e := *s
	if e.DeleteMarker {
		e.Action = planMarker
		if err := r.drop(p); err != nil {
			return err
		}
		return out.write(&e)
	}
	e.Path = ObjectsDir + "/" + as.assign(e.Key)
	samePath := p != nil && !p.DeleteMarker && p.Path == e.Path
	unchanged := samePath && p.Fingerprint == e.Fingerprint
	switch {
	case samePath:
		e.ParentMTime = p.MTime
	case p != nil:
		if err := r.drop(p); err != nil {
			return err
		}
	}
	if unchanged && r.incremental {
		e.MTime, e.Action = p.MTime, planKeep
	} else {
		// Read (also unchanged keys of a full run): a time unlike the
		// parent's file at the path, so that restic reads the file.
		e.MTime, e.Action = mtimeFor(e.LastModified, e.ParentMTime), planMaterialize
	}
	return out.write(&e)
}

// drop removes the parent's file of entry p from a reused tree.
func (r *snapRun) drop(p *entry) error {
	if p == nil || !r.stageValid || p.DeleteMarker || p.Path == "" {
		return nil
	}
	return r.removeStaged(p.Path)
}

// removeStaged removes the tree's file p and the directories it leaves
// empty.
func (r *snapRun) removeStaged(p string) error {
	name := r.file(p)
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resticsnap: %w", err)
	}
	for dir := filepath.Dir(name); len(dir) > len(r.objects) && strings.HasPrefix(dir, r.objects); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// eachPlanned calls fn with each entry of the plan.
func (r *snapRun) eachPlanned(ctx context.Context, fn func(entry) error) error {
	in, err := openJSONL(filepath.Join(r.work, "plan.jsonl"))
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	defer in.close()
	for n := int64(1); ; n++ {
		var e entry
		ok, err := in.next(&e)
		if err != nil {
			return fmt.Errorf("resticsnap: plan: %w", err)
		}
		if !ok {
			return nil
		}
		if n%statusEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

// stageAll stages the planned keys: first the placeholders of the
// unchanged keys, then the objects read from the bucket, saving partial
// snapshots as the spool budget requires. It writes the entries of the
// new manifest to work/manifest.jsonl.
func (r *snapRun) stageAll(ctx context.Context) error {
	// Every backup of the run, partial ones included, must hold the
	// unchanged keys' placeholders with the parent's size and time:
	// placeholders added after a partial snapshot would be new files to
	// restic, read as zeros.
	err := r.eachPlanned(ctx, func(e entry) error {
		if e.Action == planKeep {
			return r.placeholder(e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.man, err = createJSONL(filepath.Join(r.work, "manifest.jsonl")); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if r.chunk, err = createJSONL(filepath.Join(r.work, "chunk.jsonl")); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	n := 0
	return r.eachPlanned(ctx, func(e entry) error {
		if n++; n%statusEvery == 0 {
			r.setStatus()
		}
		switch e.Action {
		case planMarker, planKeep:
			return r.addManifest(e)
		case planMaterialize:
			if r.chunkSize > 0 && r.chunkSize+e.Size > r.s.spool {
				if err := r.partial(ctx); err != nil {
					return err
				}
			}
			return r.materialize(ctx, &e)
		}
		return fmt.Errorf("resticsnap: plan: unknown action %q", e.Action)
	})
}

// addManifest adds e to the new manifest.
func (r *snapRun) addManifest(e entry) error {
	m := e.manifestEntry()
	r.manKeys++
	r.manBytes += m.Size
	return r.man.write(&m)
}

// placeholder stages the unchanged file of e as a placeholder (a reused
// tree has it already).
func (r *snapRun) placeholder(e entry) error {
	name := r.file(e.Path)
	mt := time.Unix(e.MTime, 0)
	if r.stageValid {
		if fi, err := os.Lstat(name); err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size && fi.ModTime().Equal(mt) {
			return nil
		}
	}
	return makePlaceholder(name, e.Size, mt)
}

// makePlaceholder replaces the file name by a placeholder (removing it
// first, so that a link there is not followed).
func makePlaceholder(name string, size int64, mt time.Time) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	return restic.MakePlaceholder(name, size, mt)
}

// materialize reads the object of e into the tree. A version gone since
// the listing leaves the snapshot; a version that changed since (a null
// version overwritten) is staged as it is now.
func (r *snapRun) materialize(ctx context.Context, e *entry) error {
	info, rc, err := r.s.router.OpenObject(ctx, r.bucket, e.Key, e.VersionID)
	switch {
	case errors.Is(err, storage.ErrObjectNotFound) || errors.Is(err, storage.ErrVersionNotFound):
		return r.removeStaged(e.Path)
	case err != nil:
		return err
	}
	info.Key = e.Key
	now := entryOf(info)
	if info.DeleteMarker {
		if err := r.removeStaged(e.Path); err != nil {
			return err
		}
		return r.addManifest(now)
	}
	defer rc.Close()
	now.Path, now.ParentMTime = e.Path, e.ParentMTime
	if now.Fingerprint == e.Fingerprint {
		now.MTime = e.MTime
	} else {
		now.MTime = mtimeFor(now.LastModified, now.ParentMTime)
	}
	*e = now
	if err := writeStaged(r.file(e.Path), rc, e.Size, e.MTime); err != nil {
		return err
	}
	if err := r.addChunk(*e); err != nil {
		return err
	}
	return r.addManifest(*e)
}

// writeStaged writes the size bytes of src to the file name with the
// modification time mt (Unix seconds).
func writeStaged(name string, src io.Reader, size, mt int64) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	// A new file (O_EXCL does not follow a link left in its place).
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	if n != size {
		return fmt.Errorf("resticsnap: stage: read %d bytes of an object of %d bytes", n, size)
	}
	if err := os.Chtimes(name, time.Time{}, time.Unix(mt, 0)); err != nil {
		return fmt.Errorf("resticsnap: stage: %w", err)
	}
	return nil
}

// addChunk records a file staged since the last backup.
func (r *snapRun) addChunk(e entry) error {
	r.chunkN++
	r.chunkSize += e.Size
	r.status.Materialized++
	c := entry{Key: e.Key, Path: e.Path, Size: e.Size, MTime: e.MTime}
	return r.chunk.write(&c)
}

// eachChunkFile calls fn with each file staged since the last backup.
func (r *snapRun) eachChunkFile(fn func(entry) error) error {
	if err := r.chunk.flush(); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	in, err := openJSONL(r.chunk.f.Name())
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	defer in.close()
	for {
		var e entry
		ok, err := in.next(&e)
		if err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		if !ok {
			return nil
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

// flattenChunk turns the files staged since the last backup into
// placeholders (the backup holds their content) and starts a new chunk.
func (r *snapRun) flattenChunk() error {
	err := r.eachChunkFile(func(c entry) error {
		name := r.file(c.Path)
		if fi, err := os.Lstat(name); err != nil || !fi.Mode().IsRegular() {
			return nil // gone since (or not ours)
		}
		return makePlaceholder(name, c.Size, time.Unix(c.MTime, 0))
	})
	if err != nil {
		return err
	}
	path := r.chunk.f.Name()
	if err := r.chunk.close(); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if r.chunk, err = createJSONL(path); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	r.chunkN, r.chunkSize, r.extra = 0, 0, 0
	return nil
}

// partial saves a partial snapshot of the tree and chains the next backup
// on it.
func (r *snapRun) partial(ctx context.Context) error {
	sum, err := r.backup(ctx)
	if err != nil {
		return err
	}
	r.partials = append(r.partials, sum.SnapshotID)
	r.status.Partials++
	r.chain = sum.SnapshotID
	r.setStatus()
	r.h.SetPhase("staging")
	return r.flattenChunk()
}

// backup backs the tree up on r.chain, as a snapshot tagged TagPartial,
// and verifies it (see the top of the file). A snapshot that fails is
// forgotten (on a detached context); when that fails too, its tag keeps it
// from being used, and the next run forgets it.
func (r *snapRun) backup(ctx context.Context) (restic.Summary, error) {
	tags := append(slices.Clone(r.tags), TagPartial)
	for attempt := 0; ; attempt++ {
		r.h.SetPhase("backing up")
		sum, err := r.s.rt.BackupTree(ctx, r.loc, r.tree, restic.TreeOptions{
			Parent: r.chain, Tags: tags, Host: SnapshotHost, Time: r.snapTime, IgnoreInode: true, IgnoreCtime: true,
		})
		if err != nil {
			if sum.SnapshotID != "" {
				_ = r.forgetDetached(ctx, sum.SnapshotID, false) // an incomplete snapshot
			}
			return restic.Summary{}, err
		}
		if sum.FilesNew+sum.FilesChanged <= r.chunkN+r.extra {
			return sum, nil
		}
		r.h.SetPhase("verifying")
		r.warn("resticsnap: restic read more files than were written; comparing with the parent snapshot",
			"snapshot", sum.SnapshotID, "new", sum.FilesNew, "changed", sum.FilesChanged, "written", r.chunkN+r.extra)
		paths, err := r.offenders(ctx, sum.SnapshotID)
		if err != nil {
			_ = r.forgetDetached(ctx, sum.SnapshotID, false)
			return restic.Summary{}, err
		}
		if len(paths) == 0 {
			// restic counted files it read, but their content is the
			// parent's: the snapshot is right.
			return sum, nil
		}
		if err := r.forgetDetached(ctx, sum.SnapshotID, false); err != nil {
			return restic.Summary{}, err
		}
		if attempt >= maxVerifyRetries {
			return restic.Summary{}, jobs.Permanent(r.verifyError(len(paths)))
		}
		if err := r.materializeOffenders(ctx, paths); err != nil {
			return restic.Summary{}, err
		}
	}
}

// forgetTimeout bounds the forgets of snapshots a run must not leave
// behind, which run on a context detached from the job's (a canceled job
// still cleans up).
const forgetTimeout = 2 * time.Minute

// forgetDetached forgets the snapshots ids on a detached context. When
// complete (the snapshots lack TagPartial) and forgetting fails, they are
// recorded as unverified in the scheduler state: never parents, never
// listed or restored, forgotten by the next run.
func (r *snapRun) forgetDetached(ctx context.Context, id string, complete bool) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetTimeout)
	defer cancel()
	err := r.s.forget(fctx, r.loc, []string{id}, snapshotGuard(r.bucket))
	if err != nil {
		r.warn("resticsnap: could not forget an unverified snapshot", "snapshot", id, "err", err)
		if complete {
			r.s.updateState(r.bucket, func(st *bucketState) {
				if !slices.Contains(st.Unverified, id) {
					st.Unverified = append(st.Unverified, id)
				}
			})
		}
	}
	return err
}

// verifyError is the error of a run whose backups restic could not take
// from placeholders.
func (r *snapRun) verifyError(n int) error {
	hint := "set restic.snapshots.incremental: false"
	if !r.incremental {
		hint = "raise restic.snapshots.spool_bytes above the bucket's size, so that a snapshot needs no partial snapshots"
	}
	return fmt.Errorf("resticsnap: restic read %d placeholder files instead of taking their content from the parent snapshot "+
		"(the stage's file system or restic does not keep their size and modification time); %s", n, hint)
}

// offenders returns the paths (below objects/) of the files snapshot
// changed or added compared with r.chain that were not staged since the
// last backup.
func (r *snapRun) offenders(ctx context.Context, snapshot string) ([]string, error) {
	if r.chain == "" {
		return nil, jobs.Permanent(fmt.Errorf("resticsnap: restic reported more files than were staged in a snapshot without a parent"))
	}
	staged := make(map[string]bool, r.chunkN)
	if err := r.eachChunkFile(func(c entry) error {
		staged[c.Path] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return r.changedObjects(ctx, r.chain, snapshot, staged)
}

// changedObjects returns the paths (below objects/) of the files snapshot
// to changed or added compared with snapshot from, except those in skip.
func (r *snapRun) changedObjects(ctx context.Context, from, to string, skip map[string]bool) ([]string, error) {
	diff, err := r.s.rt.Diff(ctx, r.loc, from, to)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range diff {
		p := strings.TrimPrefix(d.Path, "/")
		if strings.HasSuffix(p, "/") || !strings.HasPrefix(p, ObjectsDir+"/") || !strings.ContainsAny(d.Modifier, "+MT?") || skip[p] {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// materializeOffenders stages the real content of the files at paths
// (placeholders restic read): the versions the manifest so far, or the
// plan for keys not staged yet, records there.
func (r *snapRun) materializeOffenders(ctx context.Context, paths []string) error {
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	found := map[string]entry{}
	if err := r.man.flush(); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	for _, name := range []string{r.man.f.Name(), filepath.Join(r.work, "plan.jsonl")} {
		in, err := openJSONL(name)
		if err != nil {
			return fmt.Errorf("resticsnap: %w", err)
		}
		for {
			var e entry
			ok, err := in.next(&e)
			if err != nil {
				in.close()
				return fmt.Errorf("resticsnap: %w", err)
			}
			if !ok {
				break
			}
			if _, done := found[e.Path]; want[e.Path] && !done && !e.DeleteMarker {
				found[e.Path] = e
			}
		}
		in.close()
	}
	if len(found) < len(want) {
		return jobs.Permanent(fmt.Errorf("resticsnap: restic changed %d files the snapshot run did not stage", len(want)-len(found)))
	}
	var total int64
	for _, e := range found {
		total += e.Size
	}
	if total > r.s.spool {
		return jobs.Permanent(r.verifyError(len(found)))
	}
	r.h.SetPhase("staging")
	for _, p := range slices.Sorted(maps.Keys(found)) {
		e := found[p]
		info, rc, err := r.s.router.OpenObject(ctx, r.bucket, e.Key, e.VersionID)
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotFound) || errors.Is(err, storage.ErrVersionNotFound) {
				return fmt.Errorf("resticsnap: object %q changed during the snapshot's verification", e.Key)
			}
			return err
		}
		info.Key = e.Key
		if info.DeleteMarker || entryOf(info).Fingerprint != e.Fingerprint {
			if rc != nil {
				rc.Close()
			}
			return fmt.Errorf("resticsnap: object %q changed during the snapshot's verification", e.Key)
		}
		err = writeStaged(r.file(e.Path), rc, e.Size, e.MTime)
		rc.Close()
		if err != nil {
			return err
		}
		if err := r.addChunk(e); err != nil {
			return err
		}
	}
	r.setStatus()
	return nil
}

// cleanupPartials forgets the run's partial snapshots after a failure, on
// a detached context (best effort: the next run forgets what is left).
func (r *snapRun) cleanupPartials(ctx context.Context) {
	if len(r.partials) == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetTimeout)
	defer cancel()
	if err := r.s.forget(fctx, r.loc, r.partials, snapshotGuard(r.bucket)); err != nil {
		r.warn("resticsnap: could not forget the partial snapshots of a failed run", "err", err)
	}
}

// discardStage removes the tree, work/ and the stamp of a failed run: the
// object data and listings it staged, in plain text.
func (r *snapRun) discardStage() {
	for _, p := range []string{r.tree, r.work, filepath.Join(r.stage, "stamp.json")} {
		if err := os.RemoveAll(p); err != nil {
			r.warn("resticsnap: could not remove the stage of a failed run", "path", p, "err", err)
		}
	}
}

// finish writes the manifest and the bucket description, takes the
// snapshot and promotes it, stamps the tree and forgets the partial
// snapshots.
func (r *snapRun) finish(ctx context.Context) error {
	if err := r.man.close(); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if err := r.writeManifest(); err != nil {
		return err
	}
	if err := r.writeBucketDoc(ctx); err != nil {
		return err
	}
	r.extra = 2
	sum, err := r.backup(ctx)
	if err != nil {
		return err
	}
	verified := sum.SnapshotID
	r.partials = append(r.partials, verified)
	id, err := r.promote(ctx, sum)
	if err != nil {
		return err
	}
	r.status.Snapshot = id
	r.setStatus()
	if err := r.flattenChunk(); err != nil {
		return err
	}
	if err := r.chunk.close(); err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	raw, _ := json.Marshal(stamp{Snapshot: id, Repository: r.loc.Repository, Bucket: r.bucket, BucketID: r.bucketID})
	if err := config.WriteFileAtomic(filepath.Join(r.stage, "stamp.json"), raw, 0o600); err != nil {
		return fmt.Errorf("resticsnap: stamp the stage: %w", err)
	}
	if err := r.s.forget(ctx, r.loc, r.partials, snapshotGuard(r.bucket)); err != nil {
		if ctx.Err() != nil {
			return err
		}
		r.status.Warnings = append(r.status.Warnings, "forgetting the partial snapshots: "+err.Error())
	} else {
		r.status.Forgotten += r.status.Partials
	}
	return nil
}

// promote backs the unchanged tree up again on the verified snapshot of
// summary verified, without TagPartial and with the verified backup's
// summary as a tag (summaryTag: the promotion's own summary reports
// nothing new), and returns the new snapshot's ID. restic must read
// nothing; a promotion it changed is forgotten (or recorded as unverified)
// and fails the run.
func (r *snapRun) promote(ctx context.Context, verifiedSum restic.Summary) (string, error) {
	r.h.SetPhase("promoting")
	verified := verifiedSum.SnapshotID
	sum, err := r.s.rt.BackupTree(ctx, r.loc, r.tree, restic.TreeOptions{
		Parent: verified, Tags: append(slices.Clone(r.tags), summaryTag(verifiedSum)),
		Host: SnapshotHost, Time: r.snapTime, IgnoreInode: true, IgnoreCtime: true,
	})
	if err != nil {
		if sum.SnapshotID != "" {
			_ = r.forgetDetached(ctx, sum.SnapshotID, true)
		}
		return "", err
	}
	if sum.FilesNew+sum.FilesChanged == 0 {
		return sum.SnapshotID, nil
	}
	changed, err := r.changedObjects(ctx, verified, sum.SnapshotID, nil)
	if err == nil && len(changed) == 0 {
		return sum.SnapshotID, nil
	}
	_ = r.forgetDetached(ctx, sum.SnapshotID, true)
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("resticsnap: restic changed %d files backing up a verified snapshot's tree again", len(changed))
}

// writeManifest writes the tree's manifest: the header and the entries.
func (r *snapRun) writeManifest() error {
	h := manifestHeader{
		Format: manifestFormat, Version: formatVersion, Bucket: r.bucket, BucketID: r.bucketID, AsOf: r.asOf,
		Prefix: r.cfg.Prefix, Consistent: r.cfg.Consistent, Versioning: string(r.info.Versioning),
		Keys: r.manKeys, Bytes: r.manBytes,
	}
	tmp := filepath.Join(r.tree, ManifestName+".tmp")
	w, err := createJSONL(tmp)
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	if err := w.write(h); err != nil {
		w.close()
		return fmt.Errorf("resticsnap: %w", err)
	}
	in, err := os.Open(r.man.f.Name())
	if err != nil {
		w.close()
		return fmt.Errorf("resticsnap: %w", err)
	}
	_, err = io.Copy(w.w, in)
	in.Close()
	if cerr := w.close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(r.tree, ManifestName))
	}
	if err != nil {
		return fmt.Errorf("resticsnap: write the manifest: %w", err)
	}
	return nil
}

// writeBucketDoc writes the tree's bucket description.
func (r *snapRun) writeBucketDoc(ctx context.Context) error {
	in := r.info
	doc := bucketDoc{
		Format: bucketDocFormat, Version: formatVersion, AsOf: r.asOf, Configs: map[string]configDoc{},
		Bucket: bucketInfo{
			Name: r.bucket, ID: r.bucketID, CreatedAt: in.CreatedAt.UTC(), Region: in.Region, Versioning: string(in.Versioning),
			ObjectLockEnabled: in.ObjectLockEnabled, Owner: in.Owner, Backing: in.Backing, Kind: string(in.Kind), Location: in.Location,
		},
	}
	for _, key := range storage.AllConfigKeys {
		raw, err := r.s.router.GetBucketConfig(ctx, r.bucket, key)
		if errors.Is(err, storage.ErrConfigNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		doc.Configs[key] = newConfigDoc(raw)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.tree, BucketDocName), raw, 0o600); err != nil {
		return fmt.Errorf("resticsnap: write the bucket description: %w", err)
	}
	return nil
}

// applyRetention forgets the bucket's complete snapshots the retention
// policy does not keep (never the new one). Failures are warnings.
func (r *snapRun) applyRetention(ctx context.Context) {
	snaps, err := r.s.listSnapshots(ctx, r.loc, r.bucket)
	if err != nil {
		r.status.Warnings = append(r.status.Warnings, "retention: "+err.Error())
		return
	}
	snaps = slices.DeleteFunc(snaps, func(sn restic.Snapshot) bool { return sn.HasTag(TagPartial) })
	_, drop := selectRetention(snaps, r.cfg.Retention)
	var ids []string
	for _, sn := range drop {
		if sn.ID != r.status.Snapshot {
			ids = append(ids, sn.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := r.s.forget(ctx, r.loc, ids, snapshotGuard(r.bucket)); err != nil {
		r.status.Warnings = append(r.status.Warnings, "retention: "+err.Error())
		return
	}
	r.status.Forgotten += len(ids)
	r.setStatus()
}

// maybePrune prunes the repository when the configuration's prune
// interval has passed since the last prune (the first run only starts the
// clock). Failures are warnings.
func (r *snapRun) maybePrune(ctx context.Context) {
	enabled := r.cfg.Prune.Enabled
	if r.cfg.Prune.Default {
		// The default follows the bucket's archive scope now: a prune's
		// exclusive lock would stall an archive of every write.
		all, err := r.s.archiveScopeAll(ctx, r.bucket)
		if err != nil {
			r.status.Warnings = append(r.status.Warnings, "prune: "+err.Error())
			return
		}
		enabled = !all
	}
	if !enabled || ctx.Err() != nil {
		return
	}
	now := r.s.now()
	last := r.s.stateOf(r.bucket).LastPruneAt
	if last.IsZero() {
		r.s.updateState(r.bucket, func(st *bucketState) { st.LastPruneAt = now })
		return
	}
	if now.Sub(last) < r.cfg.pruneInterval() {
		return
	}
	r.h.SetPhase("pruning")
	if err := r.s.prune(ctx, r.loc, restic.PruneOptions{RetryLock: pruneRetryLock}); err != nil {
		r.status.Warnings = append(r.status.Warnings, "prune: "+err.Error())
		return
	}
	r.s.updateState(r.bucket, func(st *bucketState) { st.LastPruneAt = now })
}

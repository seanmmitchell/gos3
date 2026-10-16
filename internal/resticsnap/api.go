package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// snapshotsTTL is how long a listing of a bucket's snapshots is cached.
const snapshotsTTL = 30 * time.Second

// cachedSnapshots is a cached listing of a bucket's snapshots.
type cachedSnapshots struct {
	at         time.Time
	repository string
	list       []restic.Snapshot
}

// targetOf returns the target of bucket's snapshots: its configuration's,
// or, for a bucket without one (deleted, or whose configuration was
// deleted), the last target configured. It reports false when there is
// none.
func (s *Service) targetOf(ctx context.Context, bucket string) (Target, bool, error) {
	c, err := s.loadConfig(ctx, bucket)
	switch {
	case err == nil:
		return c.Target, true, nil
	case errors.Is(err, storage.ErrConfigNotFound), errors.Is(err, storage.ErrBucketNotFound):
		if st := s.stateOf(bucket); st.Target != nil {
			return *st.Target, true, nil
		}
		return Target{}, false, nil
	}
	return Target{}, false, err
}

// repositoryOf returns the existing repository of bucket's snapshots,
// without creating it. ok is false when the bucket has no target or its
// repository was not created yet.
func (s *Service) repositoryOf(ctx context.Context, bucket string) (info restic.RepositoryInfo, loc restic.Location, ok bool, err error) {
	t, ok, err := s.targetOf(ctx, bucket)
	if err != nil || !ok {
		return info, loc, false, err
	}
	info, loc, err = s.resolve(ctx, bucket, t, false)
	if errors.Is(err, iofs.ErrNotExist) {
		return info, loc, false, nil
	}
	if err != nil {
		return info, loc, false, err
	}
	return info, loc, true, nil
}

// Repository describes the repository of the bucket's snapshots (no
// secrets); nil when the bucket has no snapshot configuration or its
// repository does not exist yet.
func (s *Service) Repository(ctx context.Context, bucket string) (*restic.RepositoryInfo, error) {
	info, _, ok, err := s.repositoryOf(ctx, bucket)
	if err != nil || !ok {
		return nil, err
	}
	return &info, nil
}

// Credentials returns the location (password and environment included) and
// the description of the repository of the bucket's snapshots, for an
// operator to run restic on it; storage.ErrConfigNotFound when the bucket
// has no snapshot configuration, an error wrapping fs.ErrNotExist when the
// repository was not created yet. Callers must never log the location.
// The info's Backing names the backing the location was resolved on;
// authorize access to the location against it.
func (s *Service) Credentials(ctx context.Context, bucket string) (restic.Location, *restic.RepositoryInfo, error) {
	t, ok, err := s.targetOf(ctx, bucket)
	if err != nil {
		return restic.Location{}, nil, err
	}
	if !ok {
		return restic.Location{}, nil, fmt.Errorf("bucket %q has no snapshot configuration: %w", bucket, storage.ErrConfigNotFound)
	}
	info, loc, err := s.resolve(ctx, bucket, t, false)
	if err != nil {
		return restic.Location{}, nil, err
	}
	return loc, &info, nil
}

// Snapshots lists the bucket's whole-bucket snapshots (host SnapshotHost,
// tags TagSnapshot and the bucket's tag; partial snapshots of a run in
// progress or a failed one included, never snapshots recorded as
// unverified), newest first, each with the summary of the backup its run
// verified. The listing is cached for 30 seconds. A bucket without a
// configuration or repository has none.
func (s *Service) Snapshots(ctx context.Context, bucket string) ([]restic.Snapshot, error) {
	info, loc, ok, err := s.repositoryOf(ctx, bucket)
	if err != nil || !ok {
		return nil, err
	}
	now := s.now()
	s.mu.Lock()
	c, hit := s.cache[bucket]
	s.mu.Unlock()
	if hit && c.repository == info.Repository && now.Sub(c.at) < snapshotsTTL && !now.Before(c.at) {
		return slices.Clone(c.list), nil
	}
	list, err := s.listSnapshots(ctx, loc, bucket)
	if err != nil {
		return nil, err
	}
	unverified := s.stateOf(bucket).Unverified
	list = slices.DeleteFunc(list, func(sn restic.Snapshot) bool { return slices.Contains(unverified, sn.ID) })
	for i := range list {
		list[i] = withRunSummary(list[i])
	}
	slices.Reverse(list)
	s.mu.Lock()
	s.cache[bucket] = cachedSnapshots{at: now, repository: info.Repository, list: list}
	s.mu.Unlock()
	return slices.Clone(list), nil
}

// listSnapshots lists bucket's whole-bucket snapshots in the repository
// loc, oldest first.
func (s *Service) listSnapshots(ctx context.Context, loc restic.Location, bucket string) ([]restic.Snapshot, error) {
	list, err := s.rt.Snapshots(ctx, loc, restic.SnapshotFilter{Host: SnapshotHost, Tags: []string{TagSnapshot, restic.BucketTag(bucket)}})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(sn restic.Snapshot) bool { return !snapshotGuard(bucket)(sn) }), nil
}

// tagSummaryPrefix starts the tag of a promoted snapshot (see run.go) that
// holds the summary of the backup the run verified: the counters of
// restic.Summary joined by '.' (restic splits tags at commas).
const tagSummaryPrefix = "gos3-summary:"

// summaryTag returns the summary tag of sum.
func summaryTag(sum restic.Summary) string {
	n := []int64{sum.FilesNew, sum.FilesChanged, sum.FilesUnmodified, sum.DirsNew, sum.DirsChanged, sum.DirsUnmodified,
		sum.DataAdded, sum.TotalFilesProcessed, sum.TotalBytesProcessed}
	parts := make([]string, len(n))
	for i, v := range n {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return tagSummaryPrefix + strings.Join(parts, ".")
}

// withRunSummary returns sn with the summary of its summary tag (the run's
// verified backup) in place of restic's (its promotion's), and without the
// tag.
func withRunSummary(sn restic.Snapshot) restic.Snapshot {
	i := slices.IndexFunc(sn.Tags, func(t string) bool { return strings.HasPrefix(t, tagSummaryPrefix) })
	if i < 0 {
		return sn
	}
	parts := strings.Split(strings.TrimPrefix(sn.Tags[i], tagSummaryPrefix), ".")
	sn.Tags = slices.Delete(slices.Clone(sn.Tags), i, i+1)
	if len(parts) != 9 {
		return sn
	}
	var n [9]int64
	for j, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return sn
		}
		n[j] = v
	}
	sn.Summary = &restic.Summary{
		FilesNew: n[0], FilesChanged: n[1], FilesUnmodified: n[2], DirsNew: n[3], DirsChanged: n[4], DirsUnmodified: n[5],
		DataAdded: n[6], TotalFilesProcessed: n[7], TotalBytesProcessed: n[8], SnapshotID: sn.ID,
	}
	return sn
}

// invalidate drops the cached snapshot listing of bucket.
func (s *Service) invalidate(bucket string) {
	s.mu.Lock()
	delete(s.cache, bucket)
	s.mu.Unlock()
}

// bucketSpec is the spec of KindSnapshot and KindPrune jobs.
type bucketSpec struct {
	Bucket string `json:"bucket"`
	// Trigger says who asked for a snapshot: "manual" or "schedule".
	Trigger string `json:"trigger,omitempty"`
}

// RunNow submits a snapshot of the bucket now (a KindSnapshot job), also
// when its schedule is disabled. It fails with jobs.ErrConflict while a
// snapshot job of the bucket is active and with an error wrapping
// jobs.ErrInvalidSpec when the bucket has no snapshot configuration.
func (s *Service) RunNow(ctx context.Context, bucket, createdBy string) (jobs.Job, error) {
	return s.submitSnapshot(ctx, bucket, "manual", createdBy)
}

// submitSnapshot submits a KindSnapshot job unless one is active.
func (s *Service) submitSnapshot(ctx context.Context, bucket, trigger, createdBy string) (jobs.Job, error) {
	m, err := s.manager()
	if err != nil {
		return jobs.Job{}, err
	}
	spec, _ := json.Marshal(bucketSpec{Bucket: bucket, Trigger: trigger})
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if id := activeJob(m, KindSnapshot, bucket); id != "" {
		return jobs.Job{}, fmt.Errorf("%w: snapshot job %s of bucket %q is active", jobs.ErrConflict, id, bucket)
	}
	return m.Submit(ctx, KindSnapshot, spec, createdBy)
}

// Prune submits a prune of the repository of the bucket's snapshots (a
// KindPrune job): restic removes the data no snapshot references, holding
// its exclusive lock meanwhile. jobs.ErrConflict while a prune of the
// bucket is active.
func (s *Service) Prune(ctx context.Context, bucket, createdBy string) (jobs.Job, error) {
	m, err := s.manager()
	if err != nil {
		return jobs.Job{}, err
	}
	spec, _ := json.Marshal(bucketSpec{Bucket: bucket})
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if id := activeJob(m, KindPrune, bucket); id != "" {
		return jobs.Job{}, fmt.Errorf("%w: prune job %s of bucket %q is active", jobs.ErrConflict, id, bucket)
	}
	return m.Submit(ctx, KindPrune, spec, createdBy)
}

// Forget forgets one whole-bucket snapshot of the bucket (a full ID). A
// snapshot that is not one of the bucket's whole-bucket snapshots (a
// per-version archive snapshot, another bucket's) is refused with an error
// wrapping restic.ErrForgetRefused and jobs.ErrInvalidSpec; a missing one
// is restic.ErrSnapshotNotFound. The data stays in the repository until
// it is pruned.
func (s *Service) Forget(ctx context.Context, bucket, snapshotID string) error {
	if !restic.ValidSnapshot(snapshotID) {
		return invalidSpec("invalid snapshot ID %q (want 64 hex digits)", snapshotID)
	}
	_, loc, ok, err := s.repositoryOf(ctx, bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: bucket %q has no snapshot repository", restic.ErrSnapshotNotFound, bucket)
	}
	err = s.rt.Forget(ctx, loc, []string{snapshotID}, snapshotGuard(bucket))
	s.invalidate(bucket)
	if errors.Is(err, restic.ErrForgetRefused) {
		return fmt.Errorf("%w: %w: snapshot %s is not a whole-bucket snapshot of bucket %q", jobs.ErrInvalidSpec, err, snapshotID, bucket)
	}
	if err == nil {
		s.log.Info("resticsnap: snapshot forgotten", "bucket", bucket, "snapshot", snapshotID)
	}
	return err
}

// Restore submits the restore of one of the bucket's snapshots (a
// KindRestore job). req is {"target": {"bucket": "b", "create"?: {...}},
// "prefix"?: "", "onExisting"?: "fail"|"merge", "configs"?: "safe"|"none"}.
// Errors are those of the job's validation: jobs.ErrInvalidSpec for an
// invalid request, restic.ErrSnapshotNotFound, jobs.ErrConflict for a
// non-empty target with onExisting "fail".
func (s *Service) Restore(ctx context.Context, bucket, snapshotID string, req json.RawMessage, createdBy string) (jobs.Job, error) {
	m, err := s.manager()
	if err != nil {
		return jobs.Job{}, err
	}
	var in restoreRequest
	if err := decodeStrict(req, &in); err != nil {
		return jobs.Job{}, invalidSpec("restore request: %v", err)
	}
	spec, err := json.Marshal(restoreSpec{
		Bucket: bucket, Snapshot: snapshotID, Target: in.Target,
		Prefix: in.Prefix, OnExisting: in.OnExisting, Configs: in.Configs,
	})
	if err != nil {
		return jobs.Job{}, err
	}
	return m.Submit(ctx, KindRestore, spec, createdBy)
}

// pruneRunner runs KindPrune jobs.
type pruneRunner struct{ s *Service }

// Validate implements jobs.Runner.
func (r pruneRunner) Validate(ctx context.Context, raw json.RawMessage) (json.RawMessage, map[string]string, error) {
	var spec bucketSpec
	if err := decodeStrict(raw, &spec); err != nil || spec.Bucket == "" {
		return nil, nil, invalidSpec("prune spec: want {\"bucket\": name}")
	}
	if _, ok, err := r.s.targetOf(ctx, spec.Bucket); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, invalidSpec("bucket %q has no snapshot configuration", spec.Bucket)
	}
	norm, _ := json.Marshal(bucketSpec{Bucket: spec.Bucket})
	return norm, map[string]string{"bucket": spec.Bucket}, nil
}

// pruneRetryLock is how long a prune waits for other restic commands'
// locks.
const pruneRetryLock = 10 * time.Minute

// Run implements jobs.Runner.
func (r pruneRunner) Run(ctx context.Context, h *jobs.Handle) error {
	s := r.s
	var spec bucketSpec
	if err := h.Spec(&spec); err != nil {
		return jobs.Permanent(err)
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	h.SetPhase("pruning")
	info, loc, ok, err := s.repositoryOf(ctx, spec.Bucket)
	if err != nil {
		return err
	}
	if !ok {
		return jobs.Permanent(fmt.Errorf("bucket %q has no snapshot repository", spec.Bucket))
	}
	_ = h.SetStatus(jobStatus{Repository: info.Repository})
	if err := s.rt.Prune(ctx, loc, restic.PruneOptions{RetryLock: pruneRetryLock}); err != nil {
		return err
	}
	s.updateState(spec.Bucket, func(st *bucketState) { st.LastPruneAt = s.now() })
	h.SetPhase("done")
	return nil
}

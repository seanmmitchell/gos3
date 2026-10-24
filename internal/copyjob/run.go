package copyjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// Phases (Job.Phase and checkpoint.Phase).
const (
	phasePreparing = "preparing"
	phaseCopying   = "copying"
	phaseVerifying = "verifying"
	phaseWatching  = "watching"
	phaseDone      = "done"
)

// maxPassErrors is the number of failed keys after which a pass gives up
// (the job retries later).
const maxPassErrors = 1000

// errDeferred: a key changed during every attempt to copy it.
var errDeferred = errors.New("the key kept changing during the copy; it is copied again later")

// checkpoint is where a job's next run resumes (Job.Checkpoint).
type checkpoint struct {
	// Phase is the phase the job is in.
	Phase string `json:"phase,omitempty"`
	// After is the low-watermark source key of the pass in progress: every
	// key up to it is done.
	After string `json:"after,omitempty"`
	// Full: a mirror's full pass is in progress (After is its position).
	Full bool `json:"full,omitempty"`
	// Created: the job created the target bucket.
	Created bool `json:"created,omitempty"`
	// TargetIdentity pins a target created by this job. Existing targets are
	// pinned in the normalized spec at submission.
	TargetIdentity bucketIdentity `json:"targetIdentity,omitempty"`
	// Configs are the configuration documents the job copied to the
	// target (a mirror deletes them when the source drops them).
	Configs []string `json:"configs,omitempty"`
	// LastFullAt is when a mirror's last full pass ended.
	LastFullAt time.Time `json:"lastFullAt,omitzero"`
}

// run is one run of a job.
type run struct {
	s        *Service
	h        *jobs.Handle
	job      jobs.Job
	sp       Spec
	log      *slog.Logger
	in       *inspection
	src, dst copier.Endpoint
	pol      copier.Policy
	identity bool
	st       store
	tr       *tracker
	w        *watch // mirrors
	conc     int

	cpMu      sync.Mutex
	cp        checkpoint
	cpDirty   bool
	cpSavedAt time.Time

	// The current pass (or batch of changed keys); guarded by the lock of
	// syncItems while its workers run.
	frozen   bool     // the checkpoint stopped at a failed key
	passErrs int      // keys that failed
	failed   []string // their source keys
}

// run runs job h of kind (see Runner.Run).
func (s *Service) run(ctx context.Context, h *jobs.Handle, kind string) error {
	id := h.Job().ID
	s.pruneMemStores()
	r, err := s.newRun(h, kind)
	if err == nil {
		stop := r.report()
		err = r.execute(ctx)
		stop()
		r.flush()
		if cerr := r.st.Close(); cerr != nil {
			r.log.Warn("copy job: closing the job database", "error", cerr)
		}
	}
	err = classify(err)
	if err == nil || errors.Is(err, jobs.ErrPermanent) {
		// Finished: the job's in-memory state goes.
		s.unregister(id)
		s.dropMemStore(id)
	}
	return err
}

// newRun loads a run's job: spec, checkpoint, status and state store.
func (s *Service) newRun(h *jobs.Handle, kind string) (*run, error) {
	job := h.Job()
	r := &run{s: s, h: h, job: job, log: h.Log()}
	if s.router == nil {
		return nil, jobs.Permanent(errors.New("copyjob: no storage router"))
	}
	if err := h.Spec(&r.sp); err != nil {
		return nil, jobs.Permanent(err)
	}
	if r.sp.Kind != kind {
		return nil, jobs.Permanent(fmt.Errorf("copyjob: job %s of kind %q has a spec of kind %q", job.ID, kind, r.sp.Kind))
	}
	r.log = r.log.With("copy", describe(&r.sp))
	if _, err := h.Checkpoint(&r.cp); err != nil {
		r.log.Warn("copy job: ignoring an unreadable checkpoint; starting over", "error", err)
		r.cp = checkpoint{}
	}
	var st Status
	if len(job.Status) > 0 {
		if err := json.Unmarshal(job.Status, &st); err != nil {
			st = Status{}
		}
	}
	r.tr = newTracker(st)
	r.src, r.dst = s.endpoints(&r.sp)
	r.conc = max(min(r.sp.Options.Concurrency, s.maxConc), 1)
	if r.sp.mirror() {
		r.w = s.register(job.ID, r.sp)
	}
	var err error
	if r.st, err = s.openStore(h, job.ID); err != nil {
		return nil, err
	}
	return r, nil
}

// pruneMemStores forgets the in-memory stores of jobs that are gone.
func (s *Service) pruneMemStores() {
	m := s.mgr.Load()
	if m == nil {
		return
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.memStores))
	for id := range s.memStores {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		if j, err := m.Get(id); err != nil || j.State.Finished() {
			s.dropMemStore(id)
		}
	}
}

// execute does the run's work.
func (r *run) execute(ctx context.Context) error {
	if r.w != nil {
		if b, role := r.w.deletedBucket(); b != "" {
			return jobs.Permanent(fmt.Errorf("the mirror's %s bucket %q was deleted", role, b))
		}
	}
	if err := r.refresh(ctx); err != nil {
		return err
	}
	// A resumed job must be checked against jobs submitted while it was
	// failed or paused, including when it last stopped during preparation.
	// A first run has no checkpoint; prepare uses olderJobs to arbitrate
	// jobs submitted concurrently before either starts.
	if r.phase() != "" {
		if err := checkJobs(&r.sp, r.s.activeJobs(func(j jobs.Job) bool { return j.ID != r.job.ID })); err != nil {
			return jobs.Permanent(err)
		}
	}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	if err := r.buildPolicy(ctx); err != nil {
		return err
	}
	if r.sp.mirror() {
		return r.mirror(ctx)
	}
	return r.copy(ctx)
}

// refresh reads the buckets' state. A missing source fails the job, as
// does a missing target once the job prepared it.
func (r *run) refresh(ctx context.Context) error {
	gone := func(role, bucket string) error {
		_, err := r.s.router.GetBucket(ctx, bucket)
		if errors.Is(err, storage.ErrBucketNotFound) {
			return jobs.Permanent(fmt.Errorf("the %s bucket %q no longer exists", role, bucket))
		}
		return nil
	}
	if err := gone("source", r.sp.Source.Bucket); err != nil {
		return err
	}
	if p := r.phase(); p != "" && p != phasePreparing {
		if err := gone("target", r.sp.Target.Bucket); err != nil {
			return err
		}
	}
	sp := r.sp
	if sp.Target.Create != nil {
		c := *sp.Target.Create
		sp.Target.Create = &c
	}
	in, err := r.s.inspect(ctx, &sp, r.job.CreatedBy)
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidSpec) {
			return jobs.Permanent(err)
		}
		return err
	}
	if !in.dstExists && r.phase() != "" && r.phase() != phasePreparing {
		return jobs.Permanent(fmt.Errorf("the target bucket %q no longer exists", r.sp.Target.Bucket))
	}
	r.in = in
	if r.sp.BucketBindings.Source.empty() || !r.sp.BucketBindings.Source.matches(in.srcInfo) {
		return jobs.Permanent(fmt.Errorf("the source bucket %q was replaced; submit a new job", r.sp.Source.Bucket))
	}
	if in.dstExists {
		want := r.sp.BucketBindings.Target
		if want.empty() {
			want = r.cp.TargetIdentity
		}
		switch {
		case !want.empty() && !want.matches(in.dstInfo):
			return jobs.Permanent(fmt.Errorf("the target bucket %q was replaced; submit a new job", r.sp.Target.Bucket))
		case want.empty() && !r.created():
			return jobs.Permanent(fmt.Errorf("the target bucket %q appeared after submission; submit a new job", r.sp.Target.Bucket))
		case want.empty() && r.phase() != phasePreparing:
			return jobs.Permanent(fmt.Errorf("the target bucket %q has no saved identity; submit a new job", r.sp.Target.Bucket))
		}
	} else if !r.sp.BucketBindings.Target.empty() || !r.cp.TargetIdentity.empty() {
		return jobs.Permanent(fmt.Errorf("the target bucket %q no longer exists", r.sp.Target.Bucket))
	}
	r.identity = in.identity()
	mode := modeFingerprint
	if r.identity {
		mode = modeIdentity
	}
	r.tr.update(func(st *Status) { st.Mode = mode })
	return nil
}

// prepare runs the preparing phase (see the package documentation), once.
func (r *run) prepare(ctx context.Context) error {
	if p := r.phase(); p != "" && p != phasePreparing {
		return nil
	}
	r.h.SetPhase(phasePreparing)
	r.setCP(func(cp *checkpoint) { cp.Phase = phasePreparing })
	// Jobs submitted at once may each have passed Validate: the older one
	// wins.
	if err := checkJobs(&r.sp, r.s.olderJobs(r.job)); err != nil {
		return jobs.Permanent(err)
	}
	created := r.created()
	if !r.in.dstExists {
		err := r.s.router.CreateBucket(ctx, r.sp.Target.Bucket, r.in.create)
		switch {
		case err == nil:
			created = true
			r.setCP(func(cp *checkpoint) { cp.Created = true })
			r.saveCheckpoint(true)
			r.log.Info("copy job created its target bucket", "bucket", r.sp.Target.Bucket, "backing", r.in.create.Backing)
		case errors.Is(err, storage.ErrBucketExists):
			// Created meanwhile: it is an existing target.
		case errors.Is(err, storage.ErrInvalidBacking), errors.Is(err, storage.ErrInvalidTarget), errors.Is(err, storage.ErrInvalidName):
			return jobs.Permanent(fmt.Errorf("creating the target bucket %q: %w", r.sp.Target.Bucket, err))
		default:
			return fmt.Errorf("creating the target bucket %q: %w", r.sp.Target.Bucket, err)
		}
		if err := r.refresh(ctx); err != nil {
			return err
		}
		if !r.in.dstExists {
			return fmt.Errorf("the target bucket %q was not created", r.sp.Target.Bucket)
		}
	}
	if created && r.cp.TargetIdentity.empty() {
		r.setCP(func(cp *checkpoint) { cp.TargetIdentity = identityOf(r.in.dstInfo) })
		// Do not write any copied data until the created bucket's identity
		// is durable. An ordinary progress checkpoint may be skipped on a
		// storage error, but this binding is required for a safe retry.
		r.cpMu.Lock()
		cp := r.cp
		r.cpMu.Unlock()
		if err := r.h.SaveCheckpoint(cp); err != nil {
			return fmt.Errorf("saving the created target's identity: %w", err)
		}
		r.cpMu.Lock()
		r.cpDirty, r.cpSavedAt = false, time.Now()
		r.cpMu.Unlock()
	}
	if !created && r.sp.Options.OnExisting == OnExistingFail {
		empty, err := rangeEmpty(ctx, r.s.router, r.sp.Target.Bucket, r.sp.Target.Prefix)
		if err != nil {
			return err
		}
		if !empty {
			return jobs.Permanent(errNotEmpty(&r.sp))
		}
	}
	if err := r.syncVersioning(ctx); err != nil {
		return err
	}
	if err := r.copyConfigs(ctx); err != nil {
		return err
	}
	keys, bytes, err := measure(ctx, r)
	if err != nil {
		return fmt.Errorf("measuring the source: %w", err)
	}
	r.tr.update(func(st *Status) { st.TotalKeys, st.TotalBytes = keys, bytes })
	r.setCP(func(cp *checkpoint) { cp.Phase, cp.After = phaseCopying, "" })
	r.saveCheckpoint(true)
	return nil
}

// copyConfigs runs syncConfigs and reports its warnings in the status.
func (r *run) copyConfigs(ctx context.Context) error {
	r.cpMu.Lock()
	configs := slices.Clone(r.cp.Configs)
	r.cpMu.Unlock()
	warnings, configs, err := r.syncConfigs(ctx, configs)
	r.setCP(func(cp *checkpoint) { cp.Configs = configs })
	r.tr.update(func(st *Status) { st.Warnings = warnings })
	return err
}

// copy runs a copy job's passes.
func (r *run) copy(ctx context.Context) error {
	finalPass := r.sp.Options.finalPass()
	if r.phase() == phaseCopying {
		if err := r.pass(ctx, phaseCopying, !finalPass); err != nil {
			return err
		}
		next := phaseVerifying
		if !finalPass {
			next = phaseDone
		}
		r.setCP(func(cp *checkpoint) { cp.Phase, cp.After = next, "" })
		r.saveCheckpoint(true)
	}
	if r.phase() == phaseVerifying {
		if err := r.pass(ctx, phaseVerifying, true); err != nil {
			return err
		}
		r.setCP(func(cp *checkpoint) { cp.Phase, cp.After = phaseDone, "" })
	}
	if p := r.phase(); p != phaseDone {
		return jobs.Permanent(fmt.Errorf("copyjob: unknown phase %q", p))
	}
	r.h.SetPhase(phaseDone)
	r.tr.idle()
	return nil
}

// mirror runs a mirror job: its initial pass, then, run after run, the
// changed keys and the periodic full passes.
func (r *run) mirror(ctx context.Context) error {
	w := r.w
	if r.phase() == phaseCopying {
		if r.after() != "" && w.overflowed() {
			// Changes were lost behind the checkpoint: start over.
			r.setCP(func(cp *checkpoint) { cp.After = "" })
		}
		if r.after() == "" {
			// The pass sees every change made so far.
			w.setLive(true)
			w.takeOverflow()
			w.takeDirty()
		}
		if err := r.pass(ctx, phaseCopying, false); err != nil {
			return err
		}
		if w.isLive() {
			w.setVerified()
		}
		r.setCP(func(cp *checkpoint) {
			cp.Phase, cp.After, cp.Full, cp.LastFullAt = phaseWatching, "", false, time.Now().UTC()
		})
		r.saveCheckpoint(true)
	}
	if p := r.phase(); p != phaseWatching {
		return jobs.Permanent(fmt.Errorf("copyjob: unknown phase %q", p))
	}
	r.h.SetPhase(phaseWatching)
	if w.takeConfig() {
		if err := r.resyncConfigs(ctx); err != nil {
			w.mu.Lock()
			w.configDirty = true
			w.mu.Unlock()
			return err
		}
	}

	interval := r.fullInterval()
	overflow := w.takeOverflow()
	r.cpMu.Lock()
	inFull, lastFull := r.cp.Full, r.cp.LastFullAt
	r.cpMu.Unlock()
	if overflow || inFull || !w.isVerified() || !time.Now().Before(lastFull.Add(interval)) {
		// After an overflow, changes behind an interrupted pass's
		// checkpoint are lost: start over.
		if err := r.fullMirrorPass(ctx, inFull && !overflow); err != nil {
			if overflow {
				w.mu.Lock()
				w.overflow = true
				w.mu.Unlock()
			}
			return err
		}
		r.h.SetPhase(phaseWatching)
	}

	retry, err := r.syncChanged(ctx)
	if err != nil {
		return err
	}

	r.cpMu.Lock()
	next := r.cp.LastFullAt.Add(interval)
	r.cpMu.Unlock()
	nextPass := next
	if retry {
		next = minTime(next, time.Now().Add(retryKeysDelay))
	}
	r.h.SetNextRun(next)
	n, oldest := w.pending()
	r.tr.update(func(st *Status) {
		st.Pending, st.LagSeconds, st.NextPassAt = int64(n), lag(oldest), nextPass.UTC()
	})
	r.tr.idle()
	return jobs.ErrIdle
}

// fullInterval is the time between a mirror's full passes: verifyInterval,
// or pollInterval for a native source when shorter.
func (r *run) fullInterval() time.Duration {
	d := r.sp.Options.verifyInterval()
	if r.in.nativeSource() {
		d = min(d, r.sp.Options.pollInterval())
	}
	return d
}

// fullMirrorPass compares every key again. It resumes an interrupted pass
// (resume) only when this process wrote its checkpoint, having watched the
// changes all along; otherwise it starts over.
func (r *run) fullMirrorPass(ctx context.Context, resume bool) error {
	w := r.w
	if !resume || !w.isLive() || r.after() == "" {
		r.setCP(func(cp *checkpoint) { cp.After = "" })
		w.setLive(true)
		w.takeDirty() // the pass sees these changes
	}
	r.setCP(func(cp *checkpoint) { cp.Full = true })
	if err := r.pass(ctx, phaseVerifying, false); err != nil {
		return err
	}
	w.setVerified()
	r.setCP(func(cp *checkpoint) { cp.Full, cp.After, cp.LastFullAt = false, "", time.Now().UTC() })
	r.saveCheckpoint(true)
	return nil
}

// resyncConfigs follows a change of the source's configuration or
// versioning.
func (r *run) resyncConfigs(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
		return err
	}
	if err := r.syncVersioning(ctx); err != nil {
		return err
	}
	if err := r.copyConfigs(ctx); err != nil {
		return err
	}
	r.saveCheckpoint(true)
	// The target's encryption settings may have changed with them.
	return r.buildPolicy(ctx)
}

// syncChanged copies a mirror's changed keys. Keys that fail are marked
// changed again; retry reports that some did.
func (r *run) syncChanged(ctx context.Context) (retry bool, err error) {
	w := r.w
	keys, since := w.takeDirty()
	if len(keys) == 0 {
		return false, nil
	}
	var oldest time.Time
	for _, at := range since {
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	r.tr.update(func(st *Status) { st.Pending, st.LagSeconds = int64(len(keys)), lag(oldest) })
	_ = r.h.SetStatus(r.tr.snapshot())
	items := make([]item, len(keys))
	for i, k := range keys {
		items[i] = item{pair: copier.KeyPair{SrcKey: k}, lookup: true}
	}
	ok := make([]bool, len(keys))
	r.passErrs, r.failed = 0, nil
	err = r.syncItems(ctx, items, func(i int, failed bool) { ok[i] = !failed })
	var again []string
	for i, k := range keys {
		if !ok[i] {
			again = append(again, k)
		}
	}
	w.requeue(again, since)
	return len(again) > 0, err
}

// pass compares every key of the source and the target (copier.ZipWalk),
// resuming after the checkpoint's key. In a final pass a key that fails
// stops the checkpoint there and fails the pass at its end (the job
// retries from that key). In other passes the checkpoint moves on: a
// copy's final pass covers the failed keys again, and a mirror's are
// marked changed (also when the pass is interrupted).
func (r *run) pass(ctx context.Context, phase string, final bool) error {
	r.h.SetPhase(phase)
	after := r.after()
	if after == "" {
		r.tr.startPass()
	}
	r.frozen, r.passErrs, r.failed = false, 0, nil
	err := copier.ZipWalk(ctx, r.src, r.dst, after, r.sp.all(), func(batch []copier.KeyPair) error {
		return r.batch(ctx, batch, final)
	})
	if r.w != nil {
		// The checkpoint moved past the keys that failed: whatever ends
		// the pass, a mirror copies them as changed keys.
		r.w.requeue(r.failed, nil)
	}
	r.saveCheckpoint(true)
	if err != nil {
		return err
	}
	if final && r.passErrs > 0 {
		return fmt.Errorf("%d keys could not be copied (see the job's errors); retrying from %q", r.passErrs, r.after())
	}
	r.tr.update(func(st *Status) {
		st.Passes++
		st.LastPassAt = time.Now().UTC()
	})
	return nil
}

// batch synchronises one batch of a pass and advances the checkpoint over
// the longest finished prefix of it.
func (r *run) batch(ctx context.Context, batch []copier.KeyPair, final bool) error {
	items := make([]item, len(batch))
	for i, p := range batch {
		items[i] = item{pair: p}
	}
	done := make([]bool, len(batch))
	bad := make([]bool, len(batch))
	next := 0
	return r.syncItems(ctx, items, func(i int, failed bool) {
		done[i], bad[i] = true, failed
		for !r.frozen && next < len(batch) && done[next] {
			if bad[next] && final {
				r.frozen = true
				break
			}
			next++
			r.advance(batch[next-1].SrcKey)
		}
	})
}

// item is a key to synchronise: a pair from a walk, or (lookup) a source
// key whose pair is listed first.
type item struct {
	pair   copier.KeyPair
	lookup bool
}

// syncItems synchronises items, up to r.conc at once (and within the
// service's limit). done is called, serialized, for each item that
// finished: failed when its key failed (the error is recorded). An error
// that concerns more than one key (an unavailable backing, a policy the
// target cannot satisfy, a canceled context) stops the work and is
// returned.
func (r *run) syncItems(ctx context.Context, items []item, done func(i int, failed bool)) error {
	if len(items) == 0 {
		return nil
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		fatal error
		next  atomic.Int64
		wg    sync.WaitGroup
	)
	for range min(r.conc, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(items) || ctx.Err() != nil {
					return
				}
				if err := r.s.acquire(ctx); err != nil {
					return
				}
				key, err := r.syncItem(ctx, items[i])
				r.s.release()
				mu.Lock()
				switch {
				case err == nil:
					done(i, false)
				case fatal != nil:
					// Stopping anyway.
				case ctx.Err() != nil || fatalError(err):
					fatal = err
					cancel()
				default:
					r.passErrs++
					r.failed = append(r.failed, key)
					r.tr.keyError(key, err, time.Now())
					r.log.Warn("copy job: a key could not be copied", "key", key, "error", err)
					done(i, true)
					if r.passErrs >= maxPassErrors {
						fatal = fmt.Errorf("%d keys could not be copied; the last, %q: %w", r.passErrs, key, err)
						cancel()
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := parent.Err(); err != nil {
		return err
	}
	return fatal
}

// syncItem synchronises one key and returns its source key.
func (r *run) syncItem(ctx context.Context, it item) (string, error) {
	p := it.pair
	all := r.sp.all()
	if it.lookup {
		lp, err := copier.LookupPair(ctx, r.src, r.dst, p.SrcKey, all)
		if err != nil {
			return p.SrcKey, err
		}
		p = lp
	}
	var (
		res copier.Result
		err error
	)
	if r.identity {
		res, err = copier.SyncKey(ctx, r.src, r.dst, p, r.pol)
	} else {
		res, err = copier.SyncKeyFingerprint(ctx, r.src, r.dst, p, r.st, r.pol)
	}
	r.tr.record(p, all, res)
	switch {
	case err != nil:
		return p.SrcKey, err
	case res.Deferred:
		return p.SrcKey, errDeferred
	}
	return p.SrcKey, r.markOwned(p, res)
}

// markOwned records whether the job owns the target key of p after it was
// synchronised: a key the source has is the job's (it holds the source's
// copy); one the source no longer has stops being the job's once it is
// gone from the target.
func (r *run) markOwned(p copier.KeyPair, res copier.Result) error {
	if p.DstKey == "" || len(p.DstKey) > copier.MaxKeyLen {
		return nil
	}
	if hasSource(p, r.sp.all()) {
		return r.st.SetOwned(p.DstKey, true)
	}
	if len(res.Conflicts) == 0 && (res.VersionsDeleted > 0 || len(p.Dst) == 0) {
		return r.st.SetOwned(p.DstKey, false)
	}
	return nil
}

// Checkpoint access; the checkpoint is changed by the run's goroutine and,
// serialized, by syncItems's workers.

func (r *run) setCP(f func(*checkpoint)) {
	r.cpMu.Lock()
	f(&r.cp)
	r.cpDirty = true
	r.cpMu.Unlock()
}

func (r *run) phase() string {
	r.cpMu.Lock()
	defer r.cpMu.Unlock()
	return r.cp.Phase
}

func (r *run) after() string {
	r.cpMu.Lock()
	defer r.cpMu.Unlock()
	return r.cp.After
}

func (r *run) created() bool {
	r.cpMu.Lock()
	defer r.cpMu.Unlock()
	return r.cp.Created
}

// advance moves the pass's low watermark to key, saving the checkpoint at
// most every checkpointEvery.
func (r *run) advance(key string) {
	r.setCP(func(cp *checkpoint) { cp.After = key })
	r.saveCheckpoint(false)
}

// saveCheckpoint hands the checkpoint to the Manager if it changed (unless
// force, at most every checkpointEvery).
func (r *run) saveCheckpoint(force bool) {
	r.cpMu.Lock()
	defer r.cpMu.Unlock()
	if !r.cpDirty || !force && time.Since(r.cpSavedAt) < r.s.checkpointEvery {
		return
	}
	// The state the checkpoint relies on (owned keys, fingerprint
	// records) is written first.
	if err := r.st.Flush(); err != nil {
		r.log.Warn("copy job: checkpoint not saved", "error", err)
		return
	}
	if err := r.h.SaveCheckpoint(r.cp); err != nil {
		r.log.Debug("copy job: checkpoint not saved", "error", err)
		return
	}
	r.cpDirty, r.cpSavedAt = false, time.Now()
}

// report refreshes the job's status every statusEvery until stop is
// called.
func (r *run) report() (stop func()) {
	r.tr.tick(time.Now())
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(r.s.statusEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				r.tr.tick(now)
				_ = r.h.SetStatus(r.tr.snapshot())
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// flush hands the final status and checkpoint of the run to the Manager.
func (r *run) flush() {
	r.tr.tick(time.Now())
	_ = r.h.SetStatus(r.tr.snapshot())
	r.saveCheckpoint(true)
}

// transient reports errors that a later run may not meet: unavailable
// backings, buckets or encryption, a router shutting down.
func transient(err error) bool {
	return errors.Is(err, storage.ErrBackingUnavailable) || errors.Is(err, storage.ErrEncryptionUnavailable) ||
		errors.Is(err, storage.ErrResourceLimit) ||
		errors.Is(err, multi.ErrClosed)
}

// impossible reports errors that running the job again unchanged would
// meet again: policies the target cannot satisfy, a bucket that is gone.
// copier.ErrTooManyVersions is not among them: it concerns one key, which
// fails alone (anyone allowed to write a key could otherwise stop the
// whole job).
func impossible(err error) bool {
	for _, e := range []error{
		copier.ErrPlaintext, copier.ErrEncryptionUnsupported, copier.ErrRetention, copier.ErrNoImport,
		copier.ErrNotVersioned, storage.ErrBucketNotFound, jobs.ErrInvalidSpec, jobs.ErrConflict,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// fatalError reports errors of one key that stop the whole pass.
func fatalError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || transient(err) || impossible(err)
}

// classify returns a run's result for the jobs Manager: impossible errors
// become permanent, the others are retried.
func classify(err error) error {
	switch {
	case err == nil, errors.Is(err, jobs.ErrPermanent), errors.Is(err, jobs.ErrIdle):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), transient(err):
		return err
	case impossible(err):
		return jobs.Permanent(err)
	}
	return err
}

// lag returns the seconds since t (0 for the zero time).
func lag(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return int64(max(time.Since(t), 0) / time.Second)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

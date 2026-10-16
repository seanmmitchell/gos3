package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// DefaultInterval is the time between two lifecycle passes when Options or
// Reconfigure do not specify a positive interval.
const DefaultInterval = time.Hour

// walkBatch is the number of object versions collected from a backend walk
// before the walk is suspended and the batch processed. The worker never
// mutates the backend from inside a walk callback, so that backends holding
// locks or transactions during a walk are safe. A batch of versions may end
// in the middle of a key: the worker keeps only a bounded summary of the
// key's versions across batches (see keyState), so its memory does not
// grow with the number of versions of a key.
const walkBatch = 1000

// Options configure a Worker.
type Options struct {
	// Interval is the time between the end of one scheduled pass and the
	// start of the next (default DefaultInterval).
	Interval time.Duration
	// Logger receives one info record per action taken and warnings for
	// failures (default: discard).
	Logger *slog.Logger
	// Now returns the current time used to evaluate rules and object lock
	// retention (default time.Now). Tests inject a fake clock.
	Now func() time.Time
}

// Stats summarizes one lifecycle pass.
type Stats struct {
	// Buckets is the number of buckets with a lifecycle configuration that
	// were processed.
	Buckets int `json:"buckets"`
	// Expired counts current versions expired by Expiration Days/Date
	// (removed in unversioned buckets, hidden behind a new delete marker
	// otherwise).
	Expired int `json:"expired"`
	// NoncurrentExpired counts noncurrent versions (including noncurrent
	// delete markers) permanently removed by NoncurrentVersionExpiration.
	NoncurrentExpired int `json:"noncurrentExpired"`
	// DeleteMarkersRemoved counts expired object delete markers removed.
	DeleteMarkersRemoved int `json:"deleteMarkersRemoved"`
	// UploadsAborted counts multipart uploads aborted by
	// AbortIncompleteMultipartUpload.
	UploadsAborted int `json:"uploadsAborted"`
	// Errors counts failed actions and buckets that could not be processed
	// (unreadable or invalid configuration, failed walks).
	Errors int `json:"errors"`
}

// Status describes a Worker's configuration and its most recent pass.
type Status struct {
	Enabled  bool          `json:"enabled"`
	Interval time.Duration `json:"interval"`
	// Running is true while a pass (scheduled or RunOnce) is in progress.
	Running bool `json:"running"`
	// LastStart and LastEnd bound the most recent completed pass (zero if
	// none has completed yet).
	LastStart time.Time `json:"lastStart"`
	LastEnd   time.Time `json:"lastEnd"`
	LastStats Stats     `json:"lastStats"`
	// LastError is the error returned by the most recent pass, if any.
	LastError string `json:"lastError,omitempty"`
}

// Worker periodically applies the lifecycle configurations of all buckets of
// a storage backend. All methods are safe for concurrent use. Passes never
// overlap: RunOnce waits for a pass in progress to finish.
type Worker struct {
	backend storage.Backend
	log     *slog.Logger
	now     func() time.Time

	runMu sync.Mutex // serializes passes

	mu       sync.Mutex // guards the fields below
	interval time.Duration
	enabled  bool
	cancel   context.CancelFunc // non-nil while the scheduler runs
	done     chan struct{}      // closed when the scheduler goroutine exits
	wake     chan struct{}      // capacity 1; signals Reconfigure to the scheduler
	status   Status
}

// NewWorker returns a stopped, enabled Worker for backend. Call Start to
// run it periodically or RunOnce to run a single pass.
func NewWorker(backend storage.Backend, opts Options) *Worker {
	w := &Worker{
		backend:  backend,
		log:      opts.Logger,
		now:      opts.Now,
		interval: opts.Interval,
		enabled:  true,
		wake:     make(chan struct{}, 1),
	}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	return w
}

// Start launches the scheduler goroutine, which runs a first pass right
// away (if enabled) and then one pass every interval, measured from the end
// of the previous pass. The scheduler stops when ctx is done or Stop is
// called. Calling Start while the scheduler is running is a no-op; a stopped
// Worker may be started again.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		select {
		case <-w.done:
			// The previous scheduler exited because its context ended.
			w.cancel()
		default:
			return
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.cancel, w.done = cancel, done
	go w.loop(ctx, done)
}

// Stop stops the scheduler, cancelling a scheduled pass in progress, and
// waits for it to exit. It is idempotent. A pass started with RunOnce is not
// affected (it is governed by its own context).
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Reconfigure changes the pass interval (interval <= 0 selects
// DefaultInterval) and enables or disables scheduled passes. It takes effect
// immediately: when the new interval has already elapsed since the last
// pass, or passes are re-enabled after a pause that long, a pass starts
// right away. A disabled Worker still runs explicit RunOnce calls.
func (w *Worker) Reconfigure(interval time.Duration, enabled bool) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	w.mu.Lock()
	w.interval, w.enabled = interval, enabled
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Status returns the Worker's configuration and the result of its most
// recent pass.
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	s.Enabled, s.Interval = w.enabled, w.interval
	return s
}

func (w *Worker) settings() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.interval, w.enabled
}

// loop is the scheduler goroutine.
func (w *Worker) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	var last time.Time // end of the last scheduled pass (wall clock)
	for {
		interval, enabled := w.settings()
		var (
			timer *time.Timer
			fire  <-chan time.Time
		)
		if enabled {
			wait := time.Until(last.Add(interval))
			if last.IsZero() || wait <= 0 {
				if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
					w.log.Warn("lifecycle: pass failed", "error", err)
				}
				last = time.Now()
				if ctx.Err() != nil {
					return
				}
				continue
			}
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// RunOnce runs one lifecycle pass over every bucket that has a lifecycle
// configuration and returns what it did. Failures of individual actions or
// buckets are logged and counted in Stats.Errors; the returned error is
// non-nil only if the bucket list could not be read or ctx ended (the Stats
// then cover the work done so far).
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	w.runMu.Lock()
	defer w.runMu.Unlock()

	start := w.now()
	w.mu.Lock()
	w.status.Running = true
	w.mu.Unlock()

	st, err := w.run(ctx, start.UTC())

	w.mu.Lock()
	w.status.Running = false
	w.status.LastStart, w.status.LastEnd = start, w.now()
	w.status.LastStats = st
	w.status.LastError = ""
	if err != nil {
		w.status.LastError = err.Error()
	}
	w.mu.Unlock()

	if st != (Stats{}) {
		w.log.Info("lifecycle: pass complete",
			"buckets", st.Buckets, "expired", st.Expired, "noncurrentExpired", st.NoncurrentExpired,
			"deleteMarkersRemoved", st.DeleteMarkersRemoved, "uploadsAborted", st.UploadsAborted,
			"errors", st.Errors)
	}
	return st, err
}

func (w *Worker) run(ctx context.Context, now time.Time) (Stats, error) {
	var st Stats
	if w.backend == nil {
		return st, errors.New("lifecycle: no storage backend")
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	buckets, err := w.backend.ListBuckets(ctx)
	if err != nil {
		return st, fmt.Errorf("lifecycle: list buckets: %w", err)
	}
	for _, b := range buckets {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		cfg, err := w.loadConfig(ctx, b.Name)
		switch {
		case err == nil:
		case errors.Is(err, storage.ErrConfigNotFound), errors.Is(err, storage.ErrBucketNotFound):
			continue
		case ctx.Err() != nil:
			return st, ctx.Err()
		default:
			st.Errors++
			w.log.Warn("lifecycle: cannot load bucket lifecycle configuration", "bucket", b.Name, "error", err)
			continue
		}
		st.Buckets++
		p := newPass(w, b, cfg, now, &st)
		if err := p.run(ctx); err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			if errors.Is(err, storage.ErrBucketNotFound) {
				continue // deleted concurrently
			}
			st.Errors++
			w.log.Warn("lifecycle: bucket pass failed", "bucket", b.Name, "error", err)
		}
	}
	return st, nil
}

// loadConfig reads and parses the stored lifecycle configuration of bucket.
func (w *Worker) loadConfig(ctx context.Context, bucket string) (*Configuration, error) {
	data, err := w.backend.GetBucketConfig(ctx, bucket, storage.ConfigLifecycle)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("invalid stored configuration: %w", err)
	}
	return cfg, nil
}

// compiledRule is an enabled rule with its precomputed filter.
type compiledRule struct {
	*Rule
	pred predicate
}

// matchesVersion reports whether the rule's filter selects o. Delete markers
// have neither tags nor a size: they are selected by the key prefix alone,
// and never by rules with tag conditions.
func (r *compiledRule) matchesVersion(o *storage.ObjectInfo) bool {
	if o.DeleteMarker {
		return len(r.pred.tags) == 0 && strings.HasPrefix(o.Key, r.pred.prefix)
	}
	return r.pred.matches(o.Key, o.Tags, o.Size)
}

// pass applies one configuration to one bucket.
type pass struct {
	w      *Worker
	bucket storage.BucketInfo
	now    time.Time
	st     *Stats

	// cd and nd delete conditionally (nil when the backend cannot): see
	// expireCurrent, expireNoncurrent and removeMarker.
	cd           storage.ConditionalDeleter
	nd           storage.NewerVersionsDeleter
	warnedNoCond bool // a missing conditional delete was logged

	rules      []*compiledRule // enabled rules, in document order
	objects    bool            // some rule acts on object versions
	uploads    bool            // some rule aborts uploads
	walkPrefix string          // common key prefix of all object rules
	// uploadPrefix is the common key prefix of all upload rules.
	uploadPrefix string
}

func newPass(w *Worker, b storage.BucketInfo, cfg *Configuration, now time.Time, st *Stats) *pass {
	p := &pass{w: w, bucket: b, now: now, st: st}
	p.cd, _ = w.backend.(storage.ConditionalDeleter)
	p.nd, _ = w.backend.(storage.NewerVersionsDeleter)
	first := true
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if !r.Enabled() {
			continue
		}
		cr := &compiledRule{Rule: r, pred: r.predicate()}
		p.rules = append(p.rules, cr)
		if r.AbortIncompleteMultipartUpload != nil {
			if !p.uploads {
				p.uploadPrefix = cr.pred.prefix
			} else {
				p.uploadPrefix = commonPrefix(p.uploadPrefix, cr.pred.prefix)
			}
			p.uploads = true
		}
		if r.Expiration != nil || r.NoncurrentVersionExpiration != nil {
			p.objects = true
			if first {
				p.walkPrefix, first = cr.pred.prefix, false
			} else {
				p.walkPrefix = commonPrefix(p.walkPrefix, cr.pred.prefix)
			}
		}
	}
	return p
}

// commonPrefix returns the longest common byte prefix of a and b.
func commonPrefix(a, b string) string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func (p *pass) run(ctx context.Context) error {
	var errs []error
	if p.objects {
		var err error
		if p.bucket.Versioning == storage.VersioningUnversioned {
			err = p.walkUnversioned(ctx)
		} else {
			err = p.walkVersioned(ctx)
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, storage.ErrBucketNotFound) {
				return err
			}
			errs = append(errs, err)
		}
	}
	if p.uploads {
		if err := p.abortUploads(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Rule evaluation

// currentRule returns the first rule whose Expiration (Days or Date) has
// expired o, the current version of its key, at p.now.
func (p *pass) currentRule(o *storage.ObjectInfo) *compiledRule {
	if o.DeleteMarker {
		return nil
	}
	for _, r := range p.rules {
		e := r.Expiration
		if e == nil || !r.matchesVersion(o) {
			continue
		}
		switch {
		case e.Days != nil:
			if !p.now.Before(Expiry(o.LastModified, *e.Days)) {
				return r
			}
		case e.Date != nil:
			if !p.now.Before(e.Date.Time) {
				return r
			}
		}
	}
	return nil
}

// markerRule returns the first rule that removes dm, an expired object
// delete marker (a delete marker that is the only version of its key):
// either ExpiredObjectDeleteMarker is true, or a Days-based Expiration has
// elapsed since the marker was created (S3 removes expired object delete
// markers as part of Days-based expiration).
func (p *pass) markerRule(dm *storage.ObjectInfo) *compiledRule {
	for _, r := range p.rules {
		e := r.Expiration
		if e == nil || !r.matchesVersion(dm) {
			continue
		}
		if e.ExpiredObjectDeleteMarker != nil && *e.ExpiredObjectDeleteMarker {
			return r
		}
		if e.Days != nil && !p.now.Before(Expiry(dm.LastModified, *e.Days)) {
			return r
		}
	}
	return nil
}

// noncurrentRule returns the first rule whose NoncurrentVersionExpiration
// applies to o, a noncurrent version that became noncurrent at since (the
// LastModified of the next newer version) and has newer noncurrent versions
// that are newer than it.
func (p *pass) noncurrentRule(o *storage.ObjectInfo, since time.Time, newer int) *compiledRule {
	for _, r := range p.rules {
		if r.matchesVersion(o) && p.noncurrentDue(r, since, newer) {
			return r
		}
	}
	return nil
}

// noncurrentDue reports whether r's NoncurrentVersionExpiration expires a
// version that became noncurrent at since and has newer noncurrent
// versions that are newer than it (the rule's filter is not consulted).
func (p *pass) noncurrentDue(r *compiledRule, since time.Time, newer int) bool {
	n := r.NoncurrentVersionExpiration
	if n == nil || n.NoncurrentDays == nil {
		return false
	}
	if n.NewerNoncurrentVersions != nil && newer < *n.NewerNoncurrentVersions {
		return false
	}
	return !p.now.Before(Expiry(since, *n.NoncurrentDays))
}

// abortRule returns the first rule that aborts u at p.now. Only the key
// prefix of a rule's filter applies to uploads: size bounds are ignored
// because an upload has no final size yet, and rules with tag conditions
// never apply (Validate rejects AbortIncompleteMultipartUpload with tags).
func (p *pass) abortRule(u *storage.UploadInfo) *compiledRule {
	for _, r := range p.rules {
		a := r.AbortIncompleteMultipartUpload
		if a == nil || a.DaysAfterInitiation == nil || len(r.pred.tags) > 0 ||
			!strings.HasPrefix(u.Key, r.pred.prefix) {
			continue
		}
		if !p.now.Before(Expiry(u.Initiated, *a.DaysAfterInitiation)) {
			return r
		}
	}
	return nil
}

// Protected reports whether object lock forbids permanently deleting the
// version o at now: it is under legal hold or its retention period has not
// ended. Delete markers are never protected.
func Protected(o storage.ObjectInfo, now time.Time) bool {
	return !o.DeleteMarker && (o.LegalHold || o.RetainUntil.After(now))
}

// ---------------------------------------------------------------------------
// Walks

// walkUnversioned expires current objects of an unversioned bucket.
func (p *pass) walkUnversioned(ctx context.Context) error {
	b := p.w.backend
	after := ""
	for {
		batch := make([]storage.ObjectInfo, 0, 64)
		err := b.WalkObjects(ctx, p.bucket.Name, p.walkPrefix, after, func(o storage.ObjectInfo) error {
			batch = append(batch, o)
			if len(batch) >= walkBatch {
				return storage.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk objects: %w", err)
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.expireCurrent(ctx, &batch[i], nil)
		}
		if len(batch) < walkBatch {
			return nil
		}
		after = batch[len(batch)-1].Key
	}
}

// walkVersioned processes every key of a versioned or versioning-suspended
// bucket, one version at a time (newest first within a key), in batches of
// walkBatch versions. The last version a walk delivers is only processed
// with the next batch: it is the next walk's resume marker, and deleting it
// first would make resuming after it backend-dependent.
func (p *pass) walkVersioned(ctx context.Context) error {
	b := p.w.backend
	var (
		keyMarker, vidMarker string
		carry                *storage.ObjectInfo // the previous walk's last version
		ks                   keyState
	)
	for {
		batch := make([]storage.ObjectInfo, 0, 64)
		if carry != nil {
			batch = append(batch, *carry)
		}
		full := false
		err := b.WalkVersions(ctx, p.bucket.Name, p.walkPrefix, keyMarker, vidMarker, func(o storage.ObjectInfo) error {
			batch = append(batch, o)
			if len(batch) > walkBatch {
				full = true
				return storage.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk versions: %w", err)
		}
		n := len(batch)
		if full {
			n--
			last := batch[n]
			carry = &last
			keyMarker, vidMarker = last.Key, last.VersionID
		}
		for i := range batch[:n] {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.version(ctx, &ks, &batch[i])
		}
		if !full {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.endKey(ctx, &ks)
			return nil
		}
	}
}

// keyState is what processing the versions of one key, newest first, keeps
// from one version to the next: a bounded summary, never the versions
// themselves.
type keyState struct {
	key string
	// ok is false before the first key and once the key is left to the
	// next pass (its walk did not start with its latest version, or
	// reported a second latest version) or finished (endKey).
	ok  bool
	cur storage.ObjectInfo // the key's latest version, first in the walk
	// seen counts the versions walked so far (cur included), remaining
	// those of them not deleted.
	seen, remaining int
	// prevModified is the LastModified of the last version walked: the
	// time the next (older) one became noncurrent.
	prevModified time.Time
	// null is the key's null version when it is protected by object lock
	// (versioning-suspended buckets only, see expireCurrent).
	null *storage.ObjectInfo
}

// version applies the rules to o, the next version of the walk. The first
// version of a key finishes the previous key (endKey) and starts the new
// one; the key's later versions are noncurrent.
func (p *pass) version(ctx context.Context, ks *keyState, o *storage.ObjectInfo) {
	if ks.seen == 0 || o.Key != ks.key {
		p.endKey(ctx, ks)
		// Walks start at key boundaries, so the first version is the
		// latest; anything else means the key changed under us. It will
		// be handled by the next pass.
		*ks = keyState{key: o.Key, ok: o.IsLatest, cur: *o, seen: 1, remaining: 1, prevModified: o.LastModified}
		p.noteNull(ks, o)
		return
	}
	if !ks.ok {
		return
	}
	if o.IsLatest {
		// A second "latest" entry: newer versions were removed while the
		// walk read the key (e.g. its delete marker was deleted to restore
		// it), so the rest of the walk is not all noncurrent. Leave the key
		// to the next pass. The deletes already made were each checked
		// against the key's versions at the time (expireNoncurrent).
		ks.ok = false
		return
	}
	// A version becomes noncurrent when the next newer version is
	// created; seen-1 noncurrent versions are newer than it.
	since, newer := ks.prevModified, ks.seen-1
	ks.seen++
	ks.remaining++
	ks.prevModified = o.LastModified
	p.noteNull(ks, o)
	r := p.noncurrentRule(o, since, newer)
	if r == nil {
		return
	}
	switch p.expireNoncurrent(ctx, o, r) {
	case versionDeleted:
		p.st.NoncurrentExpired++
		ks.remaining--
	case versionGone:
		ks.remaining--
	}
}

// noteNull records o in ks when it is the null version of a key in a
// versioning-suspended bucket and protected by object lock.
func (p *pass) noteNull(ks *keyState, o *storage.ObjectInfo) {
	if p.bucket.Versioning == storage.VersioningSuspended && o.VersionID == storage.NullVersionID && Protected(*o, p.now) {
		null := *o
		ks.null = &null
	}
}

// endKey applies the rules to the latest version of the key of ks once its
// versions have been walked: Expiration of a current object, or the
// removal of an expired object delete marker. It acts at most once per key.
func (p *pass) endKey(ctx context.Context, ks *keyState) {
	if !ks.ok {
		return
	}
	ks.ok = false
	cur := &ks.cur
	if !cur.DeleteMarker {
		p.expireCurrent(ctx, cur, ks.null)
		return
	}
	if ks.remaining != 1 {
		return
	}
	r := p.markerRule(cur)
	if r == nil || !p.onlyVersion(ctx, cur) {
		return
	}
	if p.removeMarker(ctx, cur, r) == versionDeleted {
		p.st.DeleteMarkersRemoved++
	}
}

// errSkip is returned by the conditional delete checks of expireCurrent,
// expireNoncurrent and removeMarker when the version changed since the
// walk, is no longer expired or is protected; the delete is then not
// performed.
var errSkip = errors.New("lifecycle: version changed or protected")

// noConditionalDelete logs, once per pass, that a permanent delete was
// skipped because the backend cannot delete conditionally (it implements
// neither storage.ConditionalDeleter nor, for noncurrent versions,
// storage.NewerVersionsDeleter): an unconditional delete could remove data
// written or locked after the worker's last check.
func (p *pass) noConditionalDelete() {
	if !p.warnedNoCond {
		p.warnedNoCond = true
		p.w.log.Warn("lifecycle: storage backend cannot delete conditionally; skipping permanent deletes",
			"bucket", p.bucket.Name)
	}
}

// expireCurrent applies Expiration Days/Date to o, the current version of
// its key. null is the key's null version when the walk found it protected
// by object lock in a versioning-suspended bucket (nil otherwise).
//
// The delete (no version ID) is conditional, atomically in the backend, on
// the key's latest version still being o, unchanged (so a version written
// meanwhile is never removed or hidden), still expired by a rule (its tags
// may have changed) and, in unversioned and suspended buckets where the
// delete removes it permanently, not protected by object lock.
func (p *pass) expireCurrent(ctx context.Context, o *storage.ObjectInfo, null *storage.ObjectInfo) {
	if p.currentRule(o) == nil {
		return
	}
	permanent := p.bucket.Versioning != storage.VersioningEnabled
	if permanent && p.cd == nil {
		p.noConditionalDelete()
		return
	}
	// In a suspended bucket the delete replaces the null version, which
	// may be a noncurrent version the check below does not see. Object
	// lock cannot be enabled on suspended buckets, so this snapshot test
	// is only defensive.
	if null != nil {
		p.protected(null, p.currentRule(o))
		return
	}
	var (
		r         *compiledRule // the rule expiring the fresh version
		protected *storage.ObjectInfo
	)
	check := func(fresh storage.ObjectInfo) error {
		r, protected = nil, nil
		if fresh.DeleteMarker || !fresh.IsLatest || fresh.VersionID != o.VersionID ||
			!fresh.LastModified.Equal(o.LastModified) || fresh.ETag != o.ETag {
			return errSkip
		}
		if r = p.currentRule(&fresh); r == nil {
			return errSkip
		}
		if permanent && Protected(fresh, p.now) {
			protected = &fresh
			return errSkip
		}
		return nil
	}
	b := p.w.backend
	var (
		res storage.DeleteResult
		err error
	)
	if p.cd != nil {
		res, err = p.cd.DeleteObjectIf(ctx, p.bucket.Name, o.Key, "", check)
	} else {
		// Versioning enabled: the delete only adds a delete marker, so a
		// snapshot check is enough (a version written in between stays
		// readable as a noncurrent version).
		var fresh storage.ObjectInfo
		if fresh, err = b.GetObjectInfo(ctx, p.bucket.Name, o.Key, ""); err == nil {
			if err = check(fresh); err == nil {
				res, err = b.DeleteObject(ctx, p.bucket.Name, o.Key, "")
			}
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, errSkip):
		if protected != nil {
			p.protected(protected, r)
		}
		return
	case errors.Is(err, storage.ErrObjectNotFound), errors.Is(err, storage.ErrBucketNotFound):
		return
	default:
		p.fail("expire object", o, err)
		return
	}
	p.st.Expired++
	attrs := []any{"bucket", p.bucket.Name, "key", o.Key, "versionId", o.VersionID, "rule", r.ID}
	if res.DeleteMarker {
		attrs = append(attrs, "deleteMarkerVersionId", res.VersionID)
	}
	p.w.log.Info("lifecycle: object expired", attrs...)
}

// deleteOutcome is the result of expireNoncurrent and removeMarker.
type deleteOutcome int

const (
	versionKept    deleteOutcome = iota // not deleted (changed, protected or failed)
	versionDeleted                      // deleted by this pass
	versionGone                         // already removed by someone else
)

// unchanged reports whether fresh, the version v as the backend stores it
// at the delete, is still what the walk saw and is (latest) or is not
// (!latest) the key's latest version: a noncurrent version that has become
// the latest (e.g. a null version replaced by a new current one) is never
// deleted.
func unchanged(fresh, v *storage.ObjectInfo, latest bool) bool {
	return fresh.IsLatest == latest && fresh.DeleteMarker == v.DeleteMarker &&
		fresh.LastModified.Equal(v.LastModified) && fresh.ETag == v.ETag
}

// expireNoncurrent permanently removes v, a noncurrent version that rule r
// selected from the walk. The delete is conditional, atomically in the
// backend (storage.NewerVersionsDeleter), on v being unchanged since the
// walk and noncurrent, still being selected by r, not being protected by
// object lock (a retention or legal hold set meanwhile is honoured), and on
// r still expiring v given the versions newer than v at the delete: a newer
// version removed since the walk can make v noncurrent for a shorter time
// (NoncurrentDays) or leave it fewer newer noncurrent versions than
// NewerNoncurrentVersions keeps.
func (p *pass) expireNoncurrent(ctx context.Context, v *storage.ObjectInfo, r *compiledRule) deleteOutcome {
	if v.IsLatest {
		return versionKept
	}
	if p.nd == nil {
		p.noConditionalDelete()
		return versionKept
	}
	// The newer versions the check needs: the next newer one (since when v
	// is noncurrent) and enough to count NewerNoncurrentVersions of them
	// besides the latest.
	limit := 1
	if n := r.NoncurrentVersionExpiration.NewerNoncurrentVersions; n != nil {
		limit = *n + 1
	}
	var protected *storage.ObjectInfo
	check := func(fresh storage.ObjectInfo, newer []storage.ObjectInfo) error {
		protected = nil
		if !unchanged(&fresh, v, false) || !r.matchesVersion(&fresh) || len(newer) == 0 {
			return errSkip // changed since the walk; the next pass decides
		}
		count := len(newer) // newer noncurrent versions (a lower bound at limit)
		if newer[count-1].IsLatest {
			count--
		}
		if !p.noncurrentDue(r, newer[0].LastModified, count) {
			return errSkip
		}
		if Protected(fresh, p.now) {
			protected = &fresh
			return errSkip
		}
		return nil
	}
	_, err := p.nd.DeleteVersionIf(ctx, p.bucket.Name, v.Key, v.VersionID, limit, check)
	if errors.Is(err, errors.ErrUnsupported) {
		// A routing backend whose bucket's storage cannot.
		p.noConditionalDelete()
		return versionKept
	}
	return p.deleted(err, v, r, protected, "noncurrent version expired")
}

// removeMarker permanently removes dm, an expired object delete marker
// (the only version of its key) that rule r selected. The delete is
// conditional, atomically in the backend, on dm being unchanged since the
// walk and still the latest version, and still being selected by r.
func (p *pass) removeMarker(ctx context.Context, dm *storage.ObjectInfo, r *compiledRule) deleteOutcome {
	if p.cd == nil {
		p.noConditionalDelete()
		return versionKept
	}
	check := func(fresh storage.ObjectInfo) error {
		if !unchanged(&fresh, dm, true) || !r.matchesVersion(&fresh) {
			return errSkip // changed since the walk; the next pass decides
		}
		return nil
	}
	_, err := p.cd.DeleteObjectIf(ctx, p.bucket.Name, dm.Key, dm.VersionID, check)
	return p.deleted(err, dm, r, nil, "expired object delete marker removed")
}

// deleted classifies err, the result of a conditional permanent delete of
// v that rule r selected, and logs it; protected is the version as stored
// when object lock refused the delete.
func (p *pass) deleted(err error, v *storage.ObjectInfo, r *compiledRule, protected *storage.ObjectInfo, msg string) deleteOutcome {
	switch {
	case err == nil:
	case errors.Is(err, errSkip):
		if protected != nil {
			p.protected(protected, r)
		}
		return versionKept
	case errors.Is(err, storage.ErrVersionNotFound), errors.Is(err, storage.ErrObjectNotFound):
		return versionGone
	case errors.Is(err, storage.ErrBucketNotFound):
		return versionKept
	default:
		p.fail("delete version", v, err)
		return versionKept
	}
	p.w.log.Info("lifecycle: "+msg, "bucket", p.bucket.Name, "key", v.Key, "versionId", v.VersionID,
		"deleteMarker", v.DeleteMarker, "rule", r.ID)
	return versionDeleted
}

// onlyVersion re-checks that dm is still the only version of its key.
func (p *pass) onlyVersion(ctx context.Context, dm *storage.ObjectInfo) bool {
	n := 0
	same := false
	err := p.w.backend.WalkVersions(ctx, p.bucket.Name, dm.Key, "", "", func(o storage.ObjectInfo) error {
		if o.Key != dm.Key {
			return storage.ErrStopWalk // keys extending dm.Key sort after it
		}
		n++
		same = o.VersionID == dm.VersionID && o.DeleteMarker
		if n > 1 {
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, storage.ErrBucketNotFound) {
			p.fail("walk versions", dm, err)
		}
		return false
	}
	return n == 1 && same
}

// abortUploads aborts the bucket's expired incomplete multipart uploads.
// Uploads are walked in batches of walkBatch (never all at once: a bucket
// may hold any number of pending uploads), each batch processed after its
// walk returned.
func (p *pass) abortUploads(ctx context.Context) error {
	b := p.w.backend
	var after storage.UploadPosition
	for {
		batch := make([]storage.UploadInfo, 0, 64)
		err := storage.WalkMultipartUploads(ctx, b, p.bucket.Name, p.uploadPrefix, after, func(u storage.UploadInfo) error {
			batch = append(batch, u)
			if len(batch) >= walkBatch {
				return storage.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, storage.ErrBucketNotFound) {
				return nil
			}
			return fmt.Errorf("list multipart uploads: %w", err)
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.abortUpload(ctx, &batch[i])
		}
		if len(batch) < walkBatch {
			return nil
		}
		after = storage.PositionOf(batch[len(batch)-1])
	}
}

// abortUpload aborts u if a rule says it has expired.
func (p *pass) abortUpload(ctx context.Context, u *storage.UploadInfo) {
	r := p.abortRule(u)
	if r == nil {
		return
	}
	if err := p.w.backend.AbortMultipartUpload(ctx, p.bucket.Name, u.Key, u.UploadID); err != nil {
		if errors.Is(err, storage.ErrUploadNotFound) || errors.Is(err, storage.ErrBucketNotFound) {
			return // completed or aborted concurrently
		}
		p.st.Errors++
		p.w.log.Warn("lifecycle: abort multipart upload failed", "bucket", p.bucket.Name, "key", u.Key,
			"uploadId", u.UploadID, "error", err)
		return
	}
	p.st.UploadsAborted++
	p.w.log.Info("lifecycle: incomplete multipart upload aborted", "bucket", p.bucket.Name, "key", u.Key,
		"uploadId", u.UploadID, "initiated", u.Initiated, "rule", r.ID)
}

// fail records a failed action on o.
func (p *pass) fail(action string, o *storage.ObjectInfo, err error) {
	p.st.Errors++
	p.w.log.Warn("lifecycle: "+action+" failed", "bucket", p.bucket.Name, "key", o.Key,
		"versionId", o.VersionID, "error", err)
}

// protected logs that object lock prevented an action on o.
func (p *pass) protected(o *storage.ObjectInfo, r *compiledRule) {
	p.w.log.Debug("lifecycle: version protected by object lock", "bucket", p.bucket.Name, "key", o.Key,
		"versionId", o.VersionID, "legalHold", o.LegalHold, "retainUntil", o.RetainUntil, "rule", r.ID)
}

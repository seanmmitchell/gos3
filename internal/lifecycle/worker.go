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
// locks or transactions during a walk are safe. A batch only ends at a key
// boundary, so a single key with more versions than walkBatch is still
// processed as a whole.
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

	rules      []*compiledRule // enabled rules, in document order
	objects    bool            // some rule acts on object versions
	uploads    bool            // some rule aborts uploads
	walkPrefix string          // common key prefix of all object rules
}

func newPass(w *Worker, b storage.BucketInfo, cfg *Configuration, now time.Time, st *Stats) *pass {
	p := &pass{w: w, bucket: b, now: now, st: st}
	first := true
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if !r.Enabled() {
			continue
		}
		cr := &compiledRule{Rule: r, pred: r.predicate()}
		p.rules = append(p.rules, cr)
		if r.AbortIncompleteMultipartUpload != nil {
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
		n := r.NoncurrentVersionExpiration
		if n == nil || n.NoncurrentDays == nil || !r.matchesVersion(o) {
			continue
		}
		if n.NewerNoncurrentVersions != nil && newer < *n.NewerNoncurrentVersions {
			continue
		}
		if !p.now.Before(Expiry(since, *n.NoncurrentDays)) {
			return r
		}
	}
	return nil
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
// bucket, one key's versions at a time.
func (p *pass) walkVersioned(ctx context.Context) error {
	b := p.w.backend
	keyMarker := ""
	for {
		var (
			groups [][]storage.ObjectInfo // versions of one key each, newest first
			total  int
			full   bool
		)
		err := b.WalkVersions(ctx, p.bucket.Name, p.walkPrefix, keyMarker, "", func(o storage.ObjectInfo) error {
			if n := len(groups); n == 0 || groups[n-1][0].Key != o.Key {
				if total >= walkBatch {
					full = true
					return storage.ErrStopWalk
				}
				groups = append(groups, nil)
			}
			groups[len(groups)-1] = append(groups[len(groups)-1], o)
			total++
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk versions: %w", err)
		}
		for _, vs := range groups {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.processKey(ctx, vs)
		}
		if !full {
			return nil
		}
		keyMarker = groups[len(groups)-1][0].Key
	}
}

// processKey applies the rules to the versions of one key (newest first).
func (p *pass) processKey(ctx context.Context, vs []storage.ObjectInfo) {
	if len(vs) == 0 || !vs[0].IsLatest {
		// Walks start at key boundaries, so the first version is the
		// latest; anything else means the key changed under us. It will
		// be handled by the next pass.
		return
	}
	remaining := len(vs)
	for i := 1; i < len(vs); i++ {
		v := &vs[i]
		// A version becomes noncurrent when the next newer version is
		// created; i-1 noncurrent versions are newer than it.
		r := p.noncurrentRule(v, vs[i-1].LastModified, i-1)
		if r == nil {
			continue
		}
		switch p.deleteVersion(ctx, v, r, "noncurrent version expired") {
		case versionDeleted:
			p.st.NoncurrentExpired++
			remaining--
		case versionGone:
			remaining--
		}
	}

	cur := &vs[0]
	if !cur.DeleteMarker {
		p.expireCurrent(ctx, cur, vs)
		return
	}
	if remaining != 1 {
		return
	}
	r := p.markerRule(cur)
	if r == nil || !p.onlyVersion(ctx, cur) {
		return
	}
	if p.deleteVersion(ctx, cur, r, "expired object delete marker removed") == versionDeleted {
		p.st.DeleteMarkersRemoved++
	}
}

// expireCurrent applies Expiration Days/Date to o, the current version of
// its key. versions holds all versions of the key in a versioned bucket
// (nil for unversioned buckets).
func (p *pass) expireCurrent(ctx context.Context, o *storage.ObjectInfo, versions []storage.ObjectInfo) {
	r := p.currentRule(o)
	if r == nil {
		return
	}
	b := p.w.backend
	// Re-read the current version right before acting: it may have been
	// overwritten, deleted or re-tagged since the walk.
	fresh, err := b.GetObjectInfo(ctx, p.bucket.Name, o.Key, "")
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotFound) && !errors.Is(err, storage.ErrBucketNotFound) {
			p.fail("read object", o, err)
		}
		return
	}
	if fresh.DeleteMarker || fresh.VersionID != o.VersionID || !fresh.LastModified.Equal(o.LastModified) ||
		fresh.ETag != o.ETag || p.currentRule(&fresh) == nil {
		return
	}
	// Which data a delete with no version ID removes permanently: the null
	// version in unversioned and suspended buckets (in the latter, the
	// null delete marker replaces it); nothing in enabled buckets.
	switch p.bucket.Versioning {
	case storage.VersioningUnversioned:
		if Protected(fresh, p.now) {
			p.protected(&fresh, r)
			return
		}
	case storage.VersioningSuspended:
		for i := range versions {
			if versions[i].VersionID == storage.NullVersionID && Protected(versions[i], p.now) {
				p.protected(&versions[i], r)
				return
			}
		}
	}
	res, err := b.DeleteObject(ctx, p.bucket.Name, o.Key, "")
	if err != nil {
		if !errors.Is(err, storage.ErrBucketNotFound) {
			p.fail("expire object", o, err)
		}
		return
	}
	p.st.Expired++
	attrs := []any{"bucket", p.bucket.Name, "key", o.Key, "versionId", o.VersionID, "rule", r.ID}
	if res.DeleteMarker {
		attrs = append(attrs, "deleteMarkerVersionId", res.VersionID)
	}
	p.w.log.Info("lifecycle: object expired", attrs...)
}

// deleteOutcome is the result of deleteVersion.
type deleteOutcome int

const (
	versionKept    deleteOutcome = iota // not deleted (changed, protected or failed)
	versionDeleted                      // deleted by this pass
	versionGone                         // already removed by someone else
)

// deleteVersion permanently removes the version v after re-checking that
// it is unchanged since the walk (in particular that a noncurrent version
// has not become the latest) and not protected by object lock.
func (p *pass) deleteVersion(ctx context.Context, v *storage.ObjectInfo, r *compiledRule, msg string) deleteOutcome {
	b := p.w.backend
	fresh, err := b.GetObjectInfo(ctx, p.bucket.Name, v.Key, v.VersionID)
	switch {
	case errors.Is(err, storage.ErrVersionNotFound), errors.Is(err, storage.ErrObjectNotFound):
		return versionGone
	case err != nil:
		if !errors.Is(err, storage.ErrBucketNotFound) {
			p.fail("read version", v, err)
		}
		return versionKept
	}
	if fresh.IsLatest != v.IsLatest || fresh.DeleteMarker != v.DeleteMarker || !fresh.LastModified.Equal(v.LastModified) {
		return versionKept // changed since the walk; the next pass decides
	}
	if Protected(fresh, p.now) {
		p.protected(&fresh, r)
		return versionKept
	}
	if _, err := b.DeleteObject(ctx, p.bucket.Name, v.Key, v.VersionID); err != nil {
		if errors.Is(err, storage.ErrVersionNotFound) || errors.Is(err, storage.ErrObjectNotFound) {
			return versionGone
		}
		if !errors.Is(err, storage.ErrBucketNotFound) {
			p.fail("delete version", v, err)
		}
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
func (p *pass) abortUploads(ctx context.Context) error {
	b := p.w.backend
	uploads, err := b.ListMultipartUploads(ctx, p.bucket.Name)
	if err != nil {
		if errors.Is(err, storage.ErrBucketNotFound) {
			return nil
		}
		return fmt.Errorf("list multipart uploads: %w", err)
	}
	for i := range uploads {
		if err := ctx.Err(); err != nil {
			return err
		}
		u := &uploads[i]
		r := p.abortRule(u)
		if r == nil {
			continue
		}
		if err := b.AbortMultipartUpload(ctx, p.bucket.Name, u.Key, u.UploadID); err != nil {
			if errors.Is(err, storage.ErrUploadNotFound) || errors.Is(err, storage.ErrBucketNotFound) {
				continue // completed or aborted concurrently
			}
			p.st.Errors++
			p.w.log.Warn("lifecycle: abort multipart upload failed", "bucket", p.bucket.Name, "key", u.Key,
				"uploadId", u.UploadID, "error", err)
			continue
		}
		p.st.UploadsAborted++
		p.w.log.Info("lifecycle: incomplete multipart upload aborted", "bucket", p.bucket.Name, "key", u.Key,
			"uploadId", u.UploadID, "initiated", u.Initiated, "rule", r.ID)
	}
	return nil
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

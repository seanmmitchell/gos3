// Package multi implements a storage.Backend that spreads buckets over
// several named backings (other storage.Backends, typically one fs
// directory per disk) and keeps copies of a bucket on more than one
// backing.
//
// Every bucket has a placement: the backing holding its primary copy and,
// optionally, replicas on other backings, in one of three modes:
//
//   - single: one copy, on the primary.
//   - active-passive: the primary serves every request; changed keys are
//     copied to the replicas in the background. Reads fall back to a
//     replica when the primary's backing fails; a replica takes over
//     writes when it is promoted (Promote, or automatically with
//     Options.AutoFailover).
//   - active-active: a change is written to the primary and every in-sync
//     replica before it is acknowledged, object reads are spread over the
//     in-sync members, and an in-sync replica is promoted automatically
//     when the primary's backing fails.
//
// A bucket can also be moved, online, from one backing to another (Move):
// the target is filled in the background like a replica, then a short
// cutover makes it take the source's place and the source copy is
// deleted.
//
// Copies keep version IDs, LastModified times and the order of every key's
// versions; this needs the storage.VersionImporter extension on the
// buckets receiving copies. What a bucket supports can differ between the
// buckets of one backing (storage.BucketCapabilities): an s3 backing holds
// wrapped buckets, stored in gos3's format, which can be replicated, moved
// and receive copies like the buckets of fs and memory backings, and
// native buckets, stored as-is in a remote bucket, which can do none of
// that (they keep the remote's version IDs, which are not copied).
// Replicas and move targets are created with
// storage.CreateBucketOptions.Import set, so that the backing picks a
// bucket kind that imports versions; a move can name the kind and
// location of the target (Move's target, persisted with the move). Keys
// are copied by comparing their version lists (see syncKey), so copying is
// idempotent and catching up after any failure is just another comparison.
//
// A bucket can also be released (ReleaseBucket): the router forgets a
// single-copy bucket whose backing can let it go without deleting its
// data (a native bucket keeps its remote bucket).
//
// For the layers above it (copy jobs, snapshot schedulers), the router
// imports versions itself (storage.VersionImporter, into a bucket's
// primary and in-sync active-active replicas; see import.go), reports the
// changes made through it to subscribers (Subscribe), and answers for each
// bucket's capabilities (storage.BucketCapabilities) and restic repository
// (restic.Host) with those of its primary copy.
//
// A backing is marked unhealthy after a failure of the backing itself (see
// isBackingError) or a failed health probe (storage.HealthProber, else
// ListBuckets). A failure of one bucket's remote location
// (storage.ErrBucketUnavailable) does not mark its backing unhealthy:
// reads fall back to another copy and an active-active replica stops
// receiving synchronous writes until it has caught up again.
//
// The placements are persisted in a JSON state file (Options.StatePath).
// Changes waiting to be copied to a replica are only kept in memory: after
// a restart every replica is compared with its primary in full, in the
// background, and active-active replicas receive synchronous writes again
// once that comparison has finished.
//
// In-progress multipart uploads live on the primary only: they are not
// copied, and after the primary changes (promotion, move) they fail with
// storage.ErrUploadNotFound.
package multi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Errors returned by the placement management methods. They are wrapped
// with a description; test with errors.Is.
var (
	// ErrUnknownBacking: the request names a backing that is not configured.
	ErrUnknownBacking = errors.New("multi: unknown backing")
	// ErrInvalidPlacement: the requested placement is not valid (bad mode,
	// replica list, or member).
	ErrInvalidPlacement = errors.New("multi: invalid placement")
	// ErrConflict: the change conflicts with the bucket's current state
	// (a move in progress, a copy still being deleted, a replica that has
	// not finished its initial copy, ...).
	ErrConflict = errors.New("multi: conflicting placement change")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("multi: backend closed")
)

// Defaults of Options.
const (
	DefaultWorkers         = 2
	DefaultHealthInterval  = 10 * time.Second
	DefaultCutoverTimeout  = 60 * time.Second
	DefaultOpenListTimeout = 15 * time.Second
)

// Backing is one named storage backend.
type Backing struct {
	Name string
	Kind string // "fs", "memory" or "s3" (informational)
	Path string // data directory ("" for memory and s3)
	// Location says where an s3 backing's data lives, without secrets
	// (e.g. "s3://bucket/prefix/"); "" for fs and memory (informational).
	Location string
	// Backend stores the backing's buckets. The bucket kinds it offers
	// (storage.KindReporter) are reported by Router.Backings, and what each
	// bucket supports (storage.BucketCapabilities) decides where copies can
	// be made from and to.
	Backend storage.Backend
}

// Options configure a Router.
type Options struct {
	// Backings are the configured backings, at least one, with unique
	// names. The Router owns them: Close closes them.
	Backings []Backing
	// Default names the backing new buckets are created on (default:
	// Backings[0]).
	Default string
	// NewBucketMode and NewBucketReplicas are the replication of new
	// buckets (default: single, no replicas). Replicas must not include
	// Default.
	NewBucketMode     Mode
	NewBucketReplicas []string
	// StatePath is the placements state file ("" = not persisted).
	StatePath string
	// Workers is the number of concurrent copy jobs (default
	// DefaultWorkers).
	Workers int
	// HealthInterval is how often every backing is probed (default
	// DefaultHealthInterval).
	HealthInterval time.Duration
	// AutoFailover promotes an in-sync replica of an active-passive
	// bucket when its primary's backing fails. Changes not yet copied to
	// the replica are lost. Active-active buckets always fail over.
	AutoFailover bool
	// CutoverTimeout bounds how long a cutover waits for the writes in
	// flight on the bucket (default DefaultCutoverTimeout). New writes
	// wait meanwhile.
	CutoverTimeout time.Duration
	// OpenListTimeout bounds the listing of each backing's buckets in Open
	// (default DefaultOpenListTimeout). A backing that fails to answer in
	// time does not fail Open: it starts unhealthy, and the placements on
	// it are kept as they are.
	OpenListTimeout time.Duration
	Logger          *slog.Logger
}

// backing is a configured backing with its health.
type backing struct {
	Backing
	healthy   atomic.Bool
	mu        sync.Mutex
	lastErr   string
	checkedAt time.Time
}

func (b *backing) setDown(err error) bool {
	b.mu.Lock()
	b.lastErr = err.Error()
	b.mu.Unlock()
	return b.healthy.Swap(false)
}

func (b *backing) setUp(at time.Time) bool {
	b.mu.Lock()
	b.lastErr = ""
	b.checkedAt = at
	b.mu.Unlock()
	return !b.healthy.Swap(true)
}

// Router is the multi-backing storage.Backend. Create it with Open.
type Router struct {
	log        *slog.Logger
	opts       Options
	topology   atomic.Pointer[backingTopology]
	management *backingManagement

	// adminMu serializes bucket creation and deletion and placement
	// changes (the slow, rare operations), so that each sees a stable
	// placement while it validates and acts.
	adminMu sync.Mutex

	// mu guards buckets, the rec, members and syncMode fields of every
	// bucketState, and purges.
	mu      sync.RWMutex
	buckets map[string]*bucketState
	purges  map[purgeRecord]*purgeJob

	// pendingAdopt names the backings whose buckets Open could not list:
	// their buckets without a placement are adopted when the backing
	// becomes healthy (see adoptPending). Guarded by mu.
	pendingAdopt map[string]bool
	// listWarnAt is when ListBuckets last warned about buckets it had to
	// leave out (unix nanoseconds), to rate-limit that warning.
	listWarnAt atomic.Int64
	// probeNow receives backings the health loop probes at once.
	probeNow chan *backing

	saveMu sync.Mutex // serializes state file writes

	keys keyLocks
	jobs *workQueue

	// changes are the subscribers to the changes made through the router
	// (see Subscribe).
	changes changeFeed

	ctx    context.Context // canceled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

// bucketState is the runtime state of one bucket.
type bucketState struct {
	// configMu serializes full configuration copies with client writes.
	configMu sync.Mutex
	name     string
	gate     gate
	// Guarded by Router.mu:
	rec     *placementRecord        // current placement (immutable snapshot)
	members map[string]*memberState // members and the move target, by backing name
	movePh  string                  // move phase ("copying" or "cutover")
	removed bool                    // the bucket was deleted
	rr      atomic.Uint32           // round-robin counter for active-active reads
	// info is the bucket's description from the last successful listing or
	// GetBucket, which ListBuckets reports when no copy can be reached.
	info atomic.Pointer[storage.BucketInfo]
}

// cacheInfo remembers in as the bucket's description.
func (bs *bucketState) cacheInfo(in storage.BucketInfo) {
	bs.info.Store(&in)
}

var (
	_ storage.Backend                = (*Router)(nil)
	_ storage.ConditionalDeleter     = (*Router)(nil)
	_ storage.NewerVersionsDeleter   = (*Router)(nil)
	_ storage.CheckedDeleter         = (*Router)(nil)
	_ storage.ConditionalMetaUpdater = (*Router)(nil)
	_ storage.ConfigViewer           = (*Router)(nil)
	_ storage.UploadWalker           = (*Router)(nil)
	_ storage.CompressingBackend     = (*Router)(nil)
	_ storage.VersionImporter        = (*Router)(nil)
	_ storage.CompressingImporter    = (*Router)(nil)
	_ storage.BucketCapabilities     = (*Router)(nil)
)

// Open builds a Router over opts.Backings: it loads the state file,
// reconciles it with the buckets the backings actually hold (buckets
// without a placement are adopted as single-copy buckets on the backing
// holding them), saves it and starts the background copy and health
// workers. The backings are closed by Close, also when Open fails.
//
// A backing whose buckets cannot be listed (within
// Options.OpenListTimeout) does not fail Open: it starts unhealthy, the
// placements on it are kept unchanged (it is assumed to still hold its
// copies), the adoption of its buckets without a placement waits until it
// is healthy, and it is probed at once, its buckets failing over as at
// runtime when their mode allows it.
func Open(ctx context.Context, opts Options) (_ *Router, err error) {
	if len(opts.Backings) == 0 {
		return nil, errors.New("multi: no backings")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = DefaultHealthInterval
	}
	if opts.CutoverTimeout <= 0 {
		opts.CutoverTimeout = DefaultCutoverTimeout
	}
	if opts.OpenListTimeout <= 0 {
		opts.OpenListTimeout = DefaultOpenListTimeout
	}
	r := &Router{
		log:          opts.Logger,
		opts:         opts,
		buckets:      map[string]*bucketState{},
		purges:       map[purgeRecord]*purgeJob{},
		pendingAdopt: map[string]bool{},
		probeNow:     make(chan *backing, 16),
	}
	topology := &backingTopology{backings: map[string]*backing{}}
	r.topology.Store(topology) // Close also owns partially opened backings on failure.
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	now := time.Now().UTC()
	for _, cfg := range opts.Backings {
		if cfg.Backend == nil {
			return nil, errors.New("multi: a backing needs a backend")
		}
		b := &backing{Backing: cfg}
		b.setUp(now)
		topology.order = append(topology.order, b) // closed by Close, also on failure
	}
	for _, b := range topology.order {
		if b.Name == "" {
			return nil, errors.New("multi: a backing needs a name")
		}
		if _, dup := topology.backings[b.Name]; dup {
			return nil, fmt.Errorf("multi: duplicate backing %q", b.Name)
		}
		topology.backings[b.Name] = b
	}
	topology.def = topology.order[0]
	if opts.Default != "" {
		if topology.def = topology.backings[opts.Default]; topology.def == nil {
			return nil, fmt.Errorf("%w: default backing %q", ErrUnknownBacking, opts.Default)
		}
	}
	if opts.NewBucketMode == "" {
		opts.NewBucketMode = ModeSingle
		if len(opts.NewBucketReplicas) > 0 {
			opts.NewBucketMode = ModeActivePassive
		}
	}
	if !opts.NewBucketMode.Valid() || (opts.NewBucketMode == ModeSingle) != (len(opts.NewBucketReplicas) == 0) {
		return nil, fmt.Errorf("%w: new bucket mode %q with replicas %v", ErrInvalidPlacement, opts.NewBucketMode, opts.NewBucketReplicas)
	}
	replicas := make([]*backing, 0, len(opts.NewBucketReplicas))
	for i, name := range opts.NewBucketReplicas {
		b := topology.backings[name]
		if b == nil {
			return nil, fmt.Errorf("%w: new bucket replica %q", ErrUnknownBacking, name)
		}
		if name == topology.def.Name || slices.Contains(opts.NewBucketReplicas[:i], name) {
			return nil, fmt.Errorf("%w: new bucket replica %q is the default backing or listed twice", ErrInvalidPlacement, name)
		}
		replicas = append(replicas, b)
	}
	if err := checkNewBucketReplicas(ctx, topology.def, replicas); err != nil {
		return nil, err
	}
	r.opts = opts
	st, err := loadState(opts.StatePath)
	if err != nil {
		return nil, err
	}
	unknown, err := r.reconcile(ctx, st)
	if err != nil {
		return nil, err
	}
	if err := r.save(); err != nil {
		return nil, fmt.Errorf("saving %s: %w", opts.StatePath, err)
	}

	r.jobs = newWorkQueue()
	for range opts.Workers {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.runWorker()
		}()
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.healthLoop()
	}()
	r.mu.RLock()
	for _, bs := range r.buckets {
		for _, ms := range bs.members {
			if ms.backing.Name != bs.rec.primary() {
				r.kick(ms)
			}
		}
	}
	for _, j := range r.purges {
		r.startPurge(j)
	}
	r.mu.RUnlock()
	// React to the backings that could not be listed as to a failure at
	// runtime, and probe them at once: a transient failure should not
	// wait for the next health check.
	for _, b := range unknown {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.onBackingDown(b)
			r.probe(b)
		}()
	}
	return r, nil
}

// listAll lists the buckets of every backing concurrently, each within
// Options.OpenListTimeout. A backing that fails is marked unhealthy and
// returned in unknown instead; only the cancellation of ctx fails listAll.
func (r *Router) listAll(ctx context.Context) (lists map[string][]storage.BucketInfo, unknown []*backing, err error) {
	order := r.topology.Load().order
	infos := make([][]storage.BucketInfo, len(order))
	errs := make([]error, len(order))
	var wg sync.WaitGroup
	for i, b := range order {
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, r.opts.OpenListTimeout)
			defer cancel()
			infos[i], errs[i] = b.Backend.ListBuckets(lctx)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing the buckets of the backings: %w", err)
	}
	lists = map[string][]storage.BucketInfo{}
	for i, b := range order {
		err := errs[i]
		if err == nil {
			lists[b.Name] = infos[i]
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("listing its buckets timed out after %s: %w", r.opts.OpenListTimeout, err)
		}
		b.setDown(err)
		b.mu.Lock()
		b.checkedAt = time.Now().UTC()
		b.mu.Unlock()
		r.log.Warn("could not list the buckets of a storage backing; starting with it unhealthy and keeping "+
			"its bucket placements as they are", "backing", b.Name, "err", err)
		unknown = append(unknown, b)
	}
	return lists, unknown, nil
}

// reconcile turns the loaded state into the runtime state, checking it
// against the buckets the backings hold. It returns the backings whose
// buckets could not be listed ("unknown"): they are assumed to still hold
// every copy placed on them (no copy is forgotten or promoted because of
// them), and the adoption of their other buckets waits until they are
// healthy (pendingAdopt).
func (r *Router) reconcile(ctx context.Context, st *stateFile) ([]*backing, error) {
	lists, unknown, err := r.listAll(ctx)
	if err != nil {
		return nil, err
	}
	present := map[string]map[string]storage.BucketInfo{} // backing -> buckets
	for name, infos := range lists {
		set := make(map[string]storage.BucketInfo, len(infos))
		for _, in := range infos {
			set[in.Name] = in
		}
		present[name] = set
	}
	isUnknown := map[string]bool{}
	for _, b := range unknown {
		isUnknown[b.Name] = true
		r.pendingAdopt[b.Name] = true
	}
	has := func(backing, bucket string) bool {
		if isUnknown[backing] {
			return true
		}
		_, ok := present[backing][bucket]
		return ok
	}

	for _, p := range st.Purges {
		if r.topology.Load().backings[p.Backing] == nil {
			r.log.Warn("dropping the deletion of a bucket copy on a backing that is no longer configured",
				"bucket", p.Bucket, "backing", p.Backing)
			continue
		}
		r.purges[p] = &purgeJob{rec: p}
	}
	for _, p := range st.DivergedPurges {
		if j := r.purges[p]; j != nil {
			j.diverged = true
		}
	}

	for name, rec := range st.Buckets {
		rec = rec.clone()
		// Forget members on backings that are no longer configured.
		var kept []memberRecord
		for _, m := range rec.Members {
			if r.topology.Load().backings[m.Backing] == nil {
				r.log.Warn("bucket copy on a backing that is no longer configured is forgotten",
					"bucket", name, "backing", m.Backing)
				continue
			}
			if slices.ContainsFunc(kept, func(k memberRecord) bool { return k.Backing == m.Backing }) {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("bucket %q has no copy on a configured backing (it was on %s); configure that backing "+
				"again in storage.backings, or remove the bucket from %s to forget it",
				name, memberNames(rec.Members), r.opts.StatePath)
		}
		if kept[0].Backing != rec.Members[0].Backing {
			i := slices.IndexFunc(kept, func(m memberRecord) bool { return m.Synced && !m.Diverged && has(m.Backing, name) })
			if i < 0 {
				return nil, fmt.Errorf("bucket %q has no synchronized replica to replace its missing primary", name)
			}
			kept[0], kept[i] = kept[i], kept[0]
			r.log.Warn("the primary backing of a bucket is no longer configured: promoting a replica",
				"bucket", name, "old_primary", rec.Members[0].Backing, "new_primary", kept[0].Backing)
		}
		rec.Members = kept
		// The primary must hold the bucket; else promote a member that
		// does, or forget the bucket when none does.
		if !has(rec.primary(), name) {
			i := slices.IndexFunc(rec.Members, func(m memberRecord) bool { return m.Synced && !m.Diverged && has(m.Backing, name) })
			if i < 0 {
				if slices.ContainsFunc(rec.Members, func(m memberRecord) bool { return has(m.Backing, name) }) {
					return nil, fmt.Errorf("bucket %q has no synchronized replica to replace its missing primary", name)
				}
				r.log.Warn("forgetting a bucket that none of its backings holds any more",
					"bucket", name, "backings", memberNames(rec.Members))
				continue
			}
			r.log.Warn("the primary backing of a bucket does not hold it: promoting a replica that does",
				"bucket", name, "old_primary", rec.primary(), "new_primary", rec.Members[i].Backing)
			rec.Members[0], rec.Members[i] = rec.Members[i], rec.Members[0]
			rec.Members[i].Synced = false
		}
		rec.Members[0].Synced = true
		for i := 1; i < len(rec.Members); i++ {
			if !has(rec.Members[i].Backing, name) {
				rec.Members[i].Synced = false
			}
		}
		if len(rec.Members) == 1 {
			rec.Mode = ModeSingle
		} else if rec.Mode == ModeSingle {
			rec.Mode = ModeActivePassive
		}
		if mv := rec.Move; mv != nil {
			if r.topology.Load().backings[mv.To] == nil || rec.member(mv.From) < 0 || rec.member(mv.To) >= 0 {
				r.log.Warn("dropping a bucket move that no longer applies", "bucket", name, "from", mv.From, "to", mv.To)
				if r.topology.Load().backings[mv.To] != nil && rec.member(mv.To) < 0 && has(mv.To, name) {
					r.addPurgeLocked(name, mv.To, false)
				}
				rec.Move = nil
			}
		}
		r.buckets[name] = r.newBucketState(name, rec)
	}

	// Adopt buckets without a placement: the default backing first, then
	// the configuration order.
	adoptOrder := append([]*backing{r.topology.Load().def}, r.topology.Load().order...)
	for _, b := range adoptOrder {
		if !isUnknown[b.Name] {
			r.adoptLocked(b, lists[b.Name])
		}
	}
	// Seed the descriptions ListBuckets falls back on, preferring the
	// primary's.
	for name, bs := range r.buckets {
		for _, m := range bs.rec.Members {
			if in, ok := present[m.Backing][name]; ok {
				bs.cacheInfo(in)
				break
			}
		}
	}
	return unknown, nil
}

// adoptLocked gives the buckets of infos (listed on backing b) that have
// no placement a single-copy placement on b, and warns about copies of
// buckets placed elsewhere. It reports whether it adopted any. Router.mu
// must be held (or Open must be running).
func (r *Router) adoptLocked(b *backing, infos []storage.BucketInfo) bool {
	adopted := false
	for _, in := range infos {
		name := in.Name
		if bs := r.buckets[name]; bs != nil {
			if !bs.rec.holds(b.Name) && r.purges[purgeRecord{Bucket: name, Backing: b.Name}] == nil {
				r.log.Warn("a backing holds a copy of a bucket that is placed elsewhere; the copy is ignored "+
					"(delete it, or the bucket on that backing, by hand when it is not needed)",
					"bucket", name, "backing", b.Name, "placement", memberNames(bs.rec.Members))
			}
			continue
		}
		if r.purges[purgeRecord{Bucket: name, Backing: b.Name}] != nil {
			continue // a deleted bucket's copy still being removed
		}
		bs := r.newBucketState(name, &placementRecord{
			Mode:    ModeSingle,
			Members: []memberRecord{{Backing: b.Name, Synced: true}},
		})
		bs.cacheInfo(in)
		r.buckets[name] = bs
		adopted = true
	}
	return adopted
}

// adoptPending adopts the buckets without a placement of backing b, which
// Open could not list, now that b is healthy. The listing runs without
// adminMu (it is remote I/O); the adoption under it. A failed listing
// marks b unhealthy again, so that its next recovery retries.
func (r *Router) adoptPending(b *backing) {
	r.mu.RLock()
	pending := r.pendingAdopt[b.Name]
	r.mu.RUnlock()
	if !pending || r.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.opts.OpenListTimeout)
	infos, err := b.Backend.ListBuckets(ctx)
	cancel()
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	r.mu.RLock()
	pending = r.pendingAdopt[b.Name]
	r.mu.RUnlock()
	switch {
	case !pending || r.closed.Load():
		return // adopted meanwhile
	case r.topology.Load().backings[b.Name] != b:
		return // replaced or removed meanwhile, which settles its adoption
	case err != nil:
		if r.ctx.Err() == nil {
			r.log.Warn("could not list the buckets of a recovered storage backing to adopt them; "+
				"retrying when it recovers again", "backing", b.Name, "err", err)
			if b.setDown(fmt.Errorf("listing its buckets: %w", err)) {
				go r.onBackingDown(b)
			}
		}
		return
	}
	if r.adoptListed(b, infos) {
		r.saveLogged("adopting buckets")
	}
}

// adoptListed ends the pending adoption of backing b (see pendingAdopt)
// with infos, its buckets, and reports whether it adopted any (the caller
// saves the placements then). The caller holds adminMu.
func (r *Router) adoptListed(b *backing, infos []storage.BucketInfo) bool {
	r.mu.Lock()
	delete(r.pendingAdopt, b.Name)
	adopted := r.adoptLocked(b, infos)
	r.mu.Unlock()
	if adopted {
		r.log.Info("adopted the buckets without a placement of a storage backing that could not be listed at startup",
			"backing", b.Name)
	}
	return adopted
}

func memberNames(ms []memberRecord) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Backing
	}
	return out
}

// newBucketState builds the runtime state of a bucket. Every member but
// the primary, and the move target, starts with a full comparison.
func (r *Router) newBucketState(name string, rec *placementRecord) *bucketState {
	if rec.ID == "" {
		// Upgrade placements written before IDs existed, then persist them
		// with the normal save at the end of Open.
		rec = rec.clone()
		rec.ID = strings.ToLower(rand.Text())
	}
	bs := &bucketState{name: name, rec: rec, members: map[string]*memberState{}}
	for _, m := range rec.Members {
		ms := r.newMemberState(bs, m.Backing)
		if m.Backing != rec.primary() {
			ms.needFull = true
		}
		bs.members[m.Backing] = ms
	}
	if rec.Move != nil {
		ms := r.newMemberState(bs, rec.Move.To)
		ms.needFull = true
		bs.members[rec.Move.To] = ms
		bs.movePh = phaseCopying
	}
	return bs
}

// save writes the current placements to the state file.
func (r *Router) save() error {
	if r.opts.StatePath == "" {
		return nil
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	st := &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{}}
	r.mu.RLock()
	for name, bs := range r.buckets {
		st.Buckets[name] = bs.rec
	}
	for p, j := range r.purges {
		st.Purges = append(st.Purges, p)
		if j.diverged {
			st.DivergedPurges = append(st.DivergedPurges, p)
		}
	}
	r.mu.RUnlock()
	for _, ps := range [][]purgeRecord{st.Purges, st.DivergedPurges} {
		slices.SortFunc(ps, func(a, b purgeRecord) int {
			if a.Bucket != b.Bucket {
				return cmpString(a.Bucket, b.Bucket)
			}
			return cmpString(a.Backing, b.Backing)
		})
	}
	return writeState(r.opts.StatePath, st)
}

// saveLogged saves and logs a failure (for background changes that have
// no caller to report to).
func (r *Router) saveLogged(what string) {
	if err := r.save(); err != nil {
		r.log.Error("could not save the bucket placements", "after", what, "path", r.opts.StatePath, "err", err)
	}
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Close stops the background work and closes every backing.
func (r *Router) Close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.cancel()
		if r.jobs != nil {
			r.jobs.close()
		}
		r.wg.Wait()
		r.adminMu.Lock()
		defer r.adminMu.Unlock()
		var errs []error
		for _, b := range r.topology.Load().order {
			if err := b.Backend.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing backing %q: %w", b.Name, err))
			}
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// DefaultBacking returns the name of the backing new buckets are created on.
func (r *Router) DefaultBacking() string { return r.topology.Load().def.Name }

// AutoFailover reports whether active-passive buckets fail over
// automatically.
func (r *Router) AutoFailover() bool { return r.opts.AutoFailover }

// --- health ---

// isBackingError reports whether err (returned by a backing) indicates a
// failure of the backing itself rather than an answer about the request
// (not found, exists, precondition failed, an operation the backing does
// not support, a target it cannot honour, a refusal by a remote service,
// ...), contention between clients (storage.ErrConflict: any user can
// cause it on their own objects, so it must never fail over other
// buckets), a canceled request or a failure that other buckets on the backing
// do not share: one bucket's encryption (a missing key, an unavailable
// restic repository) or one bucket's remote location
// (storage.ErrBucketUnavailable). storage.ErrBackingUnavailable (e.g. an
// unreachable remote service) is a backing error.
func isBackingError(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range []error{
		storage.ErrBucketNotFound, storage.ErrBucketExists, storage.ErrBucketNotEmpty,
		storage.ErrObjectNotFound, storage.ErrVersionNotFound, storage.ErrUploadNotFound,
		storage.ErrPartNotFound, storage.ErrConfigNotFound, storage.ErrPreconditionFailed,
		storage.ErrInvalidName, storage.ErrStopWalk, context.Canceled, context.DeadlineExceeded,
		errors.ErrUnsupported, storage.ErrEncryptionUnavailable, storage.ErrNotSupported,
		storage.ErrResourceLimit,
		storage.ErrInvalidBacking, storage.ErrBucketUnavailable, storage.ErrRemoteDenied,
		storage.ErrRemoteRejected, storage.ErrInvalidTarget, storage.ErrConflict,
	} {
		if errors.Is(err, s) {
			return false
		}
	}
	var ce *callerError
	return !errors.As(err, &ce)
}

// callerError wraps an error returned by a caller's callback (walk
// function, delete check, metadata update), which says nothing about the
// backing's health.
type callerError struct{ err error }

func (e *callerError) Error() string { return e.err.Error() }
func (e *callerError) Unwrap() error { return e.err }

// observe records the outcome of a call to backing b and returns err
// unchanged. A backing error marks b unhealthy.
func (r *Router) observe(b *backing, err error) error {
	if isBackingError(err) && !r.closed.Load() {
		if b.setDown(err) {
			r.log.Warn("storage backing failed; marking it unhealthy", "backing", b.Name, "err", err)
			go r.onBackingDown(b)
		}
	}
	return err
}

// healthLoop probes every backing periodically.
func (r *Router) healthLoop() {
	t := time.NewTicker(r.opts.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case b := <-r.probeNow:
			if r.topology.Load().backings[b.Name] == b { // not replaced meanwhile
				r.probe(b)
			}
			continue
		case <-t.C:
		}
		for _, b := range r.topology.Load().order {
			r.probe(b)
		}
	}
}

// requestProbe asks the health loop to probe b soon (e.g. after its
// connection settings changed), rather than at the next interval.
func (r *Router) requestProbe(b *backing) {
	select {
	case r.probeNow <- b:
	default: // the periodic probe will do
	}
}

// probeTimeout bounds one health probe.
const probeTimeout = 5 * time.Second

// probe checks backing b with its own health check
// (storage.HealthProber), or else by listing its buckets.
func (r *Router) probe(b *backing) {
	ctx, cancel := context.WithTimeout(r.ctx, probeTimeout)
	var err error
	if hp, ok := b.Backend.(storage.HealthProber); ok {
		err = hp.ProbeHealth(ctx)
	} else {
		_, err = b.Backend.ListBuckets(ctx)
	}
	cancel()
	if r.ctx.Err() != nil {
		return
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("health check timed out after %s", probeTimeout)
		}
		if b.setDown(err) {
			r.log.Warn("storage backing failed its health check; marking it unhealthy", "backing", b.Name, "err", err)
			r.onBackingDown(b)
		}
		b.mu.Lock()
		b.checkedAt = time.Now().UTC()
		b.mu.Unlock()
		return
	}
	if b.setUp(time.Now().UTC()) {
		r.log.Info("storage backing is healthy again", "backing", b.Name)
		r.onBackingUp(b)
	}
}

// onBackingDown reacts to backing b becoming unhealthy: active-active
// replicas on b stop receiving synchronous writes, and buckets whose
// primary is on b fail over when their mode allows it.
func (r *Router) onBackingDown(b *backing) {
	var failover []*bucketState
	r.mu.Lock()
	for _, bs := range r.buckets {
		ms := bs.members[b.Name]
		if ms == nil || ms.backing != b {
			continue
		}
		if bs.rec.primary() == b.Name {
			if bs.rec.Mode == ModeActiveActive || (bs.rec.Mode == ModeActivePassive && r.opts.AutoFailover) {
				failover = append(failover, bs)
			}
			continue
		}
		if ms.syncMode {
			ms.syncMode = false
			ms.mu.Lock()
			ms.needFull = true // writes may have been lost in flight
			ms.mu.Unlock()
		}
	}
	r.mu.Unlock()
	for _, bs := range failover {
		r.failover(bs, b.Name)
	}
}

// onBackingUp restarts the copy work that waited for a backing, and
// adopts its buckets without a placement when Open could not list them.
func (r *Router) onBackingUp(b *backing) {
	r.mu.RLock()
	pending := r.pendingAdopt[b.Name]
	r.mu.RUnlock()
	if pending && r.ctx.Err() == nil {
		// Not on the caller's goroutine: the health loop must not wait for
		// the listing. The caller runs in r.wg, so Add cannot race Wait.
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.adoptPending(b)
		}()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, bs := range r.buckets {
		for _, ms := range bs.members {
			if ms.backing.Name != bs.rec.primary() {
				r.kick(ms)
			}
		}
	}
}

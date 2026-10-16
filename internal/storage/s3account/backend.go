package s3account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/localstate"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Lifecycle
//
// Open does no remote I/O that can fail it. With a home, the home's store
// loads in the background; once it serves, the registry is read from its
// aux namespace "account", the native store is opened on its aux namespace
// "gateway", the registered containers are opened (each loading in the
// background) and the home is reconciled (which runs the migrations of
// legacy homes, see reconcile.go); then Ready is closed. Until then every
// operation answers storage.ErrBackingUnavailable. Without a home the
// records are in a local state database, read by Open itself, so the
// backing is ready when Open returns (its containers may still load).
//
// Locks: adminMu serializes every change of the records and every
// structural change (buckets created, deleted or released, containers
// added or removed, reconciliation, migrations, native bucket settings);
// mu guards the in-memory state (registry, containers, native store) and
// is only held briefly, never across remote I/O. adminMu is taken before
// mu.

// Backend is an s3 backing. Create it with Open.
type Backend struct {
	opts  Options
	name  string
	log   *slog.Logger
	hooks hooks
	// endpoint is the service the backing's claims are on: the endpoint
	// at Open (the claims keep it when a connection change later names
	// the service differently).
	endpoint string

	home        *s3store.Backend // nil without a home
	homeRelease func()
	local       *localstate.DB // nil with a home
	records     s3store.AuxStore
	natState    native.StateStore

	// conn is the current connection; containers opened later use it.
	conn atomic.Pointer[s3store.Connection]

	ready     chan struct{}
	readyOnce sync.Once
	closed    atomic.Bool

	adminMu sync.Mutex

	mu          sync.RWMutex
	reg         *registry // nil until the records are loaded
	homeEpoch   uint64    // epoch of the home session the records were read in
	loadErr     error
	containers  map[string]*container // by ID, the home's included
	nat         NativeStore           // nil until the records are loaded
	defaultKind storage.BucketKind    // as configured ("" = automatic)
	// pending holds the bindings of native buckets being created (their
	// records are written last), so the native store can resolve them.
	pending map[string]native.Bucket
	// refreshing holds the native buckets with a drift refresh scheduled.
	refreshing map[string]time.Time

	ctx      context.Context
	cancel   context.CancelFunc
	spawnMu  sync.Mutex
	stopping bool
	wg       sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// hooks are test seams.
type hooks struct {
	// wrapRecords wraps the records store (fault injection).
	wrapRecords func(s3store.AuxStore) s3store.AuxStore
	// step, when set, runs before each step of a reconciliation or
	// migration; an error aborts it there (crash injection).
	step func(name string) error
	// retryMin is the first delay between attempts to load (0 = 500ms).
	retryMin time.Duration
	// monitorInterval is how often the home's session is checked (0 = 2s).
	monitorInterval time.Duration
	// driftInterval is the shortest interval between two refreshes of a
	// native bucket after drift reports (0 = 30s).
	driftInterval time.Duration
	// openNative replaces native.Open.
	openNative func(native.Options) (NativeStore, error)
}

var (
	_ storage.Backend                    = (*Backend)(nil)
	_ storage.ConditionalDeleter         = (*Backend)(nil)
	_ storage.NewerVersionsDeleter       = (*Backend)(nil)
	_ storage.CheckedDeleter             = (*Backend)(nil)
	_ storage.ConditionalMetaUpdater     = (*Backend)(nil)
	_ storage.ConfigViewer               = (*Backend)(nil)
	_ storage.UploadWalker               = (*Backend)(nil)
	_ storage.VersionImporter            = (*Backend)(nil)
	_ storage.CompressingImporter        = (*Backend)(nil)
	_ storage.CompressingBackend         = (*Backend)(nil)
	_ storage.EncryptionBackend          = (*Backend)(nil)
	_ storage.EncryptionProvidersBackend = (*Backend)(nil)
	_ storage.BucketCapabilities         = (*Backend)(nil)
	_ storage.BucketReleaser             = (*Backend)(nil)
	_ storage.HealthProber               = (*Backend)(nil)
	_ storage.KindReporter               = (*Backend)(nil)
	_ RemoteManager                      = (*Backend)(nil)
)

// Open returns the backing for opts. It fails on invalid options, a home
// location claimed by another use (storage.ErrInvalidTarget) and, without
// a home, when the local records cannot be opened (localstate.ErrLocked:
// another process uses them) or read; it does no remote I/O.
func Open(opts Options) (*Backend, error) { return open(opts, hooks{}) }

func open(opts Options, h hooks) (*Backend, error) {
	if opts.Claims == nil {
		return nil, errors.New("s3account: Options.Claims is required")
	}
	conn := opts.Conn
	if conn.Client == nil {
		c, err := s3client.New(s3client.Config{
			Endpoint: conn.Endpoint, Region: conn.Region, AccessKey: conn.AccessKey,
			SecretKey: conn.SecretKey, PathStyle: conn.PathStyle,
		})
		if err != nil {
			return nil, fmt.Errorf("s3account: %w", err)
		}
		conn.Client = c
	}
	if conn.Region == "" {
		conn.Region = conn.Client.Region()
	}
	if len(conn.KEK) != 0 && len(conn.KEK) != crypt.KeySize {
		return nil, fmt.Errorf("s3account: the encryption key has %d bytes, want %d", len(conn.KEK), crypt.KeySize)
	}
	switch opts.DefaultKind {
	case "", storage.BucketKindWrapped, storage.BucketKindNative:
	default:
		return nil, fmt.Errorf("s3account: unknown bucket kind %q", opts.DefaultKind)
	}
	if opts.Home != nil {
		if opts.Home.Bucket == "" {
			return nil, errors.New("s3account: the home needs a bucket")
		}
		if err := validPrefix(opts.Home.Prefix); err != nil {
			return nil, fmt.Errorf("s3account: home prefix: %w", err)
		}
	} else if opts.LocalDir == "" {
		return nil, errors.New("s3account: a backing without a home needs a local records directory")
	}
	if h.retryMin <= 0 {
		h.retryMin = 500 * time.Millisecond
	}
	if h.monitorInterval <= 0 {
		h.monitorInterval = 2 * time.Second
	}
	if h.driftInterval <= 0 {
		h.driftInterval = 30 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := &Backend{
		opts:        opts,
		name:        opts.Name,
		log:         log,
		hooks:       h,
		ready:       make(chan struct{}),
		containers:  map[string]*container{},
		defaultKind: opts.DefaultKind,
		pending:     map[string]native.Bucket{},
		refreshing:  map[string]time.Time{},
	}
	b.conn.Store(&conn)
	b.endpoint = conn.Endpoint
	if b.endpoint == "" {
		b.endpoint = conn.Client.Endpoint()
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	if opts.Home != nil {
		if err := b.openHome(&conn); err != nil {
			b.cancel()
			return nil, err
		}
		b.spawn(b.loadLoop)
		b.spawn(b.monitorHome)
		return b, nil
	}
	db, err := localstate.Open(opts.LocalDir, localstate.Options{Logger: log})
	if err != nil {
		b.cancel()
		return nil, fmt.Errorf("s3account %q: open the local records: %w", b.name, err)
	}
	b.local = db
	b.setRecords(db.Namespace(accountNamespace), db.Namespace(nativeNamespace))
	if err := b.load(b.ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// openHome claims the home and opens its store.
func (b *Backend) openHome(conn *s3store.Connection) error {
	h := *b.opts.Home
	rel, err := b.claim(h, RoleHome, "")
	if err != nil {
		return fmt.Errorf("s3account %q: home: %w", b.name, err)
	}
	st, err := openStore(b.storeOptions(conn, h.Bucket, h.Prefix, "", s3store.RoleHome, s3store.CreateIfNoData, "", "c-"+homeID))
	if err != nil {
		rel()
		return fmt.Errorf("s3account %q: home: %w", b.name, err)
	}
	b.home, b.homeRelease = st, rel
	b.containers[homeID] = &container{
		id: homeID, home: true, store: st, done: make(chan struct{}), conn: conn,
		rec: containerRecord{ID: homeID, Bucket: h.Bucket, Prefix: h.Prefix, Role: containerRoleHome},
	}
	b.setRecords(st.AuxState(accountNamespace), st.AuxState(nativeNamespace))
	return nil
}

// setRecords installs the record stores.
func (b *Backend) setRecords(records s3store.AuxStore, nat native.StateStore) {
	if b.hooks.wrapRecords != nil {
		records = b.hooks.wrapRecords(records)
	}
	b.records, b.natState = records, nat
}

// validPrefix checks a container or home prefix: "" or ending in "/",
// valid UTF-8 of at most 512 bytes, not starting with "/", without empty,
// "." or ".." segments (the rules of the configuration's prefixes).
func validPrefix(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasSuffix(p, "/") {
		return fmt.Errorf("prefix %q must end with /", p)
	}
	if len(p) > 512 || strings.HasPrefix(p, "/") || !utf8.ValidString(p) {
		return fmt.Errorf("prefix %q must be valid UTF-8 of at most 512 bytes, not starting with /", p)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("prefix %q has an empty, \".\" or \"..\" segment", p)
		}
	}
	return nil
}

// spawn runs fn in a goroutine Close waits for; false once Close began.
func (b *Backend) spawn(fn func()) bool {
	b.spawnMu.Lock()
	defer b.spawnMu.Unlock()
	if b.stopping {
		return false
	}
	b.wg.Go(fn)
	return true
}

// loadLoop loads the records once the home serves, retrying failures.
func (b *Backend) loadLoop() {
	delay := b.hooks.retryMin
	for {
		select {
		case <-b.home.Ready():
		case <-b.ctx.Done():
			return
		}
		err := b.load(b.ctx)
		if err == nil || b.ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		b.loadErr = err
		b.mu.Unlock()
		b.log.Warn("s3account: loading the backing's records failed; retrying", "backing", b.name, "err", err, "retry", delay)
		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return
		}
		delay = min(2*delay, 30*time.Second)
	}
}

// load reads and installs the records (once), reconciles the home and
// marks the backing ready.
func (b *Backend) load(ctx context.Context) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.closed.Load() {
		return b.unavailable()
	}
	b.mu.RLock()
	installed := b.reg != nil
	b.mu.RUnlock()
	if !installed {
		if err := b.readRecords(ctx); err != nil {
			return err
		}
	}
	b.mu.RLock()
	opened := b.nat != nil
	b.mu.RUnlock()
	if !opened {
		nat, err := b.openNative(b.conn.Load())
		if err != nil {
			return fmt.Errorf("s3account %q: open the native store: %w", b.name, err)
		}
		b.mu.Lock()
		b.nat = nat
		b.mu.Unlock()
	}
	if err := b.ensureSchema(ctx); err != nil {
		return err
	}
	if b.home != nil {
		if err := b.reconcileContainer(ctx, b.containerByID(homeID)); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.loadErr = nil
	b.mu.Unlock()
	b.readyOnce.Do(func() { close(b.ready) })
	b.log.Info("s3account: backing loaded", "backing", b.name, "records", b.recordsWhere())
	return nil
}

// readRecords reads the records and installs the registry (adminMu held).
func (b *Backend) readRecords(ctx context.Context) error {
	var epoch uint64
	if b.home != nil {
		epoch = b.home.Status().Epoch
	}
	recs, err := b.records.Records(ctx)
	if err != nil {
		return fmt.Errorf("s3account %q: read the records: %w", b.name, err)
	}
	reg, orphans, err := decodeRegistry(recs)
	if err != nil {
		return err
	}
	b.install(reg, epoch)
	for _, p := range orphans {
		if err := b.records.DeleteRecords(ctx, p); err != nil {
			b.log.Warn("s3account: delete orphaned configuration records", "backing", b.name, "prefix", p, "err", err)
		}
	}
	return nil
}

// install makes reg the registry (adminMu held): containers are opened,
// closed or kept to match its container records, and native buckets
// claimed. Conflicting claims are logged; the container or bucket is then
// unavailable.
func (b *Backend) install(reg *registry, epoch uint64) {
	b.mu.RLock()
	cur := b.reg
	curContainers := make(map[string]*container, len(b.containers))
	for id, c := range b.containers {
		curContainers[id] = c
	}
	b.mu.RUnlock()

	next := map[string]*container{}
	var opened, closing []*container
	if hc := curContainers[homeID]; hc != nil {
		next[homeID] = hc
	}
	for id, rec := range reg.containers {
		if id == homeID {
			continue
		}
		if c := curContainers[id]; c != nil && sameContainer(c.rec, rec) {
			next[id] = c // a label change is read from the registry
			continue
		}
		if c := curContainers[id]; c != nil {
			closing = append(closing, c) // changed location: closed first
			_ = closeContainer(c)
		}
		c := b.openContainer(rec, s3store.MustExist, rec.BackingID, nil)
		if c.failErr != nil {
			b.log.Error("s3account: a container cannot be used", "backing", b.name, "container", id,
				"location", c.locationString(), "err", c.failErr)
		}
		next[id] = c
		opened = append(opened, c)
	}
	for id, c := range curContainers {
		if next[id] == nil && !slices.Contains(closing, c) {
			_ = closeContainer(c)
		}
	}
	// Native claims: kept for unchanged bindings, taken for new ones.
	for name, e := range reg.buckets {
		if e.Kind != storage.BucketKindNative {
			continue
		}
		if cur != nil {
			if old := cur.buckets[name]; old != nil && old.Kind == storage.BucketKindNative &&
				strings.EqualFold(old.Remote, e.Remote) && (old.release != nil || old.claimErr != nil) {
				e.release, e.claimErr = old.release, old.claimErr
				old.release = nil
				continue
			}
		}
		rel, err := b.claim(Location{Bucket: e.Remote}, RoleNative, name)
		if err != nil {
			e.claimErr = err
			b.log.Error("s3account: a native bucket's remote bucket is used elsewhere; the bucket is unavailable",
				"backing", b.name, "bucket", name, "remote", e.Remote, "err", err)
			continue
		}
		e.release = rel
	}
	if cur != nil {
		for _, old := range cur.buckets {
			if old.release != nil {
				old.release()
				old.release = nil
			}
		}
	}
	b.mu.Lock()
	b.reg = reg
	b.containers = next
	b.homeEpoch = epoch
	b.mu.Unlock()
	for _, c := range opened {
		b.watchContainer(c)
	}
}

// sameContainer reports whether two records describe the same store.
func sameContainer(a, b containerRecord) bool {
	return a.Bucket == b.Bucket && a.Prefix == b.Prefix && a.Region == b.Region && a.BackingID == b.BackingID
}

// monitorHome re-reads the records when the home was reloaded (a new
// session after a fail-stop): another server may have changed them while
// this one did not own the home.
func (b *Backend) monitorHome() {
	t := time.NewTicker(b.hooks.monitorInterval)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		if !b.isReady() {
			continue
		}
		st := b.home.Status()
		b.mu.RLock()
		epoch := b.homeEpoch
		b.mu.RUnlock()
		if st.State != s3store.StateServing || st.Epoch == epoch {
			continue
		}
		b.reload(st.Epoch)
	}
}

// reload re-reads the records after the home was reloaded.
func (b *Backend) reload(epoch uint64) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	recs, err := b.records.Records(b.ctx)
	if err != nil {
		b.log.Warn("s3account: re-reading the records after the home was reloaded failed; retrying", "backing", b.name, "err", err)
		return
	}
	reg, _, err := decodeRegistry(recs)
	if err != nil {
		b.log.Error("s3account: the records changed into ones this build cannot read", "backing", b.name, "err", err)
		return
	}
	b.install(reg, epoch)
	if err := b.reconcileContainer(b.ctx, b.containerByID(homeID)); err != nil && b.ctx.Err() == nil {
		b.log.Warn("s3account: reconciling the home failed", "backing", b.name, "err", err)
	}
}

// isReady reports whether the records are loaded.
func (b *Backend) isReady() bool {
	select {
	case <-b.ready:
		return true
	default:
		return false
	}
}

// Ready returns a channel closed once the records are loaded (the home
// serves and the registry was read; at once for local records).
func (b *Backend) Ready() <-chan struct{} { return b.ready }

// WaitReady waits until the records are loaded (see Ready). It gives up
// early when loading failed (the home's load or reading the records; the
// backing keeps retrying in the background) and when ctx ends, returning
// the reason: an error wrapping storage.ErrBackingUnavailable and the
// cause (s3store.ErrInUse, s3store.ErrWrongKEK, a remote error, ...).
func (b *Backend) WaitReady(ctx context.Context) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-b.ready:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", b.unavailable(), ctx.Err())
		case <-tick.C:
			if b.closed.Load() {
				return b.unavailable()
			}
			b.mu.RLock()
			lerr := b.loadErr
			b.mu.RUnlock()
			if lerr != nil {
				return b.unavailable()
			}
			if b.home != nil {
				if st := b.home.Status(); st.State == s3store.StateFailed && st.Err != nil {
					return b.unavailable()
				}
			}
		}
	}
}

// WaitContainers waits until every container other than the home has
// settled: it serves, or its load failed for good (its buckets then answer
// storage.ErrBucketUnavailable). It returns the IDs of containers still
// loading when ctx ends (none once all settled). Containers load in the
// background after the records (see WaitReady); the server waits for them
// at startup so that the buckets they hold do not answer 503 for a moment
// after a restart.
func (b *Backend) WaitContainers(ctx context.Context) []string {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		loading := b.loadingContainers()
		if len(loading) == 0 || b.closed.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return loading
		case <-tick.C:
		}
	}
}

// loadingContainers returns the IDs of the containers (the home excluded)
// whose store is still loading, sorted.
func (b *Backend) loadingContainers() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var ids []string
	for id, c := range b.containers {
		if c.home || c.store == nil {
			continue
		}
		if st, _ := c.state(); st == "loading" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// serving returns nil when the backing can serve requests.
func (b *Backend) serving(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if b.closed.Load() || !b.isReady() {
		return b.unavailable()
	}
	return nil
}

// unavailable describes why the backing does not serve.
func (b *Backend) unavailable() error {
	if b.closed.Load() {
		return fmt.Errorf("%w: s3 backing %q is closed", storage.ErrBackingUnavailable, b.name)
	}
	b.mu.RLock()
	lerr := b.loadErr
	b.mu.RUnlock()
	if b.home != nil && lerr == nil {
		st := b.home.Status()
		if st.Err != nil {
			return fmt.Errorf("%w: s3 backing %q: home %s: %w", storage.ErrBackingUnavailable, b.name, b.home.Location(), st.Err)
		}
		return fmt.Errorf("%w: s3 backing %q: home %s is %v", storage.ErrBackingUnavailable, b.name, b.home.Location(), st.State)
	}
	if lerr != nil {
		return fmt.Errorf("%w: s3 backing %q: %w", storage.ErrBackingUnavailable, b.name, lerr)
	}
	return fmt.Errorf("%w: s3 backing %q is loading", storage.ErrBackingUnavailable, b.name)
}

// Status returns the backing's state for health reports.
func (b *Backend) Status() Status {
	st := Status{Ready: b.isReady()}
	if b.home != nil {
		st.Home = b.home.Status()
	}
	b.mu.RLock()
	lerr := b.loadErr
	b.mu.RUnlock()
	switch {
	case lerr != nil:
		st.Err = lerr
	case b.home != nil && st.Home.State != s3store.StateServing:
		st.Err = st.Home.Err
	}
	st.Containers = b.containerInfos()
	return st
}

// Location returns the home's URL ("s3://bucket/prefix") or, for a
// backing keeping its records locally, "local:" (config.S3Config's
// Location, used by the admin API and the web UI).
func (b *Backend) Location() string {
	if b.home != nil {
		return b.home.Location()
	}
	return "local:"
}

// recordsWhere is "home" or "local".
func (b *Backend) recordsWhere() string {
	if b.home != nil {
		return "home"
	}
	return "local"
}

// containerByID returns a container (nil if unknown).
func (b *Backend) containerByID(id string) *container {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.containers[id]
}

// --- kinds ---

// effectiveDefaultKind returns the kind of buckets created without one
// (mu held).
func (b *Backend) effectiveDefaultKind() storage.BucketKind {
	if b.defaultKind != "" {
		return b.defaultKind
	}
	if b.home != nil {
		return storage.BucketKindWrapped
	}
	return storage.BucketKindNative
}

// defaultContainer returns the ID of the default container ("" = none;
// mu held).
func (b *Backend) defaultContainer() string {
	if b.reg != nil && b.reg.defaultContainer != "" {
		return b.reg.defaultContainer
	}
	if b.home != nil {
		return homeID
	}
	return ""
}

// hasContainers reports whether wrapped buckets can be created (mu held).
func (b *Backend) hasContainers() bool {
	return b.home != nil || (b.reg != nil && len(b.reg.containers) > 0)
}

// BucketKinds implements storage.KindReporter.
func (b *Backend) BucketKinds() ([]storage.BucketKind, storage.BucketKind) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	def := b.effectiveDefaultKind()
	if b.hasContainers() {
		return []storage.BucketKind{storage.BucketKindWrapped, storage.BucketKindNative}, def
	}
	return []storage.BucketKind{storage.BucketKindNative}, def
}

// SetDefaultKind changes the kind of buckets created without one ("" =
// wrapped with a home, native without). Wrapped needs a home or a
// registered default container (storage.ErrInvalidTarget otherwise).
func (b *Backend) SetDefaultKind(k storage.BucketKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch k {
	case "", storage.BucketKindNative:
	case storage.BucketKindWrapped:
		if b.home == nil {
			ok := false
			if b.reg != nil && b.reg.defaultContainer != "" {
				_, ok = b.reg.containers[b.reg.defaultContainer]
			}
			if !ok {
				return fmt.Errorf("%w: s3 backing %q has no home and no default container for wrapped buckets", storage.ErrInvalidTarget, b.name)
			}
		}
	default:
		return fmt.Errorf("%w: unknown bucket kind %q", storage.ErrInvalidTarget, k)
	}
	b.defaultKind = k
	return nil
}

// --- health ---

// ProbeHealth implements storage.HealthProber: a backing with a home is
// healthy while the home serves; one without a home while the service
// answers (an access-denied answer counts: it came from the service).
func (b *Backend) ProbeHealth(ctx context.Context) error {
	if err := b.serving(ctx); err != nil {
		return err
	}
	if b.home != nil {
		if st := b.home.Status(); st.State != s3store.StateServing {
			return b.unavailableHome(st)
		}
		return nil
	}
	_, err := b.conn.Load().Client.ListBuckets(ctx, s3client.ListBucketsInput{MaxBuckets: 1})
	if err != nil && s3client.IsUnavailable(err) && !s3client.IsNotImplemented(err) {
		return fmt.Errorf("%w: s3 backing %q: %w", storage.ErrBackingUnavailable, b.name, err)
	}
	return nil
}

// unavailableHome is the error of a home that does not serve.
func (b *Backend) unavailableHome(st s3store.Status) error {
	if st.Err != nil {
		return fmt.Errorf("%w: s3 backing %q: home %s: %w", storage.ErrBackingUnavailable, b.name, b.home.Location(), st.Err)
	}
	return fmt.Errorf("%w: s3 backing %q: home %s is %v", storage.ErrBackingUnavailable, b.name, b.home.Location(), st.State)
}

// --- close ---

// Close stops the backing: background work ends, then the native store,
// the containers, the home and the local records are closed and every
// claim is released. It is idempotent.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		b.spawnMu.Lock()
		b.stopping = true
		b.spawnMu.Unlock()
		b.cancel()
		b.wg.Wait()
		b.adminMu.Lock()
		defer b.adminMu.Unlock()
		b.mu.Lock()
		nat, reg := b.nat, b.reg
		var cs []*container
		for _, c := range b.containers {
			if !c.home {
				cs = append(cs, c)
			}
		}
		b.mu.Unlock()
		var (
			errs  []error
			errMu sync.Mutex
			wg    sync.WaitGroup
		)
		if nat != nil {
			if err := nat.Close(); err != nil {
				errs = append(errs, fmt.Errorf("s3account %q: close the native store: %w", b.name, err))
			}
		}
		for _, c := range cs {
			wg.Go(func() {
				if err := closeContainer(c); err != nil {
					errMu.Lock()
					errs = append(errs, fmt.Errorf("s3account %q: close container %s: %w", b.name, c.id, err))
					errMu.Unlock()
				}
			})
		}
		wg.Wait()
		if b.home != nil {
			if err := b.home.Close(); err != nil {
				errs = append(errs, fmt.Errorf("s3account %q: close the home: %w", b.name, err))
			}
			b.homeRelease()
		}
		if b.local != nil {
			if err := b.local.Close(); err != nil {
				errs = append(errs, fmt.Errorf("s3account %q: close the local records: %w", b.name, err))
			}
		}
		if reg != nil {
			b.mu.Lock()
			for _, e := range reg.buckets {
				if e.release != nil {
					e.release()
					e.release = nil
				}
			}
			b.mu.Unlock()
		}
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}

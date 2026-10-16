package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/index"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// The Backend.
//
// Metadata lives in an in-memory index (internal/storage/index) per bucket,
// guarded by the bucket's lock. A mutation applies its effect to the index
// under the bucket's write lock, appends the matching journal record (so
// the LSN order of a bucket's records is the order of its mutations),
// releases the lock and waits until the record is durable before
// returning. Point reads wait for the LSN of the last record that touched
// the bucket; walks and listings may show writes still in flight.
//
// Open only validates options: the engine opens (remote I/O) in the
// background, retrying with backoff, and reloads after a fail-stop. Until
// the state is loaded every method answers storage.ErrBackingUnavailable.

// MaxPartNumber is the largest part number accepted (S3 allows 1..10000).
const MaxPartNumber = 10000

// Backend is the s3 storage backend.
type Backend struct {
	opts Options
	log  *slog.Logger
	keys keys
	eng  *Engine

	clock index.Clock
	now   func() time.Time

	conn atomic.Pointer[connection]

	mu      sync.RWMutex
	st      *state // nil while not loaded
	closed  bool
	loading *loader // only touched by the engine's Open (run goroutine)

	ready     chan struct{}
	readyOnce sync.Once
	failCh    chan error

	blobs   blobTracker
	pending pendingSet
	// removing holds blobs dereferenced by records not yet durable.
	removing pendingSet
	del      deleter

	keyMu      sync.RWMutex
	bucketKeys map[string]*keyEntry // by bucket ID
	restic     *restic.Runtime
	recovery   recoveryStore
	// auxNames holds the aux namespaces handed out by AuxState (string ->
	// true), whose staged blobs the orphan recovery reclaims even before
	// the state holds them.
	auxNames sync.Map
	// recoveryKept remembers, by key, the old unreferenced objects the
	// recovery found untagged or tagged by another backing, so later
	// passes skip them without a HEAD while they are unchanged.
	recoveryMu   sync.Mutex
	recoveryKept map[string]keptObject
	// recoveryHeads caps the HEAD requests of one recovery pass.
	recoveryHeads int

	// snapHook, when set (tests), runs after each bucket was written to a
	// snapshot.
	snapHook func(bucket string)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// connection is the swappable part of the options (PrepareConnection).
type connection struct {
	client   *s3client.Client
	kek      []byte
	partSize int64
	// The service as restic's s3 backend reaches it.
	endpoint, region, accessKey, secretKey string
	pathStyle                              bool
}

// state is the loaded metadata.
type state struct {
	mu      sync.RWMutex // guards buckets
	buckets map[string]*bucket

	// dropped is set (under mu) when the state is discarded after a
	// fail-stop.
	dropped bool

	auxMu  sync.Mutex // guards aux, auxLSN and stale
	aux    map[string]*auxNS
	auxLSN uint64
	stale  bool
}

func newState() *state {
	return &state{buckets: map[string]*bucket{}, aux: map[string]*auxNS{}}
}

// bucket is one bucket: its index guarded by mu.
type bucket struct {
	mu sync.RWMutex
	ix *index.Bucket[blobRef]
	// id names the bucket's blobs (data/<id>/); never reused.
	id string
	// lsn is the LSN of the last record that touched the bucket (mu).
	lsn uint64
	// keyed records that the bucket has a master key (keys/<id>.key): a
	// missing key object is then an error, never a reason to create one.
	keyed bool
	// deleted: the bucket was removed; stale: the state was dropped after
	// a fail-stop (operations holding the pointer must not continue).
	deleted, stale bool
}

func (bk *bucket) gone() bool { return bk.deleted || bk.stale }

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
)

// Open validates opts and returns the backend; the backing loads in the
// background (see WaitReady and Status). It fails only on invalid options.
func Open(opts Options) (*Backend, error) {
	if opts.Client == nil {
		return nil, errors.New("s3store: a client is required")
	}
	if opts.Bucket == "" {
		return nil, errors.New("s3store: a bucket is required")
	}
	if opts.Prefix != "" && opts.Prefix[len(opts.Prefix)-1] != '/' {
		return nil, fmt.Errorf("s3store: prefix %q must end with /", opts.Prefix)
	}
	if opts.PartSize < 0 {
		return nil, errors.New("s3store: negative part size")
	}
	if opts.OrphanRetention <= 0 {
		opts.OrphanRetention = DefaultOrphanRetention
	}
	if opts.RecoveryInterval == 0 {
		opts.RecoveryInterval = DefaultRecoveryInterval
	}
	if opts.retryMin <= 0 {
		opts.retryMin = 2 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	b := &Backend{
		opts:   opts,
		log:    opts.Logger,
		keys:   keys{prefix: opts.Prefix},
		now:    time.Now,
		ready:  make(chan struct{}),
		failCh: make(chan error, 1),
	}
	b.del.wake = make(chan struct{}, 1)
	b.conn.Store(&connection{
		client: opts.Client, kek: opts.KEK, partSize: opts.PartSize,
		endpoint: opts.Endpoint, region: opts.Region, accessKey: opts.AccessKey, secretKey: opts.SecretKey, pathStyle: opts.PathStyle,
	})
	b.restic = &restic.Runtime{Log: opts.Logger, TempDir: opts.TempDir}
	b.recovery.b = b
	b.recoveryHeads = maxRecoveryHeads
	eng, err := NewEngine(EngineOptions{
		Client:          opts.Client,
		Bucket:          opts.Bucket,
		Prefix:          opts.Prefix,
		Create:          opts.Create,
		ExpectBackingID: opts.ExpectBackingID,
		Role:            opts.Role,
		KEK:             opts.KEK,
		LeaseInterval:   opts.LeaseInterval,
		LeaseTTL:        opts.LeaseTTL,
		StallTimeout:    opts.StallTimeout,
		TempDir:         opts.TempDir,
		Logger:          opts.Logger,
		Load:            b.load,
		Snapshot:        b.snapshot,
		SnapshotPolicy:  opts.SnapshotPolicy,
		OnFail: func(err error) {
			select {
			case b.failCh <- err:
			default:
			}
		},
	})
	if err != nil {
		return nil, err
	}
	b.eng = eng
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(3)
	go b.run()
	go b.runDeleter()
	go b.runRecovery()
	return b, nil
}

func (b *Backend) client() *s3client.Client { return b.conn.Load().client }

// Location returns "s3://<bucket>/<prefix>".
func (b *Backend) Location() string { return "s3://" + b.opts.Bucket + "/" + b.opts.Prefix }

// Status returns the state of the metadata engine; Err explains why the
// backing is not serving.
func (b *Backend) Status() Status { return b.eng.State() }

// Engine returns the metadata engine (for diagnostics).
func (b *Backend) Engine() *Engine { return b.eng }

// Marker returns the backing's marker as last read or written (zero until
// a load has read it, which happens before the backing serves).
func (b *Backend) Marker() Marker { return b.eng.Marker() }

// ClearLegacyGateway clears the legacy gateway flag (Marker.Gateway) of the
// backing's marker with an If-Match rewrite, so concurrent marker updates
// are kept; it writes nothing when the flag is not set. It needs the
// marker to have been loaded (storage.ErrBackingUnavailable otherwise), and
// the marker must still name that backing (ErrMarkerMismatch, or
// ErrMarkerMissing). It is safe while the backing serves.
func (b *Backend) ClearLegacyGateway(ctx context.Context) error {
	return b.eng.clearLegacyGateway(ctx)
}

// Ready returns a channel closed once the backing was loaded for the first
// time.
func (b *Backend) Ready() <-chan struct{} { return b.ready }

// WaitReady waits until the backing is loaded and serving, or ctx ends
// (then returning the reason the backing is unavailable).
func (b *Backend) WaitReady(ctx context.Context) error {
	for {
		b.mu.RLock()
		ok, closed := b.st != nil, b.closed
		b.mu.RUnlock()
		if closed {
			return ErrClosed
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", ctx.Err(), b.unavailableErr())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// unavailableErr describes why the backing does not serve.
func (b *Backend) unavailableErr() error {
	st := b.eng.State()
	switch {
	case st.State == StateClosed:
		return ErrClosed
	case st.Err != nil:
		return fmt.Errorf("%w: s3 backing %q at %s: %w", storage.ErrBackingUnavailable, b.opts.Name, b.Location(), st.Err)
	}
	return fmt.Errorf("%w: s3 backing %q at %s is %v", storage.ErrBackingUnavailable, b.opts.Name, b.Location(), st.State)
}

// hardOpenError reports open failures that need an operator.
func hardOpenError(err error) bool {
	for _, h := range []error{ErrInUse, ErrWrongKEK, ErrNoConditionalWrites, ErrUnmanagedPrefix, ErrPrefixNotEmpty, ErrMarkerMismatch, ErrCorruptJournal} {
		if errors.Is(err, h) {
			return true
		}
	}
	return false
}

// run opens the engine, retrying with backoff, and reloads after every
// fail-stop.
func (b *Backend) run() {
	defer b.wg.Done()
	delay := b.opts.retryMin
	for {
		err := b.eng.Open(b.ctx)
		if b.ctx.Err() != nil {
			return
		}
		if err == nil {
			b.install()
			delay = b.opts.retryMin
			select {
			case <-b.ctx.Done():
				return
			case ferr := <-b.failCh:
				b.dropState()
				b.log.Error("s3store: backing stopped serving; reloading", "backing", b.opts.Name, "location", b.Location(), "err", ferr)
			}
			continue
		}
		if hardOpenError(err) {
			b.log.Error("s3store: cannot open backing", "backing", b.opts.Name, "location", b.Location(), "err", err, "retry", delay)
		} else {
			b.log.Warn("s3store: backing unavailable", "backing", b.opts.Name, "location", b.Location(), "err", err, "retry", delay)
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// install makes the loaded state current.
func (b *Backend) install() {
	l := b.loading
	b.loading = nil
	b.mu.Lock()
	if l != nil {
		b.st = l.st
	}
	b.mu.Unlock()
	b.readyOnce.Do(func() { close(b.ready) })
}

// dropState discards the state after a fail-stop; operations holding
// bucket pointers fail.
func (b *Backend) dropState() {
	b.mu.Lock()
	st := b.st
	b.st = nil
	b.mu.Unlock()
	// Removals that did not become durable are void: the reloaded state
	// references what the journal says.
	b.removing.clear()
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dropped = true
	for _, bk := range st.buckets {
		bk.mu.Lock()
		bk.stale = true
		bk.mu.Unlock()
	}
	st.auxMu.Lock()
	st.stale = true
	st.auxMu.Unlock()
}

// Close stops the backend: queued journal records are written, the lease
// is released and background work ends. Open readers keep working until
// closed. Close is idempotent.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	// The engine drains the journal first: durable callbacks may still
	// queue blob deletes, which the deleter flushes when it stops.
	err := b.eng.Close()
	b.cancel()
	b.wg.Wait()
	// Restores of canceled restic reads finish in the background (and
	// remove their temporary files); wait for them too.
	if b.restic != nil {
		b.restic.Wait()
	}
	b.mu.Lock()
	st := b.st
	b.st = nil
	b.mu.Unlock()
	if st != nil {
		st.mu.RLock()
		for _, bk := range st.buckets {
			bk.mu.Lock()
			bk.stale = true
			bk.mu.Unlock()
		}
		st.mu.RUnlock()
	}
	return err
}

// PrepareConnection verifies that conn reaches the same backing (its marker
// has the ID of the loaded backing, or Options.ExpectBackingID before the
// first load) and, when it has a KEK, that the KEK matches the backing's.
// commit makes the backend use conn for every later request.
//
// Preparing has no persistent effect, so a change abandoned after it
// leaves the backing as it was: a KEK for a backing whose marker has no
// check value yet only has to open the key objects that exist; its check
// value is recorded when it first wraps a new key (recordKEK).
func (b *Backend) PrepareConnection(ctx context.Context, conn Connection) (commit func(), err error) {
	client := conn.Client
	if client == nil {
		client, err = s3client.New(s3client.Config{
			Endpoint: conn.Endpoint, Region: conn.Region, AccessKey: conn.AccessKey,
			SecretKey: conn.SecretKey, PathStyle: conn.PathStyle,
		})
		if err != nil {
			return nil, err
		}
	}
	if conn.Endpoint == "" && conn.AccessKey == "" {
		// A bare client: keep the service description restic uses.
		cur := b.conn.Load()
		conn.Endpoint, conn.Region, conn.AccessKey, conn.SecretKey, conn.PathStyle = cur.endpoint, cur.region, cur.accessKey, cur.secretKey, cur.pathStyle
	}
	if len(conn.KEK) != 0 && len(conn.KEK) != crypt.KeySize {
		return nil, fmt.Errorf("s3store: KEK has %d bytes, want %d", len(conn.KEK), crypt.KeySize)
	}
	m, _, err := ReadMarker(ctx, client, b.opts.Bucket, b.opts.Prefix)
	if err != nil {
		if s3client.IsNotFound(err) {
			return nil, fmt.Errorf("%w (%s, with the new connection)", ErrMarkerMissing, b.Location())
		}
		return nil, err
	}
	want := b.opts.ExpectBackingID
	if cur := b.eng.Marker(); cur.BackingID != "" {
		want = cur.BackingID
	}
	if err := m.validate(want); err != nil {
		return nil, fmt.Errorf("%w (with the new connection)", err)
	}
	if len(conn.KEK) > 0 {
		if _, err := m.CheckKEK(conn.KEK); err != nil {
			return nil, err
		}
		if m.KEKCheck == "" {
			// The backing has no check value yet: the KEK must open the
			// keys it may already have wrapped. It becomes the backing's
			// KEK once committed and recorded in the marker before it
			// wraps a new key.
			if err := b.verifyKEK(ctx, client, m.BackingID, conn.KEK); err != nil {
				return nil, err
			}
		}
	}
	return func() {
		b.conn.Store(&connection{
			client: client, kek: conn.KEK, partSize: conn.PartSize,
			endpoint: conn.Endpoint, region: conn.Region, accessKey: conn.AccessKey, secretKey: conn.SecretKey, pathStyle: conn.PathStyle,
		})
		b.eng.SetConnection(client, conn.KEK)
		b.eng.noteMarker(m)
	}, nil
}

// --- lookups ---

func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// current returns the loaded state.
func (b *Backend) current(ctx context.Context) (*state, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	b.mu.RLock()
	st, closed := b.st, b.closed
	b.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if st == nil {
		return nil, b.unavailableErr()
	}
	return st, nil
}

// lookup returns the bucket named name without locking it.
func (b *Backend) lookup(ctx context.Context, name string) (*bucket, error) {
	st, err := b.current(ctx)
	if err != nil {
		return nil, err
	}
	st.mu.RLock()
	bk, ok := st.buckets[name]
	st.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	return bk, nil
}

// goneErr is the error for a bucket pointer found deleted or stale.
func (b *Backend) goneErr(bk *bucket) error {
	if bk.stale {
		b.mu.RLock()
		closed := b.closed
		b.mu.RUnlock()
		if closed {
			return ErrClosed
		}
		return b.unavailableErr()
	}
	return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bk.ix.Info.Name)
}

// rlockBucket returns the bucket read-locked.
func (b *Backend) rlockBucket(ctx context.Context, name string) (*bucket, error) {
	bk, err := b.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	bk.mu.RLock()
	if bk.gone() {
		err := b.goneErr(bk)
		bk.mu.RUnlock()
		return nil, err
	}
	return bk, nil
}

// lockBucket returns the bucket write-locked.
func (b *Backend) lockBucket(ctx context.Context, name string) (*bucket, error) {
	bk, err := b.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	return bk, b.relock(ctx, bk)
}

// relock write-locks a bucket pointer obtained earlier and verifies it is
// still current.
func (b *Backend) relock(ctx context.Context, bk *bucket) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	bk.mu.Lock()
	if bk.gone() {
		err := b.goneErr(bk)
		bk.mu.Unlock()
		return err
	}
	return nil
}

// newBucket returns a bucket index stamped by the backend's clock.
func (b *Backend) newBucket(info storage.BucketInfo, id string) *bucket {
	return &bucket{ix: index.NewBucket[blobRef](info, b.nextStamp), id: id}
}

func (b *Backend) nextStamp() (uint64, time.Time) { return b.clock.Next(b.now()) }

// --- journaling ---

// appendLocked journals rec for bk (write-locked) and records its LSN on
// the bucket; onDurable runs once the record is durable.
func (b *Backend) appendLocked(bk *bucket, rec *record, onDurable func()) <-chan error {
	lsn, done := b.appendRecord(rec, onDurable)
	if lsn > bk.lsn {
		bk.lsn = lsn
	}
	return done
}

func (b *Backend) appendRecord(rec *record, onDurable func()) (uint64, <-chan error) {
	data, err := json.Marshal(rec)
	if err != nil {
		// The mutation is applied in memory but cannot be journaled: stop
		// serving, so the state is reloaded from the journal.
		err = fmt.Errorf("s3store: encode journal record: %w", err)
		b.eng.failCurrent(err)
		ch := make(chan error, 1)
		ch <- unavailable(err)
		return 0, ch
	}
	return b.eng.AppendWith(data, onDurable)
}

// await waits for a journal append.
//
// It deliberately ignores ctx: the record is durable or not regardless of
// the caller, so a cancelled caller must still learn which. The wait is
// bounded by the engine (a stalled journal fail-stops after
// Options.StallTimeout and fails every waiter).
func await(_ context.Context, done <-chan error) error {
	return <-done
}

// waitRead waits until the record with LSN lsn (read under a bucket's
// lock) is durable, so a point read never returns state that could still
// be lost.
func (b *Backend) waitRead(ctx context.Context, lsn uint64) error {
	if lsn == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return b.eng.WaitDurable(ctx, lsn)
}

// validBucketName reports whether the backend can store name.
func validBucketName(name string) error {
	if name == "" || !utf8.ValidString(name) {
		return fmt.Errorf("%w: bucket %q", storage.ErrInvalidName, name)
	}
	return nil
}

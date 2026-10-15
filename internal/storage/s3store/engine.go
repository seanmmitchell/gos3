package s3store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// The metadata engine.
//
// An Engine owns the metadata area (meta/) of one backing prefix: the
// marker, the owner lease, the journal and the snapshots. It is generic
// over its payloads: records and snapshot chunks are opaque JSON values of
// the caller (the backend's index), which the engine stores durably and
// delivers back, in order, when the backing is opened again.
//
// Lifecycle: NewEngine validates options; Open (blocking, remote I/O)
// verifies the marker, probes conditional writes, takes the lease, loads
// the newest valid snapshot and the journal through EngineOptions.Load,
// seals the epochs of previous owners and starts serving. Append and
// AppendWith journal records; a record is durable once the batch holding
// it was written. A fencing or a stalled journal fail-stops the engine
// (State() == StateFailed); Open may then be called again to reload, and
// Close releases the lease.
//
// Durability and fencing. Each owner of the prefix writes its own journal
// epoch (the lease epoch). Batches are written strictly one after the
// other into consecutive slots meta/journal/<epoch>/<seq> with
// If-None-Match, so a slot is written at most once. A new owner first
// writes a seal object into the next free slot of every earlier epoch that
// is not sealed yet; a previous owner that is still running (a "zombie"
// whose lease expired) then finds its next slot taken (412), reads it, sees
// the seal and stops (ErrFenced). Its batches written before the seal were
// read and applied by the new owner, so nothing acknowledged is lost.
// Seals are never deleted, and the highest slot of every epoch is never
// garbage-collected, so a later owner always seals the right slot.
//
// Invariant: when an Open has sealed epoch e, every epoch below e is
// sealed (epochs are sealed in ascending order before anything else is
// written). A snapshot written in epoch S therefore covers every record of
// the epochs below S, and loading only replays epochs >= S.

// Defaults of EngineOptions.
const (
	DefaultLeaseInterval       = 10 * time.Second
	DefaultLeaseTTL            = 30 * time.Second
	DefaultJournalStallTimeout = 2 * time.Minute
)

// Journal batch limits: a batch holds at most this many records or (unless
// a single record is larger) bytes of records.
const (
	maxBatchBytes   = 4 << 20
	maxBatchRecords = 10000
)

// EngineOptions configure an Engine.
type EngineOptions struct {
	// Client talks to the remote service. Required.
	Client *s3client.Client
	// Bucket and Prefix locate the backing ("" or ending with "/").
	Bucket string
	Prefix string
	// Create says when Open may create the marker (see CreateMode). With
	// CreateIfEmpty, a marker the engine did not create is refused
	// (ErrPrefixNotEmpty) unless it is ExpectBackingID's.
	Create CreateMode
	// ExpectBackingID, when set, refuses a marker with another backing ID
	// (ErrMarkerMismatch) and a missing marker (ErrMarkerMissing). Without
	// it, every Open after the first that read a marker expects that
	// marker's backing.
	ExpectBackingID string
	// Role is stored in a marker this engine creates (Marker.Role).
	Role string
	// KEK is the key-encryption key (nil: none); see Marker.CheckKEK.
	KEK []byte
	// LeaseInterval is how often the lease is renewed; LeaseTTL is the age
	// (by the service's clock) after which an unrenewed lease may be taken
	// over. LeaseTTL must be at least twice LeaseInterval.
	LeaseInterval time.Duration
	LeaseTTL      time.Duration
	// StallTimeout bounds how long one journal batch may stay unwritten
	// before the engine fail-stops (0 = DefaultJournalStallTimeout).
	StallTimeout time.Duration
	// TempDir holds spooled snapshots ("" = os.TempDir()).
	TempDir string
	Logger  *slog.Logger
	// Load receives the loaded state during Open: LoadBegin (reset any
	// previously loaded state), then optionally LoadSnapshot and its
	// LoadChunk events, then LoadRecord for every journal record after the
	// snapshot's StartLSN, in LSN order. An error aborts Open. Required.
	Load func(LoadEvent) error
	// Snapshot writes a snapshot of the caller's state (see
	// SnapshotWriter). It runs in the background, one at a time,
	// concurrently with Append. nil disables snapshots.
	Snapshot func(ctx context.Context, w SnapshotWriter) error
	// SnapshotPolicy decides when snapshots are taken.
	SnapshotPolicy SnapshotPolicy
	// OnFail, when set, is called (in its own goroutine) once each time the
	// serving engine fail-stops, with the cause. The owner typically
	// schedules a new Open.
	OnFail func(err error)
}

// SnapshotPolicy is the snapshot cadence. A snapshot is due when, since the
// last one, more than MaxJournalBytes of journal or more than MaxBatches
// batches were written, or MaxAge passed with at least one batch written.
// Close takes a snapshot when more than CloseBatches batches were written
// since the last one. Zero fields take the defaults; negative ones disable
// that trigger.
type SnapshotPolicy struct {
	MaxJournalBytes int64         // default 64 MiB
	MaxBatches      int           // default 10,000
	MaxAge          time.Duration // default 6 h
	CloseBatches    int           // default 500
}

func (p SnapshotPolicy) withDefaults() SnapshotPolicy {
	if p.MaxJournalBytes == 0 {
		p.MaxJournalBytes = 64 << 20
	}
	if p.MaxBatches == 0 {
		p.MaxBatches = 10000
	}
	if p.MaxAge == 0 {
		p.MaxAge = 6 * time.Hour
	}
	if p.CloseBatches == 0 {
		p.CloseBatches = 500
	}
	return p
}

// LoadEventKind tells which field of a LoadEvent is set.
type LoadEventKind int

const (
	// LoadBegin starts a load: discard any state loaded before.
	LoadBegin LoadEventKind = iota
	// LoadSnapshot carries the header of the snapshot being loaded.
	LoadSnapshot
	// LoadChunk carries one snapshot chunk.
	LoadChunk
	// LoadRecord carries one journal record.
	LoadRecord
)

// LoadEvent is one step of loading (see EngineOptions.Load).
type LoadEvent struct {
	Kind   LoadEventKind
	Header SnapshotHeader // LoadSnapshot
	Chunk  Chunk          // LoadChunk
	Record Record         // LoadRecord
}

// Record is a journal record.
type Record struct {
	LSN     uint64
	Payload json.RawMessage
}

// Chunk is a piece of a snapshot: the state of one scope as of LSN (every
// record with a higher LSN that touches the scope is replayed on top).
type Chunk struct {
	Scope   string
	LSN     uint64
	Payload json.RawMessage
}

// State is the lifecycle state of an Engine.
type State int

const (
	StateNew State = iota
	StateLoading
	StateServing
	StateFailed
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateLoading:
		return "loading"
	case StateServing:
		return "serving"
	case StateFailed:
		return "failed"
	case StateClosed:
		return "closed"
	}
	return "State(" + strconv.Itoa(int(s)) + ")"
}

// Status is a snapshot of the engine's state.
type Status struct {
	State State
	// Err is why the last Open failed or the engine fail-stopped.
	Err        error
	Epoch      uint64
	DurableLSN uint64
	LastLSN    uint64
}

// Engine is the metadata engine of one backing prefix. Its methods are
// safe for concurrent use.
type Engine struct {
	opts   EngineOptions
	policy SnapshotPolicy
	keys   keys

	// Test seams.
	now       func() time.Time // local clock: timeouts and intervals only
	retryBase time.Duration    // first journal retry backoff
	retryMax  time.Duration
	leasePoll time.Duration // lease polling during Open

	conn atomic.Pointer[engineConn] // client and KEK (SetConnection)
	// refusals counts refused appends. The caller may have applied the
	// refused mutation to its state already, so a snapshot during which
	// one was refused is discarded.
	refusals atomic.Uint64

	openMu sync.Mutex // serializes Open and the end of Close

	mu         sync.Mutex
	state      State
	err        error
	marker     Marker
	kek        KEKState
	attempted  map[string]bool // backing IDs createMarker tried to create
	sess       *session
	token      leaseToken // the last lease held, for an immediate re-take
	openCancel context.CancelFunc
	closing    bool
}

// NewEngine validates opts and returns an engine in StateNew. It does no
// remote I/O.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.Client == nil {
		return nil, errors.New("s3store: engine needs a client")
	}
	if opts.Bucket == "" {
		return nil, errors.New("s3store: engine needs a bucket")
	}
	if opts.Prefix != "" && opts.Prefix[len(opts.Prefix)-1] != '/' {
		return nil, fmt.Errorf("s3store: prefix %q must end with /", opts.Prefix)
	}
	if opts.Load == nil {
		return nil, errors.New("s3store: engine needs a Load function")
	}
	if len(opts.KEK) != 0 && len(opts.KEK) != 32 {
		return nil, fmt.Errorf("s3store: KEK has %d bytes, want 32", len(opts.KEK))
	}
	switch opts.Create {
	case CreateIfNoData, CreateIfEmpty, MustExist:
	default:
		return nil, fmt.Errorf("s3store: invalid create mode %v", opts.Create)
	}
	if opts.LeaseInterval <= 0 {
		opts.LeaseInterval = DefaultLeaseInterval
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = max(DefaultLeaseTTL, 3*opts.LeaseInterval)
	}
	if opts.LeaseTTL < 2*opts.LeaseInterval {
		return nil, fmt.Errorf("s3store: lease TTL %v must be at least twice the lease interval %v", opts.LeaseTTL, opts.LeaseInterval)
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = DefaultJournalStallTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		opts:      opts,
		policy:    opts.SnapshotPolicy.withDefaults(),
		keys:      keys{prefix: opts.Prefix},
		now:       time.Now,
		retryBase: 100 * time.Millisecond,
		retryMax:  5 * time.Second,
		leasePoll: opts.LeaseInterval / 2,
	}
	e.conn.Store(&engineConn{client: opts.Client, kek: opts.KEK})
	return e, nil
}

// engineConn is the connection an engine uses; SetConnection replaces it.
type engineConn struct {
	client *s3client.Client
	kek    []byte
}

func (e *Engine) client() *s3client.Client { return e.conn.Load().client }
func (e *Engine) currentKEK() []byte       { return e.conn.Load().kek }

// noteMarker records a marker read (or updated) outside Open, with the
// KEK state of the current KEK.
func (e *Engine) noteMarker(m Marker) {
	state, _ := m.CheckKEK(e.currentKEK())
	e.mu.Lock()
	defer e.mu.Unlock()
	e.marker, e.kek = m, state
}

// SetConnection makes the engine use client (which must reach the same
// bucket and prefix) and kek from now on, e.g. after new credentials were
// verified. Requests already running finish with the previous client.
func (e *Engine) SetConnection(client *s3client.Client, kek []byte) {
	e.conn.Store(&engineConn{client: client, kek: kek})
}

// OpenEngine is NewEngine followed by Open.
func OpenEngine(ctx context.Context, opts EngineOptions) (*Engine, error) {
	e, err := NewEngine(opts)
	if err != nil {
		return nil, err
	}
	if err := e.Open(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) remote() remote       { return remote{c: e.client(), bucket: e.opts.Bucket} }
func (e *Engine) logger() *slog.Logger { return e.opts.Logger }

// location returns "s3://<bucket>/<prefix>" (errors).
func (e *Engine) location() string { return "s3://" + e.opts.Bucket + "/" + e.opts.Prefix }

// Marker returns the backing's marker (zero before the first Open got it).
func (e *Engine) Marker() Marker {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.marker
}

// KEKState reports how the configured KEK relates to the backing (valid
// after a successful Open).
func (e *Engine) KEKState() KEKState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kek
}

// State returns the engine's state.
func (e *Engine) State() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{State: e.state, Err: e.err}
	if s := e.sess; s != nil {
		st.Epoch, st.DurableLSN, st.LastLSN = s.epoch, s.durable, s.lastLSN
	}
	return st
}

// DurableLSN returns the highest LSN whose record is durable (all lower
// LSNs are durable too).
func (e *Engine) DurableLSN() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil {
		return 0
	}
	return e.sess.durable
}

// LastLSN returns the highest LSN assigned so far (durable or not). A
// snapshot chunk captured under the lock that orders the caller's appends
// to that scope uses it as the chunk's LSN.
func (e *Engine) LastLSN() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil {
		return 0
	}
	return e.sess.lastLSN
}

// Open loads the backing and starts serving (see the package comment on the
// engine). It may be called on a new engine or after a failure (StateNew or
// StateFailed); it blocks until the engine serves or the open failed, and
// a failed open leaves the engine in StateFailed.
//
// Hard errors: ErrInUse (another live server holds the lease),
// ErrWrongKEK, ErrUnmanagedPrefix, ErrPrefixNotEmpty, ErrMarkerMismatch
// (and ErrMarkerMissing, which wraps it), ErrNoConditionalWrites,
// ErrCorruptJournal and errors of Load. Remote failures wrap
// storage.ErrBackingUnavailable.
func (e *Engine) Open(ctx context.Context) error {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	e.mu.Lock()
	switch {
	case e.state == StateClosed || e.closing:
		e.mu.Unlock()
		return ErrClosed
	case e.state != StateNew && e.state != StateFailed:
		e.mu.Unlock()
		return fmt.Errorf("s3store: engine is %v", e.state)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	old := e.sess
	e.state, e.err, e.sess, e.openCancel = StateLoading, nil, nil, cancel
	e.mu.Unlock()
	if old != nil {
		// The failed session's goroutines are stopping; wait for them and
		// keep its lease token for an immediate re-take.
		old.abandon(false)
	}

	s, err := e.open(ctx)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.openCancel = nil
	switch {
	case err == nil && e.closing:
		err = ErrClosed
	case err == nil && s.failed != nil:
		// The lease was lost right at the end of loading.
		err = s.failed
	}
	if err != nil {
		if s != nil {
			e.mu.Unlock()
			s.abandon(true)
			e.mu.Lock()
		}
		if e.closing {
			e.state = StateClosed
		} else {
			e.state, e.err = StateFailed, err
		}
		return err
	}
	e.state, e.sess = StateServing, s
	s.startWriter()
	e.logger().Info("s3store: metadata engine serving", "bucket", e.opts.Bucket, "prefix", e.opts.Prefix,
		"epoch", s.epoch, "lsn", s.lastLSN)
	return nil
}

// open does the remote work of Open. On error a non-nil session (whose
// lease was taken) must be abandoned by the caller.
func (e *Engine) open(ctx context.Context) (*session, error) {
	m, kek, err := e.openMarker(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.marker, e.kek = m, kek
	e.mu.Unlock()

	s, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	// Open's I/O stops when either Open's context ends or the session
	// fail-stops (lease lost while loading).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	fail := func(err error) (*session, error) {
		if f := s.failure(); f != nil {
			err = f
		}
		return s, err
	}
	if err := e.load(ctx, s); err != nil {
		return fail(err)
	}
	if err := s.writeStartBatch(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

// Append journals rec (one JSON value) and returns its LSN and a channel
// that receives exactly one value: nil once the record is durable, or the
// error that keeps it from becoming durable (wrapping
// storage.ErrBackingUnavailable, and ErrFenced after a fencing). LSNs are
// assigned in call order. The channel is closed after the value.
func (e *Engine) Append(rec json.RawMessage) (uint64, <-chan error) {
	return e.AppendWith(rec, nil)
}

// AppendWith is Append that also calls onDurable (from the journal
// goroutine, in LSN order, before the done channel is signalled) once the
// record is durable. onDurable must not block or call back into the engine
// for long.
func (e *Engine) AppendWith(rec json.RawMessage, onDurable func()) (uint64, <-chan error) {
	done := make(chan error, 1)
	line, err := compactRecord(rec)
	if err != nil {
		done <- err
		close(done)
		return 0, done
	}
	e.mu.Lock()
	s := e.sess
	if err := e.appendErrLocked(); err != nil {
		e.refusals.Add(1)
		e.mu.Unlock()
		done <- err
		close(done)
		return 0, done
	}
	s.lastLSN++
	p := &pending{lsn: s.lastLSN, onDurable: onDurable, done: done}
	p.line = append(strconv.AppendUint(append(make([]byte, 0, len(line)+32), `{"lsn":`...), p.lsn, 10), `,"r":`...)
	p.line = append(append(p.line, line...), "}\n"...)
	s.queue = append(s.queue, p)
	e.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return p.lsn, done
}

// appendErrLocked returns why appends are refused now (nil: accepted).
func (e *Engine) appendErrLocked() error {
	s := e.sess
	switch {
	case e.state == StateClosed || e.closing:
		return ErrClosed
	case s != nil && s.failed != nil:
		return s.failed
	case e.state == StateFailed && e.err != nil:
		return unavailable(e.err)
	case e.state != StateServing || s == nil:
		return fmt.Errorf("s3store: metadata engine is %v: %w", e.state, storage.ErrBackingUnavailable)
	}
	return nil
}

// unavailable wraps err with storage.ErrBackingUnavailable unless it
// already does.
func unavailable(err error) error {
	if errors.Is(err, storage.ErrBackingUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, err)
}

// WaitDurable waits until the record with LSN lsn of the serving engine is
// durable, the engine fails or ctx ends.
func (e *Engine) WaitDurable(ctx context.Context, lsn uint64) error {
	for {
		e.mu.Lock()
		s := e.sess
		if s != nil && s.durable >= lsn {
			e.mu.Unlock()
			return nil
		}
		if err := e.appendErrLocked(); err != nil {
			e.mu.Unlock()
			return err
		}
		ch := s.durableCh
		e.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Snapshot takes a snapshot now (waiting for one already running) and
// garbage-collects old snapshots and journal batches.
func (e *Engine) Snapshot(ctx context.Context) error {
	e.mu.Lock()
	s := e.sess
	err := e.appendErrLocked()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if e.opts.Snapshot == nil {
		return errors.New("s3store: no snapshot function configured")
	}
	return s.snapshot(ctx)
}

// Close stops the engine: it waits for queued records to be written, takes
// a final snapshot when the policy asks for one, and releases the lease so
// another server can take over at once. Records appended after Close
// started fail with ErrClosed.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.state == StateClosed || e.closing {
		e.mu.Unlock()
		return nil
	}
	e.closing = true
	if e.openCancel != nil {
		e.openCancel()
	}
	s := e.sess
	e.mu.Unlock()

	if s != nil {
		s.close()
	}
	e.openMu.Lock()
	defer e.openMu.Unlock()
	e.mu.Lock()
	e.state = StateClosed
	e.mu.Unlock()
	return nil
}

// failCurrent fail-stops the serving session, if any, with err. The
// backend uses it when its in-memory state may have diverged from the
// journal (a record that could not be encoded), so that it reloads.
func (e *Engine) failCurrent(err error) {
	e.mu.Lock()
	s := e.sess
	e.mu.Unlock()
	if s != nil {
		e.failSession(s, err)
	}
}

// failSession fail-stops the serving session s with err: queued records
// fail, the lease is no longer renewed (it is not released: the token
// allows this engine an immediate re-take) and OnFail is called.
func (e *Engine) failSession(s *session, err error) {
	err = unavailable(err)
	e.mu.Lock()
	if s.failed != nil {
		e.mu.Unlock()
		return
	}
	s.failed = err
	queue := s.queue
	s.queue = nil
	close(s.durableCh)
	notify := false
	if e.sess == s && e.state == StateServing && !e.closing {
		e.state, e.err = StateFailed, err
		notify = true
	}
	e.mu.Unlock()
	s.cancel()
	for _, p := range queue {
		p.finish(err)
	}
	if notify {
		e.logger().Error("s3store: metadata engine stopped", "bucket", e.opts.Bucket, "prefix", e.opts.Prefix, "err", err)
		if e.opts.OnFail != nil {
			go e.opts.OnFail(err)
		}
	}
}

// pending is a record waiting for its batch.
type pending struct {
	lsn       uint64
	line      []byte // {"lsn":N,"r":...}\n
	onDurable func()
	done      chan error
}

func (p *pending) finish(err error) {
	p.done <- err
	close(p.done)
}

// session is one serving period of an engine: one lease epoch with its
// journal writer, lease keeper and snapshots. Fields marked (mu) are
// guarded by e.mu.
type session struct {
	e      *Engine
	ctx    context.Context // ends when the session stops
	cancel context.CancelFunc
	epoch  uint64
	owner  string

	lease       leaseState // lease goroutine (and Open) only
	leasePaused atomic.Bool
	leaseDone   chan struct{}

	lastLSN   uint64        // (mu) highest assigned
	durable   uint64        // (mu) highest durable
	nextSeq   uint64        // (mu) next journal slot of this epoch
	queue     []*pending    // (mu)
	closing   bool          // (mu) drain and stop the writer
	failed    error         // (mu) set once by failSession
	durableCh chan struct{} // (mu) closed when durable advances or on failure
	wake      chan struct{}
	writerOn  bool          // (mu) writer goroutine started
	writerEnd chan struct{} // closed when the writer exits

	journal journalState // (mu) catalog and snapshot statistics
	snapMu  sync.Mutex   // one snapshot at a time
	snapBg  sync.WaitGroup
}

func (e *Engine) newSession(epoch uint64, owner string) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		e: e, ctx: ctx, cancel: cancel, epoch: epoch, owner: owner,
		durableCh: make(chan struct{}),
		wake:      make(chan struct{}, 1),
		leaseDone: make(chan struct{}),
		writerEnd: make(chan struct{}),
		nextSeq:   1,
	}
}

// failure returns the session's fail-stop error, if any.
func (s *session) failure() error {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	return s.failed
}

// startWriter starts the journal writer (e.mu held).
func (s *session) startWriter() {
	s.writerOn = true
	go s.writer()
}

// close drains the journal, takes the closing snapshot and releases the
// lease.
func (s *session) close() {
	e := s.e
	e.mu.Lock()
	s.closing = true
	writerOn := s.writerOn
	e.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if writerOn {
		<-s.writerEnd
	}
	s.snapBg.Wait()
	e.mu.Lock()
	due := s.failed == nil && writerOn && e.opts.Snapshot != nil &&
		e.policy.CloseBatches >= 0 && s.journal.batches > e.policy.CloseBatches
	e.mu.Unlock()
	if due {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
		if err := s.snapshot(ctx); err != nil {
			e.logger().Warn("s3store: closing snapshot failed", "err", err)
		}
		cancel()
	}
	s.abandon(true)
}

// abandon stops the session's goroutines and, with release, gives the
// lease up. Queued records fail with ErrClosed.
func (s *session) abandon(release bool) {
	e := s.e
	e.mu.Lock()
	if s.failed == nil {
		s.failed = ErrClosed
		close(s.durableCh)
	}
	queue := s.queue
	s.queue = nil
	writerOn := s.writerOn
	e.mu.Unlock()
	for _, p := range queue {
		p.finish(ErrClosed)
	}
	s.cancel()
	<-s.leaseDone
	if writerOn {
		<-s.writerEnd
	}
	if release {
		s.releaseLease()
	}
	e.mu.Lock()
	e.token = s.lease.token()
	e.mu.Unlock()
}

// compactRecord validates rec as one JSON value and returns it on one line.
func compactRecord(rec json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, rec); err != nil {
		return nil, fmt.Errorf("s3store: journal record is not valid JSON: %w", err)
	}
	if b.Len() == 0 {
		return nil, errors.New("s3store: empty journal record")
	}
	return b.Bytes(), nil
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

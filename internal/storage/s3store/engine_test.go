package s3store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// model is a tiny key/value state machine journaled through the engine:
// records are {"k":..,"v":..}; a snapshot is one chunk holding the map.
type model struct {
	mu       sync.Mutex
	e        *Engine
	kv       map[string]string
	chunkLSN uint64
	records  []Record // delivered by the last load
	chunks   int
	header   *SnapshotHeader
}

type kvRec struct {
	K string `json:"k"`
	V string `json:"v"`
}

func (m *model) load(ev LoadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev.Kind {
	case LoadBegin:
		m.kv, m.chunkLSN, m.records, m.chunks, m.header = map[string]string{}, 0, nil, 0, nil
	case LoadSnapshot:
		h := ev.Header
		m.header = &h
	case LoadChunk:
		m.chunks++
		m.kv = map[string]string{}
		if err := json.Unmarshal(ev.Chunk.Payload, &m.kv); err != nil {
			return err
		}
		m.chunkLSN = ev.Chunk.LSN
	case LoadRecord:
		m.records = append(m.records, ev.Record)
		if ev.Record.LSN > m.chunkLSN {
			var r kvRec
			if err := json.Unmarshal(ev.Record.Payload, &r); err != nil {
				return err
			}
			m.kv[r.K] = r.V
		}
	}
	return nil
}

func (m *model) snapshot(_ context.Context, w SnapshotWriter) error {
	m.mu.Lock()
	payload, _ := json.Marshal(m.kv)
	lsn := m.e.LastLSN()
	m.mu.Unlock()
	if err := w.SetExtra(json.RawMessage(`{"model":1}`)); err != nil {
		return err
	}
	return w.Chunk("kv", lsn, payload)
}

// set applies and journals k=v.
func (m *model) set(k, v string) (uint64, <-chan error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kv == nil {
		m.kv = map[string]string{}
	}
	m.kv[k] = v
	rec, _ := json.Marshal(kvRec{k, v})
	return m.e.Append(rec)
}

func (m *model) state() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.kv)
}

func (m *model) recordValues() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.records {
		var kv kvRec
		_ = json.Unmarshal(r.Payload, &kv)
		out = append(out, kv.K+"="+kv.V)
	}
	return out
}

// env is a remote with a metadata bucket.
type env struct {
	t      *testing.T
	remote *s3clienttest.Remote
	client *s3client.Client
}

const testBucket = "meta"
const testPrefix = "b1/"

func newEnv(t *testing.T) *env {
	t.Helper()
	r := s3clienttest.Start(t)
	r.CreateBucket(t, testBucket)
	return &env{t: t, remote: r, client: r.Client(t)}
}

func testLogger() *slog.Logger {
	if os.Getenv("S3STORE_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func (v *env) options(m *model) EngineOptions {
	return EngineOptions{
		Client:         v.client,
		Bucket:         testBucket,
		Prefix:         testPrefix,
		LeaseInterval:  300 * time.Millisecond,
		LeaseTTL:       2 * time.Second,
		StallTimeout:   time.Second,
		TempDir:        v.t.TempDir(),
		Logger:         testLogger(),
		Load:           m.load,
		Snapshot:       m.snapshot,
		SnapshotPolicy: SnapshotPolicy{MaxJournalBytes: -1, MaxBatches: -1, MaxAge: -1, CloseBatches: -1},
	}
}

// open opens an engine over a fresh model; mod adjusts the options.
func (v *env) open(mod func(*EngineOptions)) (*Engine, *model, error) {
	v.t.Helper()
	m := &model{}
	opts := v.options(m)
	if mod != nil {
		mod(&opts)
	}
	e, err := NewEngine(opts)
	if err != nil {
		v.t.Fatal(err)
	}
	e.retryBase, e.retryMax = 10*time.Millisecond, 100*time.Millisecond
	m.e = e
	v.t.Cleanup(func() { _ = e.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e, m, e.Open(ctx)
}

func (v *env) mustOpen(mod func(*EngineOptions)) (*Engine, *model) {
	v.t.Helper()
	e, m, err := v.open(mod)
	if err != nil {
		v.t.Fatalf("Open: %v", err)
	}
	return e, m
}

// keys lists the remote keys under the prefix + sub.
func (v *env) keys(sub string) []string {
	v.t.Helper()
	var out []string
	err := v.client.Walk(context.Background(), testBucket, testPrefix+sub, "", func(o s3client.ObjectAttrs) error {
		out = append(out, o.Key)
		return nil
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return out
}

func (v *env) put(key string, data []byte) {
	v.t.Helper()
	if _, err := v.client.PutObject(context.Background(), testBucket, key, bytes.NewReader(data), int64(len(data)), s3client.PutOptions{}); err != nil {
		v.t.Fatal(err)
	}
}

func (v *env) get(key string) []byte {
	v.t.Helper()
	data, _, err := remote{c: v.client, bucket: testBucket}.getBytes(context.Background(), key, maxBatchObject)
	if err != nil {
		v.t.Fatal(err)
	}
	return data
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("append never completed")
		return nil
	}
}

func mustSet(t *testing.T, m *model, k, val string) uint64 {
	t.Helper()
	lsn, done := m.set(k, val)
	if err := wait(t, done); err != nil {
		t.Fatalf("append %s=%s: %v", k, val, err)
	}
	return lsn
}

// crash stops an engine like a killed process: nothing is released.
func crash(e *Engine) {
	e.mu.Lock()
	s := e.sess
	e.mu.Unlock()
	if s != nil {
		s.abandon(false)
	}
	e.mu.Lock()
	e.state = StateClosed
	e.mu.Unlock()
}

func pauseLease(e *Engine) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sess.leasePaused.Store(true)
}

func isJournalReq(r *http.Request) bool {
	return strings.Contains(r.URL.Path, "/"+testPrefix+journalDir)
}

// --- marker ---

func TestMarkerCreateAndReopen(t *testing.T) {
	v := newEnv(t)
	e, m := v.mustOpen(nil)
	mk := e.Marker()
	if mk.Format != 1 || mk.Kind != "s3" || len(mk.BackingID) != 32 || mk.Gateway || mk.KEKCheck != "" {
		t.Fatalf("marker = %+v", mk)
	}
	if e.KEKState() != KEKNone {
		t.Fatalf("KEKState = %v", e.KEKState())
	}
	mustSet(t, m, "a", "1")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if len(v.keys("meta/probe-")) != 0 {
		t.Fatalf("probe objects left: %v", v.keys("meta/probe-"))
	}
	e2, m2 := v.mustOpen(nil)
	if e2.Marker().BackingID != mk.BackingID {
		t.Fatalf("backing ID changed: %q -> %q", mk.BackingID, e2.Marker().BackingID)
	}
	if got := m2.state(); got["a"] != "1" {
		t.Fatalf("state after reopen = %v", got)
	}
}

func TestMarkerRefusals(t *testing.T) {
	t.Run("unmanaged", func(t *testing.T) {
		v := newEnv(t)
		v.put(testPrefix+"data/1/xyz", []byte("blob"))
		if _, _, err := v.open(nil); !errors.Is(err, ErrUnmanagedPrefix) {
			t.Fatalf("Open = %v, want ErrUnmanagedPrefix", err)
		}
		if len(v.keys(markerKey)) != 0 {
			t.Fatal("marker created over unmanaged data")
		}
	})
	t.Run("gateway mismatch", func(t *testing.T) {
		v := newEnv(t)
		e, _ := v.mustOpen(nil)
		_ = e.Close()
		if _, _, err := v.open(func(o *EngineOptions) { o.Gateway = true }); !errors.Is(err, ErrMarkerMismatch) {
			t.Fatalf("Open = %v, want ErrMarkerMismatch", err)
		}
	})
	t.Run("kek", func(t *testing.T) {
		v := newEnv(t)
		kekA, kekB := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
		e, _ := v.mustOpen(nil)
		_ = e.Close()
		e, _ = v.mustOpen(func(o *EngineOptions) { o.KEK = kekA })
		if e.KEKState() != KEKVerified || e.Marker().KEKCheck == "" {
			t.Fatalf("with KEK: state %v, marker %+v", e.KEKState(), e.Marker())
		}
		_ = e.Close()
		if _, _, err := v.open(func(o *EngineOptions) { o.KEK = kekB }); !errors.Is(err, ErrWrongKEK) {
			t.Fatalf("wrong KEK: Open = %v", err)
		}
		e, _ = v.mustOpen(nil)
		if e.KEKState() != KEKMissing {
			t.Fatalf("without KEK: state %v", e.KEKState())
		}
		_ = e.Close()
		e, _ = v.mustOpen(func(o *EngineOptions) { o.KEK = kekA })
		if e.KEKState() != KEKVerified {
			t.Fatalf("right KEK again: state %v", e.KEKState())
		}
	})
}

func TestProbeRefusesIgnoredConditionals(t *testing.T) {
	v := newEnv(t)
	v.remote.IgnoreConditionals.Store(true)
	if _, _, err := v.open(nil); !errors.Is(err, ErrNoConditionalWrites) {
		t.Fatalf("Open = %v, want ErrNoConditionalWrites", err)
	}
	if len(v.keys(markerKey)) != 0 {
		t.Fatal("marker created on a service without conditional writes")
	}
}

// --- lease ---

func TestSecondOpenWhileAliveIsInUse(t *testing.T) {
	v := newEnv(t)
	e1, m1 := v.mustOpen(nil)
	mustSet(t, m1, "a", "1")
	start := time.Now()
	if _, _, err := v.open(nil); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Open = %v, want ErrInUse", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ErrInUse took %v", d)
	}
	// The first engine is unaffected.
	mustSet(t, m1, "b", "2")
	if st := e1.State(); st.State != StateServing {
		t.Fatalf("first engine: %+v", st)
	}
}

func TestCleanCloseAllowsImmediateTakeover(t *testing.T) {
	v := newEnv(t)
	e1, m1 := v.mustOpen(nil)
	mustSet(t, m1, "a", "1")
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, done := m1.set("late", "x"); !errors.Is(wait(t, done), ErrClosed) {
		t.Fatal("append after Close did not fail with ErrClosed")
	}
	start := time.Now()
	e2, m2 := v.mustOpen(nil)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("takeover after a clean Close took %v", d)
	}
	if e2.State().Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", e2.State().Epoch)
	}
	if got := m2.state(); !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("state = %v", got)
	}
}

func TestCrashedOwnerTakenOverAfterTTL(t *testing.T) {
	v := newEnv(t)
	e1, m1 := v.mustOpen(nil)
	mustSet(t, m1, "a", "1")
	crash(e1)
	start := time.Now()
	e2, m2 := v.mustOpen(nil)
	d := time.Since(start)
	if d < time.Second {
		t.Fatalf("crashed lease taken over after only %v (TTL 2s)", d)
	}
	if got := m2.state(); !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("state = %v", got)
	}
	mustSet(t, m2, "b", "2")
	if e2.State().Epoch != 2 {
		t.Fatalf("epoch = %d", e2.State().Epoch)
	}
}

func TestZombieIsFencedBySeal(t *testing.T) {
	v := newEnv(t)
	a, ma := v.mustOpen(nil)
	mustSet(t, ma, "r1", "a")
	pauseLease(a) // a stalls: its lease expires but it keeps running

	b, mb := v.mustOpen(nil)
	if got := mb.state(); !maps.Equal(got, map[string]string{"r1": "a"}) {
		t.Fatalf("b state = %v", got)
	}
	_, done := ma.set("r2", "zombie")
	err := wait(t, done)
	if !errors.Is(err, ErrFenced) || !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("zombie append = %v, want ErrFenced and ErrBackingUnavailable", err)
	}
	if st := a.State(); st.State != StateFailed || !errors.Is(st.Err, ErrFenced) {
		t.Fatalf("zombie state = %+v", st)
	}
	if _, done := ma.set("r2b", "zombie"); wait(t, done) == nil {
		t.Fatal("append on a fenced engine succeeded")
	}
	mustSet(t, mb, "r3", "b")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	_, mc := v.mustOpen(nil)
	if got := mc.state(); !maps.Equal(got, map[string]string{"r1": "a", "r3": "b"}) {
		t.Fatalf("third open state = %v", got)
	}
	if got := mc.recordValues(); !slices.Equal(got, []string{"r1=a", "r3=b"}) {
		t.Fatalf("records = %v", got)
	}
}

// A zombie whose batch lands between the new owner's listing and its seal
// keeps that batch: the new owner applies it and seals the next slot.
func TestSealAppliesLateZombieBatch(t *testing.T) {
	v := newEnv(t)
	a, ma := v.mustOpen(nil)
	mustSet(t, ma, "r1", "a")
	pauseLease(a)

	// Let the zombie write exactly when b first tries to seal: b's seal PUT
	// is held until the zombie's append into the same slot is durable.
	var hooked atomic.Bool
	zombieDone := make(chan error, 1)
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/"+testPrefix+journalDir+hex16(1)+"/"+hex16(3)) &&
			hooked.CompareAndSwap(false, true) {
			_, done := ma.set("r2", "late")
			zombieDone <- <-done
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	b, mb := v.mustOpen(nil)
	v.remote.Fail.Store(nil)
	select {
	case err := <-zombieDone:
		// The zombie's batch and b's seal raced for slot 3; the zombie
		// held it first (b's request was paused in the hook).
		if err != nil {
			t.Fatalf("zombie append = %v", err)
		}
	default:
		t.Fatal("seal hook never ran")
	}
	if got := mb.state(); !maps.Equal(got, map[string]string{"r1": "a", "r2": "late"}) {
		t.Fatalf("new owner state = %v (late zombie batch not applied)", got)
	}
	_, done := ma.set("r3", "zombie")
	if err := wait(t, done); !errors.Is(err, ErrFenced) {
		t.Fatalf("zombie append after seal = %v", err)
	}
	_ = b.Close()
	_, mc := v.mustOpen(nil)
	if got := mc.state(); !maps.Equal(got, map[string]string{"r1": "a", "r2": "late"}) {
		t.Fatalf("reopened state = %v", got)
	}
}

func TestLeaseOverwrittenFences(t *testing.T) {
	v := newEnv(t)
	e, m := v.mustOpen(nil)
	mustSet(t, m, "a", "1")
	v.put(testPrefix+leaseKey, []byte(`{"owner":"intruder","epoch":7}`))
	deadline := time.Now().Add(5 * time.Second)
	for e.State().State != StateFailed {
		if time.Now().After(deadline) {
			t.Fatal("engine kept serving after its lease was overwritten")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := e.State(); !errors.Is(st.Err, ErrFenced) || !errors.Is(st.Err, storage.ErrBackingUnavailable) {
		t.Fatalf("state = %+v", st)
	}
	if _, done := m.set("b", "2"); !errors.Is(wait(t, done), ErrFenced) {
		t.Fatal("append after fencing did not fail with ErrFenced")
	}
}

func TestSimultaneousOpensOneWins(t *testing.T) {
	v := newEnv(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = v.open(nil)
		}()
	}
	wg.Wait()
	ok, inUse := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInUse):
			inUse++
		default:
			t.Errorf("Open = %v", err)
		}
	}
	if ok != 1 || inUse != 1 {
		t.Fatalf("results %v: want one success and one ErrInUse", errs)
	}
}

// --- journal ---

func TestAmbiguousJournalWriteResolved(t *testing.T) {
	for name, fault := range map[string]s3clienttest.Fault{"Status500": s3clienttest.Status500, "Reset": s3clienttest.Reset} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t)
			e, m := v.mustOpen(nil)
			var faults atomic.Int32
			f := func(r *http.Request) s3clienttest.Fault {
				if r.Method == http.MethodPut && isJournalReq(r) && faults.Add(1) == 1 {
					return fault
				}
				return s3clienttest.Pass
			}
			v.remote.After.Store(&f)
			mustSet(t, m, "a", "1")
			v.remote.After.Store(nil)
			if faults.Load() != 1 {
				t.Fatalf("journal PUTs = %d, want exactly 1 (the ambiguous write must not be resent)", faults.Load())
			}
			mustSet(t, m, "b", "2")
			_ = e.Close()
			// Epoch 1 holds the start batch and the two record batches.
			if got := v.keys(journalDir + hex16(1) + "/"); len(got) != 3 {
				t.Fatalf("journal objects = %v", got)
			}
			_, m2 := v.mustOpen(nil)
			if got := m2.recordValues(); !slices.Equal(got, []string{"a=1", "b=2"}) {
				t.Fatalf("records = %v", got)
			}
		})
	}
}

func TestStallFailStops(t *testing.T) {
	v := newEnv(t)
	failed := make(chan error, 1)
	e, m := v.mustOpen(func(o *EngineOptions) { o.OnFail = func(err error) { failed <- err } })
	mustSet(t, m, "a", "1")
	f := func(r *http.Request) int {
		if isJournalReq(r) {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	start := time.Now()
	_, done := m.set("b", "2")
	err := wait(t, done)
	if !errors.Is(err, storage.ErrBackingUnavailable) || !errors.Is(err, errJournalStalled) {
		t.Fatalf("append during a 503 storm = %v", err)
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("failed after %v, before the stall timeout", d)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnFail not called")
	}
	if st := e.State(); st.State != StateFailed {
		t.Fatalf("state = %+v", st)
	}
	if _, done := m.set("c", "3"); !errors.Is(wait(t, done), storage.ErrBackingUnavailable) {
		t.Fatal("append on a failed engine did not fail")
	}
	// Reload once the remote is back: the lease is re-taken at once.
	v.remote.Fail.Store(nil)
	start = time.Now()
	if err := e.Open(context.Background()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("reopen took %v", d)
	}
	if got := m.state(); !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("reloaded state = %v", got)
	}
	mustSet(t, m, "d", "4")
	if st := e.State(); st.State != StateServing || st.Epoch != 2 {
		t.Fatalf("state after reopen = %+v", st)
	}
}

func TestGroupCommit(t *testing.T) {
	v := newEnv(t)
	e, m := v.mustOpen(nil)
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && isJournalReq(r) {
			time.Sleep(50 * time.Millisecond)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	const n = 200
	var wg sync.WaitGroup
	var acked atomic.Int32
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.mu.Lock()
			rec, _ := json.Marshal(kvRec{fmt.Sprint("k", i), "v"})
			m.kv[fmt.Sprint("k", i)] = "v"
			_, done := e.AppendWith(rec, func() { acked.Add(1) })
			m.mu.Unlock()
			errs <- wait(t, done)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if acked.Load() != n {
		t.Fatalf("ack callbacks = %d", acked.Load())
	}
	if e.DurableLSN() != n {
		t.Fatalf("DurableLSN = %d", e.DurableLSN())
	}
	if err := e.WaitDurable(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	v.remote.Fail.Store(nil)
	batches := len(v.keys(journalDir+hex16(1)+"/")) - 1
	t.Logf("%d records in %d batches", n, batches)
	if batches >= n/4 {
		t.Fatalf("%d batches for %d concurrent appends: no group commit", batches, n)
	}
	_ = e.Close()
	_, m2 := v.mustOpen(nil)
	if len(m2.records) != n {
		t.Fatalf("reloaded %d records", len(m2.records))
	}
	for i, r := range m2.records {
		if r.LSN != uint64(i+1) {
			t.Fatalf("record %d has LSN %d", i, r.LSN)
		}
	}
}

func TestAppendRejectsInvalidJSON(t *testing.T) {
	v := newEnv(t)
	e, _ := v.mustOpen(nil)
	if _, done := e.Append(json.RawMessage(`{"a":`)); wait(t, done) == nil {
		t.Fatal("invalid record accepted")
	}
	lsn, done := e.Append(json.RawMessage("{\n  \"a\": 1\n}"))
	if err := wait(t, done); err != nil || lsn != 1 {
		t.Fatalf("multi-line record: lsn %d, %v", lsn, err)
	}
}

// --- snapshots ---

// fill sets n keys with the given round and returns the expected state.
func fill(t *testing.T, m *model, want map[string]string, round, n int) {
	t.Helper()
	for i := range n {
		k, val := fmt.Sprintf("k%03d", i%37), fmt.Sprintf("r%d-%d", round, i)
		mustSet(t, m, k, val)
		want[k] = val
	}
}

func TestSnapshotGCAndLoad(t *testing.T) {
	v := newEnv(t)
	e, m := v.mustOpen(nil)
	want := map[string]string{}
	ctx := context.Background()
	for round := range 4 {
		fill(t, m, want, round, 40)
		if err := e.Snapshot(ctx); err != nil {
			t.Fatalf("snapshot %d: %v", round, err)
		}
	}
	fill(t, m, want, 9, 25)
	snaps := v.keys(snapshotDir)
	if len(snaps) != 2 {
		t.Fatalf("snapshots after GC = %v", snaps)
	}
	batches := v.keys(journalDir)
	if len(batches) >= 4*40 {
		t.Fatalf("journal not collected: %d objects", len(batches))
	}
	// Everything after the older kept snapshot is still there.
	older, _, _ := keys{testPrefix}.parseSnapshot(snaps[0])
	for _, k := range batches {
		h, _, err := decodeBatch(v.get(k))
		if err != nil {
			t.Fatal(err)
		}
		if h.LSN[1] <= older && k != batches[len(batches)-1] {
			t.Errorf("batch %s (lsn %v) should have been collected (older snapshot at %d)", k, h.LSN, older)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	_, m2 := v.mustOpen(nil)
	if m2.header == nil || m2.chunks != 1 || string(m2.header.Extra) != `{"model":1}` {
		t.Fatalf("snapshot not loaded: header %+v, chunks %d", m2.header, m2.chunks)
	}
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("loaded state differs:\n got %v\nwant %v", got, want)
	}
	if len(m2.records) != 25 {
		t.Fatalf("replayed %d records after the snapshot, want 25", len(m2.records))
	}
}

func TestCorruptNewestSnapshotFallsBack(t *testing.T) {
	for _, damage := range []string{"garbage", "truncated"} {
		t.Run(damage, func(t *testing.T) {
			v := newEnv(t)
			e, m := v.mustOpen(nil)
			want := map[string]string{}
			ctx := context.Background()
			for round := range 2 {
				fill(t, m, want, round, 30)
				if err := e.Snapshot(ctx); err != nil {
					t.Fatal(err)
				}
			}
			fill(t, m, want, 5, 10)
			_ = e.Close()
			snaps := v.keys(snapshotDir)
			newest := snaps[len(snaps)-1]
			switch damage {
			case "garbage":
				v.put(newest, []byte("not a snapshot"))
			case "truncated":
				data := v.get(newest)
				v.put(newest, data[:len(data)-20])
			}
			_, m2 := v.mustOpen(nil)
			if m2.header == nil {
				t.Fatal("no snapshot loaded")
			}
			older, _, _ := keys{testPrefix}.parseSnapshot(snaps[0])
			if m2.header.StartLSN != older {
				t.Fatalf("loaded snapshot at %d, want the older one at %d", m2.header.StartLSN, older)
			}
			if got := m2.state(); !maps.Equal(got, want) {
				t.Fatalf("state differs:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestGCKeepsSeals(t *testing.T) {
	v := newEnv(t)
	want := map[string]string{}
	for round := range 3 {
		e, m := v.mustOpen(nil)
		fill(t, m, want, round, 5)
		_ = e.Close()
	}
	e, m := v.mustOpen(nil)
	ctx := context.Background()
	for round := range 3 {
		fill(t, m, want, 10+round, 20)
		if err := e.Snapshot(ctx); err != nil {
			t.Fatal(err)
		}
	}
	seals := map[uint64]bool{}
	lastOf := map[uint64]uint64{}
	for _, k := range v.keys(journalDir) {
		h, _, err := decodeBatch(v.get(k))
		if err != nil {
			t.Fatal(err)
		}
		if h.Seal {
			seals[h.Epoch] = true
		}
		lastOf[h.Epoch] = max(lastOf[h.Epoch], h.Seq)
	}
	for ep := uint64(1); ep <= 3; ep++ {
		if !seals[ep] {
			t.Errorf("seal of epoch %d was deleted (or never written)", ep)
		}
	}
	if _, ok := lastOf[4]; !ok {
		t.Error("current epoch has no journal object left")
	}
	_ = e.Close()
	_, m2 := v.mustOpen(nil)
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("state differs:\n got %v\nwant %v", got, want)
	}
}

func TestBackgroundSnapshotPolicy(t *testing.T) {
	v := newEnv(t)
	e, m := v.mustOpen(func(o *EngineOptions) {
		o.SnapshotPolicy = SnapshotPolicy{MaxJournalBytes: -1, MaxBatches: 10, MaxAge: -1, CloseBatches: -1}
	})
	want := map[string]string{}
	fill(t, m, want, 0, 25)
	deadline := time.Now().Add(10 * time.Second)
	for len(v.keys(snapshotDir)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no background snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fill(t, m, want, 1, 30)
	_ = e.Close()
	_, m2 := v.mustOpen(nil)
	if m2.header == nil {
		t.Fatal("no snapshot loaded")
	}
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("state differs:\n got %v\nwant %v", got, want)
	}
}

func TestClosingSnapshot(t *testing.T) {
	v := newEnv(t)
	policy := func(o *EngineOptions) {
		o.SnapshotPolicy = SnapshotPolicy{MaxJournalBytes: -1, MaxBatches: -1, MaxAge: -1, CloseBatches: 3}
	}
	e, m := v.mustOpen(policy)
	want := map[string]string{}
	fill(t, m, want, 0, 2)
	_ = e.Close()
	if len(v.keys(snapshotDir)) != 0 {
		t.Fatal("closing snapshot below the threshold")
	}
	e, m = v.mustOpen(policy)
	fill(t, m, want, 1, 5)
	_ = e.Close()
	_, m2 := v.mustOpen(nil)
	if m2.header == nil || len(m2.records) != 0 {
		t.Fatalf("closing snapshot missing: header %v, %d records replayed", m2.header, len(m2.records))
	}
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("state differs:\n got %v\nwant %v", got, want)
	}
}

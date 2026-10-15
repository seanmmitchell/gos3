package s3store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Regression tests for review findings.

// Records of a bucket written while a snapshot copies other buckets (after
// the bucket itself was copied) must survive a reload: their LSNs lie
// between the snapshot's start and the highest chunk LSN.
func TestSnapshotKeepsRecordsWrittenDuringIt(t *testing.T) {
	for _, gateway := range []bool{false, true} {
		name := "objects"
		if gateway {
			name = "gateway"
		}
		t.Run(name, func(t *testing.T) {
			v := newBenv(t)
			opts := v.options("p/")
			opts.Gateway = gateway
			b := v.ready(opts)
			ctx := t.Context()
			for _, n := range []string{"a", "b", "c"} {
				if err := b.CreateBucket(ctx, n, storage.CreateBucketOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := b.PutBucketConfig(ctx, n, storage.ConfigTagging, []byte("before")); err != nil {
					t.Fatal(err)
				}
			}
			aux := b.AuxState("gw")
			if gateway {
				if err := aux.PutRecord(ctx, "before", []byte("1")); err != nil {
					t.Fatal(err)
				}
			}
			b.snapHook = func(bucket string) {
				if bucket != "a" {
					return
				}
				// Bucket a is in the snapshot; b and c are copied later,
				// with higher LSNs.
				if err := b.PutBucketConfig(ctx, "a", storage.ConfigTagging, []byte("during")); err != nil {
					t.Error(err)
				}
				if gateway {
					if err := aux.PutRecord(ctx, "during", []byte("2")); err != nil {
						t.Error(err)
					}
				} else {
					putObject(t, b, "a", "during", []byte("acked"))
				}
				if err := b.PutBucketConfig(ctx, "b", storage.ConfigCORS, []byte("x")); err != nil {
					t.Error(err)
				}
			}
			if err := b.eng.Snapshot(ctx); err != nil {
				t.Fatal(err)
			}
			b.snapHook = nil
			_ = b.Close()
			b = v.ready(opts)
			if data, err := b.GetBucketConfig(ctx, "a", storage.ConfigTagging); err != nil || string(data) != "during" {
				t.Fatalf("config of a after reload = %q, %v", data, err)
			}
			if data, err := b.GetBucketConfig(ctx, "b", storage.ConfigCORS); err != nil || string(data) != "x" {
				t.Fatalf("config of b after reload = %q, %v", data, err)
			}
			if gateway {
				recs, err := b.AuxState("gw").Records(ctx)
				if err != nil || string(recs["during"]) != "2" || string(recs["before"]) != "1" {
					t.Fatalf("aux records after reload = %v, %v", recs, err)
				}
			} else if got := readObject(t, b, "a", "during", ""); string(got) != "acked" {
				t.Fatalf("object written during the snapshot = %q", got)
			}
		})
	}
}

// The recovery pass must not delete a blob whose removal is not durable:
// if the removal's record then fails, the reloaded state still references
// it.
func TestRecoveryKeepsBlobsOfPendingRemovals(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.OrphanRetention = time.Minute
	opts.StallTimeout = 1500 * time.Millisecond
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "x", []byte("precious"))
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/meta/journal/") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	deleted := make(chan error, 1)
	go func() {
		_, err := b.DeleteObject(context.Background(), "bk", "x", "")
		deleted <- err
	}()
	waitFor(t, "the in-memory removal", func() bool {
		bk, err := b.lookup(ctx, "bk")
		if err != nil {
			return false
		}
		bk.mu.RLock()
		defer bk.mu.RUnlock()
		_, _, err = bk.ix.Find("x", "")
		return err != nil
	})
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	st, err := b.recoverOrphans(ctx)
	b.now = time.Now
	if err != nil || st.deleted != 0 {
		t.Fatalf("recovery during a pending removal: %+v, %v", st, err)
	}
	if err := <-deleted; !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("delete during the journal outage = %v", err)
	}
	v.remote.Fail.Store(nil)
	waitFor(t, "reload", func() bool { return b.Status().State == StateServing && b.WaitReady(ctx) == nil })
	if got := readObject(t, b, "bk", "x", ""); string(got) != "precious" {
		t.Fatalf("x after reload = %q", got)
	}
	// Recovery refuses to run while the backing is not serving.
	b.eng.failCurrent(errors.New("test"))
	if _, err := b.recoverOrphans(ctx); err == nil {
		t.Fatal("recovery ran on a failed backing")
	}
}

// A gap in a journal epoch (a covered batch deleted by an interrupted GC)
// must not drop the batches after it, and the seal goes after the highest
// slot; a gap of records not covered by the snapshot refuses the load.
func TestJournalGap(t *testing.T) {
	setup := func(t *testing.T) (*env, map[string]string) {
		v := newEnv(t)
		e, m := v.mustOpen(nil)
		want := map[string]string{}
		for _, k := range []string{"r1", "r2", "r3", "r4", "r5"} {
			mustSet(t, m, k, k)
			want[k] = k
		}
		if err := e.Snapshot(context.Background()); err != nil { // StartLSN 5
			t.Fatal(err)
		}
		for _, k := range []string{"r6", "r7"} {
			mustSet(t, m, k, k)
			want[k] = k
		}
		_ = e.Close()
		return v, want
	}
	slot := func(seq uint64) string { return testPrefix + journalDir + hex16(1) + "/" + hex16(seq) }
	t.Run("covered", func(t *testing.T) {
		v, want := setup(t)
		// Slot 3 holds r2, covered by the snapshot.
		if err := v.client.DeleteObject(context.Background(), testBucket, slot(3)); err != nil {
			t.Fatal(err)
		}
		_, m := v.mustOpen(nil)
		if got := m.state(); !maps.Equal(got, want) {
			t.Fatalf("state = %v, want %v", got, want)
		}
		h, _, err := decodeBatch(v.get(slot(9)))
		if err != nil || !h.Seal {
			t.Fatalf("slot 9 = %+v, %v; want the seal after the highest slot", h, err)
		}
		if keys := v.keys(journalDir + hex16(1) + "/"); slices.Contains(keys, slot(3)) {
			t.Fatal("the seal filled the gap")
		}
	})
	t.Run("uncovered", func(t *testing.T) {
		v, _ := setup(t)
		// Slot 7 holds r6, written after the snapshot.
		if err := v.client.DeleteObject(context.Background(), testBucket, slot(7)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := v.open(nil); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("Open with records missing = %v, want ErrCorruptJournal", err)
		}
		if keys := v.keys(journalDir + hex16(1) + "/"); slices.Contains(keys, slot(7)) {
			t.Fatal("a seal was written into the gap")
		}
	})
}

// Journal GC deletes oldest first and stops at the first failed request, so
// the journal left behind has no gap.
func TestJournalGCStopsAtFirstFailure(t *testing.T) {
	old := gcChunk
	gcChunk = 3
	defer func() { gcChunk = old }()
	v := newEnv(t)
	e, m := v.mustOpen(nil)
	ctx := context.Background()
	want := map[string]string{}
	fill(t, m, want, 0, 12)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	fill(t, m, want, 1, 12)
	var deletes atomic.Int32
	f := func(r *http.Request) int {
		if r.Method == http.MethodPost && r.URL.Query().Has("delete") && deletes.Add(1) > 1 {
			return http.StatusInternalServerError
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	v.remote.Fail.Store(nil)
	var seqs []uint64
	for _, k := range v.keys(journalDir + hex16(1) + "/") {
		_, seq, _ := keys{testPrefix}.parseBatch(k)
		seqs = append(seqs, seq)
	}
	if len(seqs) == 0 || seqs[0] != 4 {
		t.Fatalf("journal after an interrupted GC starts at slot %v, want 4 (one chunk of 3 deleted)", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("gap in the journal after GC: %v", seqs)
		}
	}
	_ = e.Close()
	_, m2 := v.mustOpen(nil)
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("state = %v", got)
	}
}

// A snapshot taken while an append is refused (Close) must not persist the
// refused mutation the caller already applied to its state.
func TestClosingSnapshotSkipsRefusedMutations(t *testing.T) {
	v := newEnv(t)
	m := &model{}
	opts := v.options(m)
	opts.SnapshotPolicy = SnapshotPolicy{MaxJournalBytes: -1, MaxBatches: -1, MaxAge: -1, CloseBatches: 1}
	var refused atomic.Value
	opts.Snapshot = func(ctx context.Context, w SnapshotWriter) error {
		// A writer racing Close: applied to the state, refused by the
		// journal.
		_, done := m.set("refused", "x")
		refused.Store(<-done)
		return m.snapshot(ctx, w)
	}
	e, err := NewEngine(opts)
	if err != nil {
		t.Fatal(err)
	}
	m.e = e
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	fill(t, m, want, 0, 3)
	_ = e.Close()
	if err, _ := refused.Load().(error); !errors.Is(err, ErrClosed) {
		t.Fatalf("append during the closing snapshot = %v", err)
	}
	if got := v.keys(snapshotDir); len(got) != 0 {
		t.Fatalf("closing snapshot with a refused mutation was written: %v", got)
	}
	_, m2 := v.mustOpen(nil)
	if got := m2.state(); !maps.Equal(got, want) {
		t.Fatalf("state = %v, want %v", got, want)
	}
}

// PrepareConnection with a KEK on a backing without a KEK check records
// the check, so a second, different KEK is refused; with key objects, the
// KEK must open them.
func TestPrepareConnectionRecordsKEK(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.options("p/"))
	ctx := t.Context()
	kek1, kek2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	commit, err := b.PrepareConnection(ctx, Connection{Client: v.client, KEK: kek1})
	if err != nil {
		t.Fatal(err)
	}
	commit()
	if m, _, _ := ReadMarker(ctx, v.client, storeBucket, "p/"); m.KEKCheck != crypt.KeyCheck(kek1) {
		t.Fatalf("marker check = %q", m.KEKCheck)
	}
	if b.Engine().KEKState() != KEKVerified {
		t.Fatalf("KEKState = %v", b.Engine().KEKState())
	}
	if _, err := b.PrepareConnection(ctx, Connection{Client: v.client, KEK: kek2}); !errors.Is(err, ErrWrongKEK) {
		t.Fatalf("second KEK = %v", err)
	}

	// Key objects wrapped with kek1 and the marker's check lost: kek2 is
	// refused because it cannot open them, kek1 restores the check.
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	m, _, _ := ReadMarker(ctx, v.client, storeBucket, "p/")
	m.KEKCheck = ""
	raw, _ := json.Marshal(m)
	v.put("p/"+markerKey, raw)
	if _, err := b.PrepareConnection(ctx, Connection{Client: v.client, KEK: kek2}); !errors.Is(err, ErrWrongKEK) {
		t.Fatalf("KEK that cannot open the keys = %v", err)
	}
	if _, err := b.PrepareConnection(ctx, Connection{Client: v.client, KEK: kek1}); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := ReadMarker(ctx, v.client, storeBucket, "p/"); m.KEKCheck != crypt.KeyCheck(kek1) {
		t.Fatalf("marker check not restored: %q", m.KEKCheck)
	}
}

// Connection.PartSize applies on commit.
func TestPrepareConnectionPartSize(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.PartSize = 0 // default: 16 MiB single PUTs
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	var multipart atomic.Int32
	f := func(r *http.Request) int {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			multipart.Add(1)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	data := bytes.Repeat([]byte("z"), 6<<20)
	putObject(t, b, "bk", "one", data)
	if multipart.Load() != 0 {
		t.Fatal("multipart upload below the default part size")
	}
	commit, err := b.PrepareConnection(ctx, Connection{Client: v.client, PartSize: 5 << 20})
	if err != nil {
		t.Fatal(err)
	}
	commit()
	putObject(t, b, "bk", "two", data)
	if multipart.Load() != 1 {
		t.Fatalf("multipart uploads = %d after setting a 5 MiB part size", multipart.Load())
	}
}

// ErrClosed is a backing-unavailable error (503, not 500).
func TestErrClosedIsUnavailable(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.options("p/"))
	_ = b.Close()
	_, err := b.ListBuckets(t.Context())
	if !errors.Is(err, ErrClosed) || !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("after Close = %v", err)
	}
}

// A caller whose context ends while its record is being written still
// learns that the write happened.
func TestCancelledCallerSeesDurableWrite(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.Gateway = true
	b := v.ready(opts)
	if err := b.CreateBucket(t.Context(), "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/meta/journal/") {
			time.Sleep(300 * time.Millisecond)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	for name, write := range map[string]func(ctx context.Context) error{
		"aux record": func(ctx context.Context) error { return b.AuxState("gw").PutRecord(ctx, "k", []byte("v")) },
		"config":     func(ctx context.Context) error { return b.PutBucketConfig(ctx, "bk", storage.ConfigCORS, []byte("c")) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := write(ctx)
		cancel()
		if err != nil {
			t.Fatalf("%s with a cancelled context = %v, want nil (the record is durable)", name, err)
		}
	}
	recs, err := b.AuxState("gw").Records(t.Context())
	if err != nil || string(recs["k"]) != "v" {
		t.Fatalf("records = %v, %v", recs, err)
	}
}

// The restic recovery store maps remote failures like other key I/O.
func TestRecoveryStoreErrors(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.kekOptions("p/"))
	status := http.StatusForbidden
	f := func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, ".restic.json") {
			return status
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	if _, err := b.recovery.Load(t.Context(), "abc"); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("Load refused = %v", err)
	}
	status = http.StatusServiceUnavailable
	if _, err := b.recovery.Load(t.Context(), "abc"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("Load unavailable = %v", err)
	}
	if err := b.recovery.Store(t.Context(), "abc", restic.Record{Repository: "s3:x"}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("Store unavailable = %v", err)
	}
}

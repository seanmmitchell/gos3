package multi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// importAt is the LastModified of the i-th imported test version: in the
// past, with nanoseconds and a zone, which must all survive the import.
func importAt(i int) time.Time {
	return time.Date(2020, 1, 2, 3, 4, 5, 600700800, time.FixedZone("UTC+2", 2*3600)).Add(-time.Duration(i) * time.Hour)
}

// startImport opens an imported version of bucket/key through vi and
// writes data.
func startImport(t *testing.T, vi storage.CompressingImporter, bucket, key, data string, opts storage.WriteOptions) storage.VersionWriter {
	t.Helper()
	w, err := vi.NewVersionWriterWith(context.Background(), bucket, key, opts)
	if err != nil {
		t.Fatalf("NewVersionWriterWith %s/%s: %v", bucket, key, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatalf("Write %s/%s: %v", bucket, key, err)
	}
	return w
}

// importVersion imports data as version id of bucket/key, modified at lm.
func importVersion(t *testing.T, vi storage.CompressingImporter, bucket, key, id, data string, lm time.Time) storage.ObjectInfo {
	t.Helper()
	w := startImport(t, vi, bucket, key, data, storage.WriteOptions{})
	info, err := w.Commit(context.Background(), storage.ObjectInfo{
		VersionID: id, LastModified: lm,
		ObjectMeta: storage.ObjectMeta{ETag: "etag-" + id, ContentType: "text/plain", UserMeta: map[string]string{"id": id}},
	})
	if err != nil {
		t.Fatalf("Commit %s/%s@%s: %v", bucket, key, id, err)
	}
	return info
}

// wantVersion checks that b holds version id of bucket/key with data and
// LastModified lm.
func wantVersion(t *testing.T, b storage.Backend, name, bucket, key, id, data string, lm time.Time) {
	t.Helper()
	got, err := b.GetObjectInfo(context.Background(), bucket, key, id)
	if err != nil {
		t.Fatalf("%s: GetObjectInfo %s/%s@%s: %v", name, bucket, key, id, err)
	}
	if got.VersionID != id || !got.LastModified.Equal(lm) || got.ETag != "etag-"+id || got.UserMeta["id"] != id {
		t.Fatalf("%s: %s/%s@%s = %+v", name, bucket, key, id, got)
	}
	if s := read(t, b, bucket, key, id); s != data {
		t.Fatalf("%s: %s/%s@%s holds %q, want %q", name, bucket, key, id, s, data)
	}
}

// gateActive returns the number of writes in flight on bucket.
func gateActive(t *testing.T, r *Router, bucket string) int {
	t.Helper()
	bs, err := r.lookup(bucket)
	if err != nil {
		t.Fatal(err)
	}
	bs.gate.mu.Lock()
	defer bs.gate.mu.Unlock()
	return bs.gate.active
}

// Imports through the router keep version IDs, LastModified times and the
// import order, and reach an in-sync active-active replica before Commit
// returns, with the same identity and stored compressed when asked.
func TestImportTeesToActiveActiveReplica(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "aa", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, "aa", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "aa", StateInSync)

	// LastModified decreasing: the order is the import order.
	for i, id := range []string{"v1", "v2", "v3"} {
		info := importVersion(t, r, "aa", "k", id, "data "+id, importAt(i))
		if info.VersionID != id || !info.LastModified.Equal(importAt(i)) || !info.IsLatest {
			t.Fatalf("Commit of %s = %+v", id, info)
		}
		// Synchronously on both copies.
		for _, b := range bk {
			wantVersion(t, b.Backend, b.Name, "aa", "k", id, "data "+id, importAt(i))
		}
	}
	if a, b := dump(t, bk[0].Backend, "aa"), dump(t, bk[1].Backend, "aa"); a != b {
		t.Fatalf("copies differ right after the imports:\nprimary:\n%s\nreplica:\n%s", a, b)
	}
	latest, err := r.GetObjectInfo(ctx, "aa", "k", "")
	if err != nil || latest.VersionID != "v3" {
		t.Fatalf("latest version through the router: %+v, %v", latest, err)
	}

	// An imported delete marker, and a compressed import.
	w := startImport(t, r, "aa", "k", "", storage.WriteOptions{})
	if _, err := w.Commit(ctx, storage.ObjectInfo{VersionID: "dm", LastModified: importAt(5), DeleteMarker: true}); err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("compressible ", 2048)
	w = startImport(t, r, "aa", "z", data, storage.WriteOptions{
		Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1},
	})
	if _, err := w.Commit(ctx, storage.ObjectInfo{VersionID: "zz", LastModified: importAt(6), ObjectMeta: storage.ObjectMeta{ETag: "z"}}); err != nil {
		t.Fatal(err)
	}
	for _, b := range bk {
		got, err := b.Backend.GetObjectInfo(ctx, "aa", "k", "dm")
		if err != nil || !got.DeleteMarker || !got.LastModified.Equal(importAt(5)) {
			t.Errorf("%s: imported delete marker: %+v, %v", b.Name, got, err)
		}
		got, err = b.Backend.GetObjectInfo(ctx, "aa", "z", "zz")
		if err != nil || got.Compression != storage.CompressionDeflate || got.StoredSize >= got.Size {
			t.Errorf("%s: compressed import: %+v, %v", b.Name, got, err)
		}
	}
	// The replica took every import synchronously: it was never demoted.
	if st, _ := r.Placement("aa"); st.Members[1].State != StateInSync {
		t.Fatalf("replica after the imports: %+v", st.Members[1])
	}
	if n := gateActive(t, r, "aa"); n != 0 {
		t.Fatalf("%d writes still in flight after the imports", n)
	}
}

// An import into an active-passive bucket reaches the replica in the
// background.
func TestImportReachesAsyncReplica(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ap", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, "ap", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	// The replica's initial comparison is done: only the key marked dirty
	// by the import can bring it over.
	waitState(t, r, "ap", StateInSync)
	waitFor(t, "the replica's initial copy", func() bool {
		bs, _ := r.lookup("ap")
		r.mu.RLock()
		ms := bs.members["b"]
		r.mu.RUnlock()
		ms.mu.Lock()
		defer ms.mu.Unlock()
		return ms.fullDone && !ms.running
	})
	importVersion(t, r, "ap", "k", "v1", "one", importAt(1))
	importVersion(t, r, "ap", "k", "v2", "two", importAt(2))
	waitSame(t, "ap", bk[0].Backend, bk[1].Backend)
	wantVersion(t, bk[1].Backend, "replica", "ap", "k", "v1", "one", importAt(1))
	wantVersion(t, bk[1].Backend, "replica", "ap", "k", "v2", "two", importAt(2))
}

// An import holds the bucket's gate until it is committed: a move's
// cutover waits for it (and new writes wait for the cutover), and the
// version, committed after the move's full copy, reaches the move target
// before the switch.
func TestImportHoldsGateAndReachesMoveTarget(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "mv", storage.CreateBucketOptions{Backing: "a"}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "mv", "old", "old data", storage.ObjectMeta{})

	w := startImport(t, r, "mv", "k", "imported", storage.WriteOptions{})
	if _, err := r.Move(ctx, "mv", "", "b", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the move's cutover to close the gate", func() bool {
		st, err := r.Placement("mv")
		if err != nil || st.Move == nil || st.Move.Phase != phaseCutover {
			return false
		}
		bs, _ := r.lookup("mv")
		bs.gate.mu.Lock()
		defer bs.gate.mu.Unlock()
		return bs.gate.closed != nil
	})
	// The cutover waits for the import; new writes wait for the cutover.
	time.Sleep(50 * time.Millisecond)
	if st, _ := r.Placement("mv"); st.Move == nil || st.Primary != "a" {
		t.Fatalf("the move completed while an import was in flight: %+v", st)
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err := r.NewVersionWriter(tctx, "mv", "other")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NewVersionWriter during a cutover: %v", err)
	}

	if _, err := w.Commit(ctx, storage.ObjectInfo{VersionID: storage.NullVersionID, LastModified: importAt(1),
		ObjectMeta: storage.ObjectMeta{ETag: "etag-null", UserMeta: map[string]string{"id": storage.NullVersionID}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the move to complete", func() bool {
		st, err := r.Placement("mv")
		return err == nil && st.Move == nil && st.Primary == "b"
	})
	wantVersion(t, bk[1].Backend, "move target", "mv", "k", storage.NullVersionID, "imported", importAt(1))
	if got := read(t, r, "mv", "old", ""); got != "old data" {
		t.Fatalf("old object after the move: %q", got)
	}
}

// lyingImporter claims to import versions (it implements
// storage.VersionImporter) but refuses every import, like a backend whose
// buckets cannot all import.
type lyingImporter struct{ storage.Backend }

func (lyingImporter) NewVersionWriter(context.Context, string, string) (storage.VersionWriter, error) {
	return nil, fmt.Errorf("not this bucket: %w", errors.ErrUnsupported)
}

// A primary that cannot import (a native bucket, a backend without
// storage.VersionImporter, or one refusing) yields errors.ErrUnsupported
// and storage.ErrNotSupported, leaves the gate and the backing healthy,
// and reports no change.
func TestImportIntoNonImportingPrimary(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		backend storage.Backend
	}{
		{"native", newKinded(memory.New(), storage.BucketKindNative, false)},
		{"no importer", noImporter{memory.New()}},
		{"refusing", lyingImporter{memory.New()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := openRouter(t, Options{Backings: []Backing{{Name: "x", Backend: c.backend}}})
			if err := r.CreateBucket(ctx, "bkt", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			var events []Change
			defer r.Subscribe(func(ch Change) { events = append(events, ch) })()
			for _, opts := range []storage.WriteOptions{{}, {Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}}} {
				_, err := r.NewVersionWriterWith(ctx, "bkt", "k", opts)
				if !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, storage.ErrNotSupported) {
					t.Fatalf("NewVersionWriterWith(%+v): %v", opts, err)
				}
				if !strings.Contains(err.Error(), `"bkt"`) {
					t.Errorf("the error does not name the bucket: %v", err)
				}
			}
			if n := gateActive(t, r, "bkt"); n != 0 {
				t.Fatalf("%d writes in flight after refused imports", n)
			}
			if bs := r.Backings(); !bs[0].Healthy {
				t.Fatalf("backing unhealthy after a refused import: %+v", bs[0])
			}
			if len(events) != 0 {
				t.Fatalf("refused imports reported changes: %v", events)
			}
			// Plain writes still work.
			put(t, r, "bkt", "k", "plain", storage.ObjectMeta{})
		})
	}
}

package multi

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// changeLog collects the changes a subscriber receives.
type changeLog struct {
	mu  sync.Mutex
	got []Change
}

func (l *changeLog) add(c Change) {
	l.mu.Lock()
	l.got = append(l.got, c)
	l.mu.Unlock()
}

// take returns the changes received since the last take.
func (l *changeLog) take() []Change {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.got
	l.got = nil
	return out
}

// expect checks that the changes received since the last call are want,
// in order.
func (l *changeLog) expect(t *testing.T, what string, want ...Change) {
	t.Helper()
	if got := l.take(); !slices.Equal(got, want) {
		t.Fatalf("%s: changes %v, want %v", what, got, want)
	}
}

func objChange(bucket, key string) Change {
	return Change{Kind: ChangeObject, Bucket: bucket, Key: key}
}

func cfgChange(bucket, key string) Change {
	return Change{Kind: ChangeBucketConfig, Bucket: bucket, ConfigKey: key}
}

var errRefused = errors.New("refused by the caller")

// Every kind of write through the router reports its change once
// acknowledged; failed writes report nothing.
func TestSubscribeReportsEveryWrite(t *testing.T) {
	for _, mode := range []Mode{ModeSingle, ModeActiveActive} {
		t.Run(string(mode), func(t *testing.T) {
			opts := Options{Backings: memBackings(2)}
			if mode == ModeActiveActive {
				opts.NewBucketMode, opts.NewBucketReplicas = mode, []string{"b"}
			}
			testSubscribeReportsEveryWrite(t, openRouter(t, opts))
		})
	}
}

func testSubscribeReportsEveryWrite(t *testing.T, r *Router) {
	ctx := context.Background()
	var log changeLog
	unsubscribe := r.Subscribe(log.add)
	defer unsubscribe()

	// Buckets and bucket settings.
	if err := r.CreateBucket(ctx, "ev", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "CreateBucket", Change{Kind: ChangeBucketCreated, Bucket: "ev"})
	if err := r.SetBucketVersioning(ctx, "ev", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "SetBucketVersioning", cfgChange("ev", ""))
	if err := r.SetBucketObjectLock(ctx, "ev", true); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "SetBucketObjectLock", cfgChange("ev", ""))
	if err := r.PutBucketConfig(ctx, "ev", storage.ConfigPolicy, []byte(`{"Version":"2012-10-17"}`)); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "PutBucketConfig", cfgChange("ev", storage.ConfigPolicy))
	if err := r.DeleteBucketConfig(ctx, "ev", storage.ConfigPolicy); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "DeleteBucketConfig", cfgChange("ev", storage.ConfigPolicy))

	// Objects.
	v1 := put(t, r, "ev", "k", "one", storage.ObjectMeta{})
	log.expect(t, "object write", objChange("ev", "k"))
	importVersion(t, r, "ev", "imp", "i1", "imported", importAt(1))
	log.expect(t, "import", objChange("ev", "imp"))
	if _, err := r.UpdateObjectMeta(ctx, "ev", "k", v1.VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"a": "1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "UpdateObjectMeta", objChange("ev", "k"))
	if _, err := r.UpdateObjectMetaIf(ctx, "ev", "k", "", func(storage.ObjectInfo) error { return nil },
		func(m *storage.ObjectMeta) error { m.ContentType = "text/plain"; return nil }); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "UpdateObjectMetaIf", objChange("ev", "k"))
	if _, err := r.DeleteObject(ctx, "ev", "k", ""); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "DeleteObject", objChange("ev", "k"))
	if _, err := r.DeleteObjectIf(ctx, "ev", "imp", "i1", func(storage.ObjectInfo) error { return nil }); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "DeleteObjectIf", objChange("ev", "imp"))
	if _, err := r.DeleteObjectChecked(ctx, "ev", "none", "", func(_ storage.ObjectInfo, found bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "DeleteObjectChecked", objChange("ev", "none"))
	up, err := r.CreateMultipartUpload(ctx, "ev", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := r.NewPartWriter(ctx, "ev", "mp", up.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Commit(ctx, storage.PartMeta{ETag: "e"}); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "starting a multipart upload")
	if _, err := r.CompleteMultipartUpload(ctx, "ev", "mp", up.UploadID, []int{1}, storage.ObjectMeta{ETag: "mp-1"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "CompleteMultipartUpload", objChange("ev", "mp"))

	// Failures report nothing.
	if err := r.CreateBucket(ctx, "ev", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("CreateBucket of an existing bucket: %v", err)
	}
	if err := r.DeleteBucket(ctx, "ev"); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Fatalf("DeleteBucket of a bucket with objects: %v", err)
	}
	if err := r.PutBucketConfig(ctx, "nobucket", storage.ConfigPolicy, []byte("{}")); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("PutBucketConfig in a missing bucket: %v", err)
	}
	if err := r.SetBucketVersioning(ctx, "nobucket", storage.VersioningEnabled); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("SetBucketVersioning of a missing bucket: %v", err)
	}
	if _, err := r.UpdateObjectMetaIf(ctx, "ev", "mp", "", func(storage.ObjectInfo) error { return errRefused },
		func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, errRefused) {
		t.Fatalf("refused UpdateObjectMetaIf: %v", err)
	}
	if _, err := r.UpdateObjectMeta(ctx, "ev", "nokey", "", func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("UpdateObjectMeta of a missing key: %v", err)
	}
	if _, err := r.DeleteObjectIf(ctx, "ev", "mp", "", func(storage.ObjectInfo) error { return errRefused }); !errors.Is(err, errRefused) {
		t.Fatalf("refused DeleteObjectIf: %v", err)
	}
	if _, err := r.DeleteObject(ctx, "ev", "mp", "no-such-version"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("DeleteObject of a missing version: %v", err)
	}
	if _, err := r.DeleteObject(ctx, "nobucket", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("DeleteObject in a missing bucket: %v", err)
	}
	w, err := r.NewObjectWriter(ctx, "ev", "refused")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{
		BeforePublish: func(*storage.ObjectMeta, *storage.ObjectMeta, time.Time, func(string) []byte) error {
			return errRefused
		},
	}); !errors.Is(err, errRefused) {
		t.Fatalf("refused publication: %v", err)
	}
	w, err = r.NewObjectWriter(ctx, "ev", "aborted")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, errWriterDone) {
		t.Fatalf("Commit after Abort: %v", err)
	}
	vw := startImport(t, r, "ev", "badid", "x", storage.WriteOptions{})
	if _, err := vw.Commit(ctx, storage.ObjectInfo{VersionID: "a/b", LastModified: importAt(1)}); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("import with an invalid version ID: %v", err)
	}
	vw = startImport(t, r, "ev", "aborted", "x", storage.WriteOptions{})
	if err := vw.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewVersionWriter(ctx, "nobucket", "k"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("NewVersionWriter in a missing bucket: %v", err)
	}
	if err := r.DeleteBucket(ctx, "nobucket"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("DeleteBucket of a missing bucket: %v", err)
	}
	log.expect(t, "failed operations")

	// Deleting a bucket.
	if err := r.CreateBucket(ctx, "gone", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBucket(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	log.expect(t, "creating and deleting a bucket",
		Change{Kind: ChangeBucketCreated, Bucket: "gone"}, Change{Kind: ChangeBucketDeleted, Bucket: "gone"})
}

// Subscribers are independent: unsubscribing one (any number of times)
// stops its calls only.
func TestSubscribeUnsubscribe(t *testing.T) {
	r := openRouter(t, Options{Backings: memBackings(1)})
	ctx := context.Background()
	var first, second changeLog
	unsubFirst := r.Subscribe(first.add)
	unsubSecond := r.Subscribe(second.add)
	defer unsubSecond()
	if nop := r.Subscribe(nil); nop == nil {
		t.Fatal("Subscribe(nil) returned a nil unsubscribe")
	} else {
		nop()
	}
	if err := r.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "b", "k", "v", storage.ObjectMeta{})
	want := []Change{{Kind: ChangeBucketCreated, Bucket: "b"}, objChange("b", "k")}
	first.expect(t, "first subscriber", want...)
	second.expect(t, "second subscriber", want...)

	unsubFirst()
	unsubFirst()
	put(t, r, "b", "k", "w", storage.ObjectMeta{})
	first.expect(t, "first subscriber after unsubscribing")
	second.expect(t, "second subscriber", objChange("b", "k"))

	// Subscribing again works.
	defer r.Subscribe(first.add)()
	put(t, r, "b", "k2", "x", storage.ObjectMeta{})
	first.expect(t, "first subscriber subscribed again", objChange("b", "k2"))
	second.expect(t, "second subscriber", objChange("b", "k2"))
}

// Writes on many goroutines report one change each; subscribing and
// unsubscribing concurrently is safe.
func TestSubscribeConcurrent(t *testing.T) {
	r := openRouter(t, Options{Backings: memBackings(2), NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "c", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	var log changeLog
	defer r.Subscribe(log.add)()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 10 {
				w, err := r.NewObjectWriter(ctx, "c", string(rune('a'+i)))
				if err == nil {
					_, err = w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
				}
				if err != nil {
					t.Error(err)
					return
				}
				r.Subscribe(func(Change) {})()
			}
		})
	}
	wg.Wait()
	if got := log.take(); len(got) != 80 {
		t.Fatalf("%d changes for 80 writes", len(got))
	}
}

// Releasing a bucket reports it deleted.
func TestSubscribeRelease(t *testing.T) {
	k := newKinded(memory.New(), storage.BucketKindNative, false)
	r := openRouter(t, Options{Backings: []Backing{{Name: "k", Kind: "s3", Backend: k}}})
	ctx := context.Background()
	var log changeLog
	defer r.Subscribe(log.add)()
	if err := r.CreateBucket(ctx, "n", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseBucket(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseBucket(ctx, "n"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("second release: %v", err)
	}
	log.expect(t, "creating and releasing a bucket",
		Change{Kind: ChangeBucketCreated, Bucket: "n"}, Change{Kind: ChangeBucketDeleted, Bucket: "n"})
}

func TestChangeKindString(t *testing.T) {
	for k, want := range map[ChangeKind]string{
		ChangeObject: "object", ChangeBucketConfig: "bucket-config", ChangeBucketCreated: "bucket-created",
		ChangeBucketDeleted: "bucket-deleted", 0: "ChangeKind(0)",
	} {
		if got := k.String(); got != want {
			t.Errorf("ChangeKind(%d).String() = %q, want %q", uint8(k), got, want)
		}
	}
}

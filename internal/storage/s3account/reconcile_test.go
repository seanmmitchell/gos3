package s3account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// errCrash simulates a crash at a step: the operation stops there.
var errCrash = errors.New("injected crash")

// crashHooks returns test hooks whose step hook calls *fn when set.
func crashHooks(fn *atomic.Pointer[func(string) error]) hooks {
	h := testHooks()
	h.step = func(name string) error {
		if f := fn.Load(); f != nil {
			return (*f)(name)
		}
		return nil
	}
	return h
}

// crashAt returns a step function failing step name (every call).
func crashAt(name string) *func(string) error {
	f := func(step string) error {
		if step == name {
			return errCrash
		}
		return nil
	}
	return &f
}

// TestReconcileCrashPoints interrupts wrapped bucket creations and
// deletions between the change in the container and the record, and
// checks that the next load repairs the records, in the home and in a
// dedicated container.
func TestReconcileCrashPoints(t *testing.T) {
	for _, where := range []string{"home", "dedicated"} {
		t.Run(where, func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			ctx := context.Background()
			o := homeOptions(t, r, NewClaims(), "home", "")
			var crash atomic.Pointer[func(string) error]
			b := openReadyWith(t, o, crashHooks(&crash))
			target := &storage.BucketTarget{Container: homeID}
			if where == "dedicated" {
				ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "ded", CreateBucket: true})
				noErr(t, "AddContainer", err)
				target = &storage.BucketTarget{Container: ci.ID}
			}
			cs := b.containerByID(target.Container).store
			noErr(t, "create gone", b.CreateBucket(ctx, "gone", storage.CreateBucketOptions{Target: target}))

			// Created in the container, crash before the record.
			crash.Store(crashAt("create-wrapped:stored"))
			wantErr(t, "create orphan", b.CreateBucket(ctx, "orphan", storage.CreateBucketOptions{Target: target, Owner: "bob"}), errCrash)
			// Deleted from the container, crash before the record.
			crash.Store(crashAt("delete-wrapped:deleted"))
			wantErr(t, "delete gone", b.DeleteBucket(ctx, "gone"), errCrash)
			crash.Store(nil)
			if _, err := cs.GetBucket(ctx, "orphan"); err != nil {
				t.Fatalf("orphan not in the container: %v", err)
			}
			if _, err := cs.GetBucket(ctx, "gone"); !errors.Is(err, storage.ErrBucketNotFound) {
				t.Fatalf("gone still in the container: %v", err)
			}
			noErr(t, "Close", b.Close())

			b = openReady(t, o)
			waitContainers(t, b)
			eventually(t, "reconciled buckets", func() bool {
				kinds := bucketKinds(t, b)
				_, gone := kinds["gone"]
				return kinds["orphan"] == storage.BucketKindWrapped && !gone
			})
			info, err := b.GetBucket(ctx, "orphan")
			noErr(t, "GetBucket(orphan)", err)
			if info.Owner != "bob" || info.Location != b.containerByID(target.Container).locationString() {
				t.Fatalf("orphan info %+v", info)
			}
			put(t, b, "orphan", "k", "v")
		})
	}
}

// failingRecords is an AuxStore whose PutRecord fails for keys with a
// prefix.
type failingRecords struct {
	s3store.AuxStore
	fail atomic.Pointer[string]
}

func (f *failingRecords) PutRecord(ctx context.Context, key string, val []byte) error {
	if p := f.fail.Load(); p != nil && strings.HasPrefix(key, *p) {
		return fmt.Errorf("injected record failure: %w", storage.ErrBackingUnavailable)
	}
	return f.AuxStore.PutRecord(ctx, key, val)
}

// TestCreateRollback: a bucket whose record cannot be written is removed
// from its container again (wrapped) or detached and its remote bucket
// deleted (native).
func TestCreateRollback(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	var fr *failingRecords
	h := testHooks()
	h.wrapRecords = func(s s3store.AuxStore) s3store.AuxStore {
		fr = &failingRecords{AuxStore: s}
		return fr
	}
	b := openReadyWith(t, homeOptions(t, r, NewClaims(), "home", ""), h)
	p := bucketKey("rb")
	fr.fail.Store(&p)
	wantErr(t, "create wrapped", b.CreateBucket(ctx, "rb", storage.CreateBucketOptions{}), storage.ErrBackingUnavailable)
	if _, err := b.home.GetBucket(ctx, "rb"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("wrapped bucket not rolled back: %v", err)
	}
	native := &storage.BucketTarget{Kind: storage.BucketKindNative, RemoteBucket: "rb-remote"}
	wantErr(t, "create native", b.CreateBucket(ctx, "rb", storage.CreateBucketOptions{Target: native}), storage.ErrBackingUnavailable)
	if remoteHasBucket(t, r, "rb-remote") {
		t.Fatal("created remote bucket not deleted after the failed creation")
	}
	if hs := b.holders("rb-remote"); len(hs) != 0 {
		t.Fatalf("claim kept: %v", hs)
	}
	fr.fail.Store(nil)
	noErr(t, "create native", b.CreateBucket(ctx, "rb", storage.CreateBucketOptions{Target: native}))
	if k := bucketKinds(t, b)["rb"]; k != storage.BucketKindNative {
		t.Fatalf("rb kind %q", k)
	}
}

// --- legacy homes ---

// legacyStore opens an s3store as builds before s3 account backings did.
func legacyStore(t testing.TB, r *s3clienttest.Remote, bucket, prefix string) *s3store.Backend {
	t.Helper()
	conn := testConn(t, r)
	o := s3store.Options{Name: "legacy", Client: conn.Client, Bucket: bucket, Prefix: prefix, TempDir: t.TempDir()}
	fastStores(&o)
	st, err := s3store.Open(o)
	noErr(t, "s3store.Open", err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	noErr(t, "WaitReady", st.WaitReady(ctx))
	return st
}

// legacyPlainHome writes a plain legacy home: buckets with objects and
// configurations, and no records.
func legacyPlainHome(t testing.TB, r *s3clienttest.Remote, buckets []string) {
	ctx := context.Background()
	st := legacyStore(t, r, "home", "legacy/")
	for _, n := range buckets {
		noErr(t, "CreateBucket", st.CreateBucket(ctx, n, storage.CreateBucketOptions{Owner: "o-" + n}))
		noErr(t, "PutBucketConfig", st.PutBucketConfig(ctx, n, storage.ConfigPolicy, []byte("policy-"+n)))
		put(t, st, n, "obj", "data-"+n)
	}
	noErr(t, "Close", st.Close())
}

// TestMigratePlainHome opens a plain legacy home, interrupted after each
// adoption, and checks that the next load completes it.
func TestMigratePlainHome(t *testing.T) {
	buckets := []string{"one", "three", "two"}
	for crashStep := 0; crashStep <= len(buckets); crashStep++ {
		t.Run(fmt.Sprintf("crash%d", crashStep), func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			legacyPlainHome(t, r, buckets)
			o := homeOptions(t, r, NewClaims(), "home", "legacy/")
			if crashStep > 0 {
				var crash atomic.Pointer[func(string) error]
				var n atomic.Int64
				f := func(step string) error {
					if step == "reconcile:adopt" && n.Add(1) >= int64(crashStep) {
						return errCrash
					}
					return nil
				}
				crash.Store(&f)
				b, err := open(o, crashHooks(&crash))
				noErr(t, "open", err)
				eventually(t, "the crash", func() bool { return n.Load() >= int64(crashStep) })
				noErr(t, "Close", b.Close())
				if b.isReady() {
					t.Fatal("ready although the migration was interrupted")
				}
			}
			b := openReady(t, o)
			ctx := context.Background()
			kinds := bucketKinds(t, b)
			for _, n := range buckets {
				if kinds[n] != storage.BucketKindWrapped {
					t.Fatalf("bucket %s: kind %q (all: %v)", n, kinds[n], kinds)
				}
				if got := read(t, b, n, "obj"); got != "data-"+n {
					t.Fatalf("%s/obj = %q", n, got)
				}
				doc, err := b.GetBucketConfig(ctx, n, storage.ConfigPolicy)
				noErr(t, "GetBucketConfig", err)
				if string(doc) != "policy-"+n {
					t.Fatalf("policy of %s = %q", n, doc)
				}
				info, err := b.GetBucket(ctx, n)
				noErr(t, "GetBucket", err)
				if info.Owner != "o-"+n {
					t.Fatalf("owner of %s: %q", n, info.Owner)
				}
			}
			if len(kinds) != len(buckets) {
				t.Fatalf("buckets %v", kinds)
			}
		})
	}
}

// legacyGatewayHome writes the home of a legacy gateway backing: bucket
// records with configurations in an s3store whose marker says gateway, and
// remote buckets of the same names holding the objects.
func legacyGatewayHome(t testing.TB, r *s3clienttest.Remote, buckets []string) {
	ctx := context.Background()
	st := legacyStore(t, r, "home", "gw/")
	c := r.Client(t)
	for _, n := range buckets {
		noErr(t, "CreateBucket", st.CreateBucket(ctx, n, storage.CreateBucketOptions{Owner: "o-" + n}))
		noErr(t, "PutBucketConfig", st.PutBucketConfig(ctx, n, storage.ConfigPolicy, []byte("policy-"+n)))
		noErr(t, "PutBucketConfig", st.PutBucketConfig(ctx, n, storage.ConfigTagging, []byte("<Tagging/>")))
		r.CreateBucket(t, n)
		remotePut(t, c, n, "obj", "data-"+n)
	}
	noErr(t, "Close", st.Close())
	// Mark the store as a legacy gateway's.
	body, _, err := c.GetObject(ctx, "home", "gw/gos3.json", s3client.GetOptions{})
	noErr(t, "GetObject(marker)", err)
	data, err := io.ReadAll(body)
	body.Close()
	noErr(t, "read marker", err)
	var m map[string]any
	noErr(t, "decode marker", json.Unmarshal(data, &m))
	m["gateway"] = true
	data, _ = json.Marshal(m)
	_, err = c.PutObject(ctx, "home", "gw/gos3.json", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{})
	noErr(t, "PutObject(marker)", err)
}

// TestMigrateGatewayHome migrates a legacy gateway home, interrupted at
// every step in turn, and checks that the next load completes the
// migration: native buckets bound to the remote buckets of their names (as
// adopted), their configurations in the records, the home's buckets
// removed and the marker's gateway flag cleared.
func TestMigrateGatewayHome(t *testing.T) {
	buckets := []string{"gwa", "gwb"}
	const steps = 2*(2+2) + 1 // per bucket: 2 configs, record, delete; then clear
	for crashStep := 0; crashStep <= steps; crashStep++ {
		t.Run(fmt.Sprintf("crash%d", crashStep), func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			legacyGatewayHome(t, r, buckets)
			o := homeOptions(t, r, NewClaims(), "home", "gw/")
			if crashStep > 0 {
				var crash atomic.Pointer[func(string) error]
				var n atomic.Int64
				f := func(step string) error {
					if strings.HasPrefix(step, "migrate:") && n.Add(1) >= int64(crashStep) {
						return errCrash
					}
					return nil
				}
				crash.Store(&f)
				b, err := open(o, crashHooks(&crash))
				noErr(t, "open", err)
				eventually(t, "the crash", func() bool { return n.Load() >= int64(crashStep) })
				noErr(t, "Close", b.Close())
			}
			b := openReady(t, o)
			ctx := context.Background()
			if b.home.Marker().Gateway {
				t.Fatal("the gateway flag was not cleared")
			}
			left, err := b.home.ListBuckets(ctx)
			noErr(t, "home.ListBuckets", err)
			if len(left) != 0 {
				t.Fatalf("buckets left in the home: %v", left)
			}
			kinds := bucketKinds(t, b)
			if len(kinds) != len(buckets) {
				t.Fatalf("buckets %v", kinds)
			}
			for _, n := range buckets {
				if kinds[n] != storage.BucketKindNative {
					t.Fatalf("bucket %s: kind %q", n, kinds[n])
				}
				info, err := b.GetBucket(ctx, n)
				noErr(t, "GetBucket", err)
				if info.Owner != "o-"+n || info.Location != "s3://"+n {
					t.Fatalf("info of %s: %+v", n, info)
				}
				b.mu.RLock()
				rec := b.reg.buckets[n].bucketRecord
				b.mu.RUnlock()
				if rec.Created || rec.Remote != n {
					t.Fatalf("record of %s: %+v", n, rec)
				}
				for key, want := range map[string]string{storage.ConfigPolicy: "policy-" + n, storage.ConfigTagging: "<Tagging/>"} {
					doc, err := b.GetBucketConfig(ctx, n, key)
					noErr(t, "GetBucketConfig", err)
					if string(doc) != want {
						t.Fatalf("%s of %s = %q", key, n, doc)
					}
				}
				if got := read(t, b, n, "obj"); got != "data-"+n {
					t.Fatalf("%s/obj = %q", n, got)
				}
			}
			// Deleting a migrated bucket never deletes its remote bucket.
			_, err = b.DeleteObject(ctx, "gwa", "obj", "")
			noErr(t, "DeleteObject", err)
			noErr(t, "DeleteBucket", b.DeleteBucket(ctx, "gwa"))
			if !remoteHasBucket(t, r, "gwa") {
				t.Fatal("deleting a migrated bucket deleted its remote bucket")
			}
		})
	}
}

// TestNativeCrashPoints interrupts native bucket creations and deletions
// between the remote change and the record: a creation leaves an unused
// remote bucket (which can be adopted), while a deletion leaves a
// tombstone that is discarded on restart.
func TestNativeCrashPoints(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	o := localOptions(t, r, NewClaims(), t.TempDir())
	var crash atomic.Pointer[func(string) error]
	b := openReadyWith(t, o, crashHooks(&crash))
	noErr(t, "create", b.CreateBucket(ctx, "doomed", storage.CreateBucketOptions{}))
	crash.Store(crashAt("create-native:attached"))
	wantErr(t, "create", b.CreateBucket(ctx, "limbo", storage.CreateBucketOptions{}), errCrash)
	crash.Store(crashAt("delete-native:detached"))
	wantErr(t, "delete", b.DeleteBucket(ctx, "doomed"), errCrash)
	crash.Store(nil)
	noErr(t, "Close", b.Close())
	// A different owner can acquire the globally reusable remote name
	// before gos3 restarts. The tombstone must not expose or delete it.
	r.CreateBucket(t, "doomed")

	b = openReady(t, o)
	kinds := bucketKinds(t, b)
	if _, ok := kinds["limbo"]; ok || len(kinds) != 0 {
		t.Fatalf("buckets %v", kinds)
	}
	if !remoteHasBucket(t, r, "limbo") || !remoteHasBucket(t, r, "doomed") {
		t.Fatal("remote buckets not as expected")
	}
	wantErr(t, "deleted bucket stays gone", b.DeleteBucket(ctx, "doomed"), storage.ErrBucketNotFound)
	wantErr(t, "create over the leftover", b.CreateBucket(ctx, "limbo", storage.CreateBucketOptions{}), storage.ErrRemoteBucketExists)
	noErr(t, "adopt the leftover", b.CreateBucket(ctx, "limbo", storage.CreateBucketOptions{Target: nativeTarget("", true)}))
}

type failingNativeState struct {
	native.StateStore
	fail     atomic.Bool
	failRead atomic.Bool
}

func (s *failingNativeState) Records(ctx context.Context) (map[string][]byte, error) {
	if s.failRead.Load() {
		return nil, errCrash
	}
	return s.StateStore.Records(ctx)
}

func (s *failingNativeState) DeleteRecords(ctx context.Context, prefix string) error {
	if s.fail.Load() {
		return errCrash
	}
	return s.StateStore.DeleteRecords(ctx, prefix)
}

func TestFailedNativeDetachPreservesRecoverableRecord(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	o := localOptions(t, r, NewClaims(), t.TempDir())
	var state *failingNativeState
	h := testHooks()
	h.openNative = func(opts native.Options) (NativeStore, error) {
		state = &failingNativeState{StateStore: opts.State}
		opts.State = state
		return native.Open(opts)
	}
	b := openReadyWith(t, o, h)
	noErr(t, "create", b.CreateBucket(ctx, "recover", storage.CreateBucketOptions{}))
	state.fail.Store(true)
	wantErr(t, "delete with failed local cleanup", b.DeleteBucket(ctx, "recover"), errCrash)
	if !remoteHasBucket(t, r, "recover") {
		t.Fatal("failed cleanup deleted the remote bucket")
	}
	state.fail.Store(false)
	noErr(t, "close", b.Close())

	b = openReady(t, o)
	b.mu.RLock()
	e := b.reg.buckets["recover"]
	pending := e != nil && e.Deleting && !e.Detached
	b.mu.RUnlock()
	if !pending {
		t.Fatal("restart discarded the interrupted deletion record")
	}
	_, err := b.GetBucket(ctx, "recover")
	wantErr(t, "pending bucket is unavailable", err, storage.ErrBucketUnavailable)
	noErr(t, "release pending record", b.ReleaseBucket(ctx, "recover"))
	if !remoteHasBucket(t, r, "recover") {
		t.Fatal("release of pending record deleted the remote bucket")
	}
	noErr(t, "adopt released bucket", b.CreateBucket(ctx, "recover", storage.CreateBucketOptions{Target: nativeTarget("", true)}))
}

func TestInterruptedNativeDeletionReleasableImmediatelyAndAfterRouterRestart(t *testing.T) {
	remote := s3clienttest.Start(t)
	ctx := context.Background()
	o := localOptions(t, remote, NewClaims(), t.TempDir())
	var state *failingNativeState
	h := testHooks()
	h.openNative = func(opts native.Options) (NativeStore, error) {
		state = &failingNativeState{StateStore: opts.State}
		opts.State = state
		return native.Open(opts)
	}
	b := openReadyWith(t, o, h)
	noErr(t, "create immediate", b.CreateBucket(ctx, "immediate", storage.CreateBucketOptions{}))
	noErr(t, "create", b.CreateBucket(ctx, "recover", storage.CreateBucketOptions{}))
	path := filepath.Join(t.TempDir(), "placements.json")
	openRouter := func(backing *Backend) *multi.Router {
		t.Helper()
		router, err := multi.Open(ctx, multi.Options{Backings: []multi.Backing{{Name: "remote", Backend: backing}}, StatePath: path})
		noErr(t, "open router", err)
		return router
	}
	router := openRouter(b)
	state.fail.Store(true)
	wantErr(t, "delete immediate with failed local cleanup", router.DeleteBucket(ctx, "immediate"), errCrash)
	state.fail.Store(false)
	noErr(t, "release before restart", router.ReleaseBucket(ctx, "immediate"))
	if !remoteHasBucket(t, remote, "immediate") {
		t.Fatal("immediate release deleted the remote bucket")
	}
	noErr(t, "adopt under a different name", router.CreateBucket(ctx, "renamed", storage.CreateBucketOptions{Target: nativeTarget("immediate", true)}))
	noErr(t, "release renamed bucket", router.ReleaseBucket(ctx, "renamed"))
	state.fail.Store(true)
	wantErr(t, "delete with failed local cleanup", router.DeleteBucket(ctx, "recover"), errCrash)
	state.fail.Store(false)
	noErr(t, "close router", router.Close())

	b = openReady(t, o)
	router = openRouter(b)
	infos, err := router.ListBuckets(ctx)
	noErr(t, "list buckets", err)
	if len(infos) != 0 {
		t.Fatalf("interrupted deletion became visible: %+v", infos)
	}
	_, err = router.GetBucket(ctx, "recover")
	wantErr(t, "interrupted deletion unavailable", err, storage.ErrBucketUnavailable)
	noErr(t, "release through router", router.ReleaseBucket(ctx, "recover"))
	if !remoteHasBucket(t, remote, "recover") {
		t.Fatal("router release deleted the remote bucket")
	}
	noErr(t, "adopt through router", router.CreateBucket(ctx, "recover", storage.CreateBucketOptions{Target: nativeTarget("", true)}))
	noErr(t, "close recovered router", router.Close())
}

func TestInterruptedNativeReleaseClearsDurableUploads(t *testing.T) {
	remote := s3clienttest.Start(t)
	proxy, requests := countingProxy(t, remote)
	ctx := context.Background()
	o := localOptions(t, remote, NewClaims(), t.TempDir())
	o.Conn = proxyConn(t, proxy)
	var state *failingNativeState
	h := testHooks()
	h.openNative = func(opts native.Options) (NativeStore, error) {
		state = &failingNativeState{StateStore: opts.State}
		opts.State = state
		return native.Open(opts)
	}
	b := openReadyWith(t, o, h)
	for _, name := range []string{"old", "neighbor"} {
		noErr(t, "create "+name, b.CreateBucket(ctx, name, storage.CreateBucketOptions{}))
	}
	old, err := b.CreateMultipartUpload(ctx, "old", "key", storage.ObjectMeta{}, "")
	noErr(t, "create old upload", err)
	w, err := b.NewPartWriter(ctx, "old", "key", old.UploadID, 1)
	noErr(t, "old part writer", err)
	_, err = w.Write([]byte("old part"))
	noErr(t, "write old part", err)
	_, err = w.Commit(ctx, storage.PartMeta{})
	noErr(t, "commit old part", err)
	neighbor, err := b.CreateMultipartUpload(ctx, "neighbor", "keep", storage.ObjectMeta{}, "")
	noErr(t, "create neighbor upload", err)
	state.fail.Store(true)
	wantErr(t, "delete with failed native cleanup", b.DeleteBucket(ctx, "old"), errCrash)
	state.fail.Store(false)
	state.failRead.Store(true)
	wantErr(t, "release with failed state read", b.ReleaseBucket(ctx, "old"), errCrash)
	state.failRead.Store(false)
	state.fail.Store(true)
	wantErr(t, "release with failed upload deletion", b.ReleaseBucket(ctx, "old"), errCrash)
	state.fail.Store(false)
	_, err = b.GetBucket(ctx, "old")
	wantErr(t, "failed release kept tombstone", err, storage.ErrBucketUnavailable)
	before := requests.Load()
	noErr(t, "release after state recovery", b.ReleaseBucket(ctx, "old"))
	if got := requests.Load() - before; got != 0 {
		t.Fatalf("recovery release sent %d remote requests", got)
	}
	noErr(t, "rebind old name", b.CreateBucket(ctx, "old", storage.CreateBucketOptions{Target: nativeTarget("", true)}))
	uploads, err := b.ListMultipartUploads(ctx, "old")
	noErr(t, "list rebound uploads", err)
	if len(uploads) != 0 {
		t.Fatalf("old uploads returned after rebinding: %+v", uploads)
	}
	_, err = b.CompleteMultipartUpload(ctx, "old", "key", old.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
	wantErr(t, "complete old upload", err, storage.ErrUploadNotFound)
	uploads, err = b.ListMultipartUploads(ctx, "neighbor")
	noErr(t, "list neighbor uploads", err)
	if len(uploads) != 1 || uploads[0].UploadID != neighbor.UploadID {
		t.Fatalf("neighbor uploads changed: %+v", uploads)
	}
	noErr(t, "close", b.Close())
	b = openReadyWith(t, o, h)
	uploads, err = b.ListMultipartUploads(ctx, "old")
	noErr(t, "list uploads after restart", err)
	if len(uploads) != 0 {
		t.Fatalf("old uploads returned after restart: %+v", uploads)
	}
	noErr(t, "release rebound name", b.ReleaseBucket(ctx, "old"))
	noErr(t, "adopt under different name", b.CreateBucket(ctx, "renamed", storage.CreateBucketOptions{Target: nativeTarget("old", true)}))
	uploads, err = b.ListMultipartUploads(ctx, "renamed")
	noErr(t, "list renamed uploads", err)
	if len(uploads) != 0 {
		t.Fatalf("old uploads returned under a different name: %+v", uploads)
	}
	noErr(t, "create restart bucket", b.CreateBucket(ctx, "restart", storage.CreateBucketOptions{}))
	restarted, err := b.CreateMultipartUpload(ctx, "restart", "key", storage.ObjectMeta{}, "")
	noErr(t, "create restart upload", err)
	state.fail.Store(true)
	wantErr(t, "interrupt restart deletion", b.DeleteBucket(ctx, "restart"), errCrash)
	state.fail.Store(false)
	noErr(t, "close before pending release", b.Close())
	b = openReady(t, o)
	_, err = b.GetBucket(ctx, "restart")
	wantErr(t, "restarted tombstone stayed hidden", err, storage.ErrBucketUnavailable)
	before = requests.Load()
	noErr(t, "release after restart", b.ReleaseBucket(ctx, "restart"))
	if got := requests.Load() - before; got != 0 {
		t.Fatalf("release after restart sent %d remote requests", got)
	}
	noErr(t, "rebind after restart", b.CreateBucket(ctx, "restart", storage.CreateBucketOptions{Target: nativeTarget("", true)}))
	uploads, err = b.ListMultipartUploads(ctx, "restart")
	noErr(t, "list uploads after pending release", err)
	if len(uploads) != 0 {
		t.Fatalf("restarted upload returned after release: %+v", uploads)
	}
	_, err = b.CompleteMultipartUpload(ctx, "restart", "key", restarted.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
	wantErr(t, "complete restarted upload", err, storage.ErrUploadNotFound)
}

func TestHomeReloadClearsCompletedNativeDeletion(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	var fd *failingDeletes
	h := testHooks()
	h.wrapRecords = func(s s3store.AuxStore) s3store.AuxStore {
		fd = &failingDeletes{AuxStore: s}
		return fd
	}
	b := openReadyWith(t, homeOptions(t, r, NewClaims(), "home", ""), h)
	noErr(t, "create native", b.CreateBucket(ctx, "gone", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	key := bucketKey("gone")
	fd.fail.Store(&key)
	wantErr(t, "delete record during home outage", b.DeleteBucket(ctx, "gone"), storage.ErrBackingUnavailable)
	b.mu.RLock()
	e := b.reg.buckets["gone"]
	completed := e != nil && e.Deleting && e.Detached
	epoch := b.homeEpoch
	b.mu.RUnlock()
	if !completed {
		t.Fatal("failed record deletion did not leave a completed tombstone")
	}
	b.reload(epoch + 1)
	b.mu.RLock()
	_, exists := b.reg.buckets["gone"]
	b.mu.RUnlock()
	if exists {
		t.Fatal("home reload kept a completed native deletion")
	}
	if remoteHasBucket(t, r, "gone") {
		t.Fatal("remote bucket was not deleted")
	}
	noErr(t, "reuse bucket name", b.CreateBucket(ctx, "gone", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
}

func TestCompletedNativeDeletionReleasableWithoutRestart(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	var fd *failingDeletes
	h := testHooks()
	h.wrapRecords = func(s s3store.AuxStore) s3store.AuxStore {
		fd = &failingDeletes{AuxStore: s}
		return fd
	}
	b := openReadyWith(t, localOptions(t, r, NewClaims(), t.TempDir()), h)
	noErr(t, "create", b.CreateBucket(ctx, "gone", storage.CreateBucketOptions{}))
	path := filepath.Join(t.TempDir(), "placements.json")
	router, err := multi.Open(ctx, multi.Options{Backings: []multi.Backing{{Name: "remote", Backend: b}}, StatePath: path})
	noErr(t, "open router", err)
	key := bucketKey("gone")
	fd.fail.Store(&key)
	wantErr(t, "delete record", router.DeleteBucket(ctx, "gone"), storage.ErrBackingUnavailable)
	b.mu.RLock()
	e := b.reg.buckets["gone"]
	completed := e != nil && e.Deleting && e.Detached
	b.mu.RUnlock()
	if !completed {
		t.Fatal("failed record deletion did not leave a completed tombstone")
	}
	noErr(t, "release completed deletion", router.ReleaseBucket(ctx, "gone"))
	noErr(t, "reuse bucket name", router.CreateBucket(ctx, "gone", storage.CreateBucketOptions{}))
	noErr(t, "close router", router.Close())
}

// TestReloadAfterHomeSession: when the home was reloaded (a new session),
// the records are read again and changes made meanwhile (by another
// server) are applied.
func TestReloadAfterHomeSession(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	r.CreateBucket(t, "ext-remote")
	ctx := context.Background()
	claims := NewClaims()
	b := openReady(t, homeOptions(t, r, claims, "home", ""))
	noErr(t, "create", b.CreateBucket(ctx, "dropped", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	// Another server's changes, written to the records directly.
	b.adminMu.Lock()
	noErr(t, "PutRecord", b.records.PutRecord(ctx, bucketKey("ext"), recordJSON(bucketRecord{
		Kind: storage.BucketKindNative, Remote: "ext-remote", Info: storage.BucketInfo{Owner: "eve"}})))
	noErr(t, "PutRecord", b.records.PutRecord(ctx, configKey("ext", storage.ConfigPolicy), []byte("ext-policy")))
	noErr(t, "DeleteRecords", b.records.DeleteRecords(ctx, bucketKey("dropped")))
	b.adminMu.Unlock()
	b.mu.RLock()
	epoch := b.homeEpoch
	b.mu.RUnlock()
	b.reload(epoch + 1)
	kinds := bucketKinds(t, b)
	if _, ok := kinds["dropped"]; ok || kinds["ext"] != storage.BucketKindNative {
		t.Fatalf("buckets after reload %v", kinds)
	}
	doc, err := b.GetBucketConfig(ctx, "ext", storage.ConfigPolicy)
	noErr(t, "GetBucketConfig", err)
	if string(doc) != "ext-policy" {
		t.Fatalf("policy %q", doc)
	}
	if hs := claims.Holders(r.URL, "dropped"); len(hs) != 0 {
		t.Fatalf("claim of the dropped bucket kept: %v", hs)
	}
	if hs := claims.Holders(r.URL, "ext-remote"); len(hs) != 1 || hs[0].GosBucket != "ext" {
		t.Fatalf("claims of ext-remote: %v", hs)
	}
}

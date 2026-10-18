package s3account

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func nativeTarget(remote string, adopt bool) *storage.BucketTarget {
	return &storage.BucketTarget{Kind: storage.BucketKindNative, RemoteBucket: remote, Adopt: adopt}
}

// TestNativeDeleteCreatedVsAdopted: deleting a native bucket deletes the
// remote bucket gos3 created, never an adopted one.
func TestNativeDeleteCreatedVsAdopted(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "adopted-remote")
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	noErr(t, "create", b.CreateBucket(ctx, "created", storage.CreateBucketOptions{}))
	noErr(t, "adopt", b.CreateBucket(ctx, "adopted", storage.CreateBucketOptions{Target: nativeTarget("adopted-remote", true)}))
	if !remoteHasBucket(t, r, "created") {
		t.Fatal("the remote bucket was not created")
	}
	// Creating over an existing remote bucket is refused (adopting is an
	// explicit choice).
	r.CreateBucket(t, "unclaimed")
	wantErr(t, "create over existing", b.CreateBucket(ctx, "again", storage.CreateBucketOptions{Target: nativeTarget("unclaimed", false)}),
		storage.ErrRemoteBucketExists)
	wantErr(t, "create over claimed", b.CreateBucket(ctx, "again", storage.CreateBucketOptions{Target: nativeTarget("adopted-remote", true)}),
		ErrClaimed)
	put(t, b, "created", "k", "v")
	wantErr(t, "delete non-empty", b.DeleteBucket(ctx, "created"), storage.ErrBucketNotEmpty)
	_, err := b.DeleteObject(ctx, "created", "k", "")
	noErr(t, "DeleteObject", err)
	noErr(t, "delete created", b.DeleteBucket(ctx, "created"))
	noErr(t, "delete adopted", b.DeleteBucket(ctx, "adopted"))
	if remoteHasBucket(t, r, "created") {
		t.Fatal("deleting a created native bucket kept its remote bucket")
	}
	if !remoteHasBucket(t, r, "adopted-remote") {
		t.Fatal("deleting an adopted native bucket deleted its remote bucket")
	}
	if kinds := bucketKinds(t, b); len(kinds) != 0 {
		t.Fatalf("buckets left: %v", kinds)
	}
	if hs := b.holders("adopted-remote"); len(hs) != 0 {
		t.Fatalf("claims left: %v", hs)
	}
}

// countingRemote starts a remote counting the DELETE requests it serves.
func countingRemote(t *testing.T) (*s3clienttest.Remote, *atomic.Int64) {
	var deletes atomic.Int64
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodDelete {
				deletes.Add(1)
			}
			h.ServeHTTP(w, req)
		})
	}})
	return r, &deletes
}

// TestNativeReleaseNoRemoteDelete: releasing a native bucket sends no
// DELETE to the remote; the remote bucket and its objects stay and can be
// adopted again. Wrapped buckets cannot be released.
func TestNativeReleaseNoRemoteDelete(t *testing.T) {
	r, deletes := countingRemote(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	b := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))
	noErr(t, "create", b.CreateBucket(ctx, "rel", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	put(t, b, "rel", "k", "kept")
	noErr(t, "create wrapped", b.CreateBucket(ctx, "wr", storage.CreateBucketOptions{}))
	wantErr(t, "release wrapped", b.ReleaseBucket(ctx, "wr"), storage.ErrNotSupported)
	before := deletes.Load()
	noErr(t, "ReleaseBucket", b.ReleaseBucket(ctx, "rel"))
	if n := deletes.Load() - before; n != 0 {
		t.Fatalf("release sent %d DELETE requests", n)
	}
	if _, ok := bucketKinds(t, b)["rel"]; ok {
		t.Fatal("released bucket still listed")
	}
	wantErr(t, "release again", b.ReleaseBucket(ctx, "rel"), storage.ErrBucketNotFound)
	region, err := b.CheckAdoptable(ctx, "rel")
	noErr(t, "CheckAdoptable", err)
	noErr(t, "adopt", b.CreateBucket(ctx, "back", storage.CreateBucketOptions{Target: &storage.BucketTarget{
		Kind: storage.BucketKindNative, RemoteBucket: "rel", RemoteRegion: region, Adopt: true}}))
	if got := read(t, b, "back", "k"); got != "kept" {
		t.Fatalf("back/k = %q", got)
	}
}

// TestBucketUnavailable: a native bucket whose remote bucket was deleted
// and the buckets of a container that cannot load answer
// storage.ErrBucketUnavailable while the backing stays healthy.
func TestBucketUnavailable(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	o := homeOptions(t, r, NewClaims(), "home", "")
	b := openReady(t, o)
	noErr(t, "create native", b.CreateBucket(ctx, "vanishing", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "brittle", CreateBucket: true})
	noErr(t, "AddContainer", err)
	noErr(t, "create wrapped", b.CreateBucket(ctx, "inside", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	put(t, b, "inside", "k", "v")
	noErr(t, "create home bucket", b.CreateBucket(ctx, "fine", storage.CreateBucketOptions{}))
	noErr(t, "Close", b.Close())

	// The native bucket's remote bucket disappears; the container loses
	// its marker.
	noErr(t, "remote DeleteBucket", r.Backend.DeleteBucket(ctx, "vanishing"))
	_, err = r.Backend.DeleteObject(ctx, "brittle", "gos3.json", "")
	noErr(t, "remote DeleteObject", err)

	b = openReady(t, o)
	eventually(t, "the container failing", func() bool {
		for _, c := range b.Containers(ctx) {
			if c.ID == ci.ID {
				return c.State == "failed"
			}
		}
		return false
	})
	// (A HEAD answer has no body, so only listings and writes can tell a
	// vanished remote bucket from a missing object.)
	err = b.WalkObjects(ctx, "vanishing", "", "", func(storage.ObjectInfo) error { return nil })
	wantErr(t, "native WalkObjects", err, storage.ErrBucketUnavailable)
	_, err = b.NewObjectWriter(ctx, "inside", "k2")
	wantErr(t, "container NewObjectWriter", err, storage.ErrBucketUnavailable)
	_, err = b.GetObjectInfo(ctx, "inside", "k", "")
	wantErr(t, "container GetObjectInfo", err, storage.ErrBucketUnavailable)
	noErr(t, "ProbeHealth", b.ProbeHealth(ctx))
	kinds := bucketKinds(t, b)
	if kinds["inside"] != storage.BucketKindWrapped || kinds["vanishing"] != storage.BucketKindNative {
		t.Fatalf("buckets %v", kinds)
	}
	put(t, b, "fine", "k", "v")
	if st := b.Status(); !st.Ready || st.Err != nil {
		t.Fatalf("status %+v", st)
	}
	// The deleted native bucket can still be deleted (its remote bucket
	// is gone).
	noErr(t, "DeleteBucket(vanishing)", b.DeleteBucket(ctx, "vanishing"))
}

// TestContainerForbidden: a container whose remote bucket answers 403
// makes its buckets unavailable, not the backing.
func TestContainerForbidden(t *testing.T) {
	var deny atomic.Bool
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if deny.Load() && strings.HasPrefix(req.URL.Path, "/locked/") {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
				return
			}
			h.ServeHTTP(w, req)
		})
	}})
	r.CreateBucket(t, "home")
	ctx := context.Background()
	b := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "locked", CreateBucket: true})
	noErr(t, "AddContainer", err)
	noErr(t, "create", b.CreateBucket(ctx, "lb", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	w, err := b.NewObjectWriter(ctx, "lb", "k")
	noErr(t, "NewObjectWriter", err)
	_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	deny.Store(true)
	_, err = w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	wantErr(t, "Commit with a denied container", err, storage.ErrBucketUnavailable)
	deny.Store(false)
}

// TestNativeSettings: versioning, object lock and the object lock
// configuration of a native bucket go to the remote bucket; commits see
// the recorded configuration documents.
func TestNativeSettings(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	c := r.Client(t)
	noErr(t, "create", b.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}))
	info, err := b.GetBucket(ctx, "locked")
	noErr(t, "GetBucket", err)
	if !info.ObjectLockEnabled || info.Versioning != storage.VersioningEnabled {
		t.Fatalf("info %+v", info)
	}
	if st, err := c.GetBucketVersioning(ctx, "locked"); err != nil || st != "Enabled" {
		t.Fatalf("remote versioning %q, %v", st, err)
	}
	doc := `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
		`<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>3</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	noErr(t, "PutBucketConfig(object-lock)", b.PutBucketConfig(ctx, "locked", storage.ConfigObjectLock, []byte(doc)))
	cfg, found, err := c.GetObjectLockConfiguration(ctx, "locked")
	noErr(t, "remote GetObjectLockConfiguration", err)
	if !found || cfg.Mode != "GOVERNANCE" || cfg.Days != 3 {
		t.Fatalf("remote lock configuration %+v (found %v)", cfg, found)
	}
	// BeforePublish sees the records' configuration documents.
	noErr(t, "PutBucketConfig(policy)", b.PutBucketConfig(ctx, "locked", storage.ConfigPolicy, []byte("the-policy")))
	w, err := b.NewObjectWriter(ctx, "locked", "k")
	noErr(t, "NewObjectWriter", err)
	_, _ = io.WriteString(w, "data")
	var seen string
	_, err = w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{
		BeforePublish: func(meta, _ *storage.ObjectMeta, _ time.Time, config func(string) []byte) error {
			seen = string(config(storage.ConfigPolicy))
			return nil
		},
	})
	noErr(t, "Commit", err)
	if seen != "the-policy" {
		t.Fatalf("BeforePublish saw policy %q", seen)
	}

	noErr(t, "create plain", b.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}))
	noErr(t, "SetBucketVersioning", b.SetBucketVersioning(ctx, "plain", storage.VersioningEnabled))
	if st, err := c.GetBucketVersioning(ctx, "plain"); err != nil || st != "Enabled" {
		t.Fatalf("remote versioning %q, %v", st, err)
	}
	noErr(t, "SetBucketVersioning", b.SetBucketVersioning(ctx, "plain", storage.VersioningSuspended))
	info, err = b.GetBucket(ctx, "plain")
	noErr(t, "GetBucket", err)
	if info.Versioning != storage.VersioningSuspended {
		t.Fatalf("recorded versioning %q", info.Versioning)
	}
	if err := b.SetBucketVersioning(ctx, "plain", storage.VersioningUnversioned); err == nil {
		t.Fatal("a versioned bucket became unversioned")
	}
}

// TestNativeDrift: a remote bucket whose versioning was changed by another
// tool is refreshed when the native store reports drift.
func TestNativeDrift(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	h := testHooks()
	h.driftInterval = time.Millisecond
	b := openReadyWith(t, localOptions(t, r, NewClaims(), t.TempDir()), h)
	noErr(t, "create", b.CreateBucket(ctx, "drifty", storage.CreateBucketOptions{}))
	noErr(t, "remote PutBucketVersioning", r.Client(t).PutBucketVersioning(ctx, "drifty", "Enabled"))
	b.onDrift("drifty", "test")
	eventually(t, "the refresh", func() bool {
		info, err := b.GetBucket(ctx, "drifty")
		return err == nil && info.Versioning == storage.VersioningEnabled
	})
}

// TestNativeWriteOptions: native buckets store data as-is: compression is
// not applied, encryption and imports are refused.
func TestNativeWriteOptions(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	noErr(t, "create", b.CreateBucket(ctx, "asis", storage.CreateBucketOptions{}))
	w, err := b.NewObjectWriterWith(ctx, "asis", "k", storage.WriteOptions{Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}})
	noErr(t, "NewObjectWriterWith", err)
	_, _ = io.WriteString(w, strings.Repeat("compressible ", 1000))
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	noErr(t, "Commit", err)
	if info.Compression != "" {
		t.Fatalf("native object stored with compression %q", info.Compression)
	}
	_, err = b.NewObjectWriterWith(ctx, "asis", "k", storage.WriteOptions{Encryption: storage.EncryptionAES256})
	wantErr(t, "encrypted write", err, errors.ErrUnsupported)
	_, err = b.NewVersionWriter(ctx, "asis", "k")
	wantErr(t, "NewVersionWriter", err, errors.ErrUnsupported)
	wantErr(t, "NewVersionWriter", err, storage.ErrNotSupported)
	_, err = b.NewVersionWriterWith(ctx, "asis", "k", storage.WriteOptions{})
	wantErr(t, "NewVersionWriterWith", err, errors.ErrUnsupported)
	wantErr(t, "PrepareEncryption", b.PrepareEncryption(ctx, "asis", storage.EncryptionAES256), errors.ErrUnsupported)
	wantErr(t, "ValidateEncryptionSettings", b.ValidateEncryptionSettings(ctx, "asis", storage.EncryptionSettings{Provider: storage.EncryptionAES256}),
		errors.ErrUnsupported)
	if p, err := b.EncryptionProviders(ctx, "asis"); err != nil || p == nil || len(p) != 0 {
		t.Fatalf("EncryptionProviders = %v, %v", p, err)
	}
}

// TestPrepareConnection checks connection changes with native buckets and
// containers present: wrong credentials and a different KEK are refused,
// the right ones switch every store.
func TestPrepareConnection(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	for _, withHome := range []bool{true, false} {
		name := "local"
		if withHome {
			name = "home"
		}
		t.Run(name, func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			ctx := context.Background()
			var o Options
			if withHome {
				o = homeOptions(t, r, NewClaims(), "home", "")
			} else {
				o = localOptions(t, r, NewClaims(), t.TempDir())
			}
			o.Conn.KEK = kek
			b := openReady(t, o)
			ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "contained", CreateBucket: true})
			noErr(t, "AddContainer", err)
			noErr(t, "create wrapped", b.CreateBucket(ctx, "wb", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
			noErr(t, "create native", b.CreateBucket(ctx, "nb-conn", storage.CreateBucketOptions{Target: nativeTarget("", false)}))

			bad := r.Config()
			bad.SecretKey = "wrong-secret-key-000000000000000000000000"
			badClient, err := s3client.New(bad)
			noErr(t, "s3client.New", err)
			conn := testConn(t, r)
			conn.KEK = kek
			conn.Client, conn.SecretKey = badClient, bad.SecretKey
			if _, err := b.PrepareConnection(ctx, conn); err == nil {
				t.Fatal("PrepareConnection with wrong credentials succeeded")
			}
			conn = testConn(t, r)
			conn.KEK = bytes.Repeat([]byte{8}, 32)
			if _, err := b.PrepareConnection(ctx, conn); err == nil {
				t.Fatal("PrepareConnection with another KEK succeeded")
			}
			conn = testConn(t, r)
			conn.KEK = kek
			conn.PartSize = 6 << 20
			commit, err := b.PrepareConnection(ctx, conn)
			noErr(t, "PrepareConnection", err)
			commit()
			if b.conn.Load().Client != conn.Client {
				t.Fatal("the connection was not switched")
			}
			put(t, b, "wb", "k", "wrapped")
			put(t, b, "nb-conn", "k", "native")
			if got := read(t, b, "nb-conn", "k"); got != "native" {
				t.Fatalf("nb-conn/k = %q", got)
			}
			noErr(t, "create after the switch", b.CreateBucket(ctx, "nb-two", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
		})
	}
}

// TestProbeHealthWithoutHome: a backing without a home is healthy while
// the service answers, even with access denied to listing buckets.
func TestProbeHealthWithoutHome(t *testing.T) {
	var mode atomic.Int32 // 0 pass, 1 deny listing, 2 fail
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet && req.URL.Path == "/" {
				switch mode.Load() {
				case 1:
					w.WriteHeader(http.StatusForbidden)
					return
				case 2:
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			h.ServeHTTP(w, req)
		})
	}})
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	noErr(t, "ProbeHealth", b.ProbeHealth(ctx))
	mode.Store(1)
	noErr(t, "ProbeHealth (denied)", b.ProbeHealth(ctx))
	mode.Store(2)
	wantErr(t, "ProbeHealth (down)", b.ProbeHealth(ctx), storage.ErrBackingUnavailable)
}

// TestNativeACLSurvivesReopen: a public-read ACL set through gos3 on a
// native object is honoured after the backing is reopened, and a copy of
// the object's headers made on the remote by another client grants
// nothing.
func TestNativeACLSurvivesReopen(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	o := localOptions(t, r, NewClaims(), t.TempDir())
	b := openReady(t, o)
	noErr(t, "create", b.CreateBucket(ctx, "acls", storage.CreateBucketOptions{}))
	w, err := b.NewObjectWriter(ctx, "acls", "public")
	noErr(t, "NewObjectWriter", err)
	_, _ = io.WriteString(w, "hello")
	_, err = w.Commit(ctx, storage.ObjectMeta{ETag: "5d41402abc4b2a76b9719d911017c592", ACL: "public-read"}, storage.CommitOptions{})
	noErr(t, "Commit", err)
	noErr(t, "Close", b.Close())

	b = openReady(t, o)
	info, err := b.GetObjectInfo(ctx, "acls", "public", "")
	noErr(t, "GetObjectInfo", err)
	if info.ACL != "public-read" {
		t.Fatalf("ACL after reopen %q, want public-read", info.ACL)
	}
	// A foreign tool copies the object, header included, to another key.
	_, err = r.Client(t).CopyObject(ctx, "acls", "copied", "public", s3client.CopyOptions{})
	noErr(t, "remote CopyObject", err)
	info, err = b.GetObjectInfo(ctx, "acls", "copied", "")
	noErr(t, "GetObjectInfo(copied)", err)
	if info.ACL != "" {
		t.Fatalf("a copied header granted ACL %q", info.ACL)
	}
}

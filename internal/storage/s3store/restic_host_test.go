package s3store

import (
	"bytes"
	"encoding/json"
	"errors"
	iofs "io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// wantNoRepository fails unless err says that the repository does not
// exist yet (errors.ErrUnsupported and fs.ErrNotExist).
func wantNoRepository(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("%s = %v, want no repository yet", what, err)
	}
}

func TestResticHostBucket(t *testing.T) {
	log := restictest.Install(t, "s3store-host")
	v := newBenv(t)
	b := v.ready(v.kekOptions("p/"))
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	id := bucketID(t, b, "bk")
	if _, _, err := b.ResticRepository(ctx, "nope", true); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("unknown bucket = %v", err)
	}
	// Without create nothing is created.
	_, _, err := b.ResticRepository(ctx, "bk", false)
	wantNoRepository(t, "lookup before create", err)
	if keys := v.list("p/keys/"); len(keys) != 0 {
		t.Fatalf("a lookup created %v", keys)
	}

	info, loc, err := b.ResticRepository(ctx, "bk", true)
	if err != nil {
		t.Fatal(err)
	}
	bk, _ := b.lookup(ctx, "bk")
	master, err := b.bucketKey(ctx, bk, false)
	if err != nil {
		t.Fatal(err)
	}
	wantRepo := "s3:" + v.remote.URL + "/store/p/restic/" + id
	if info.BucketID != id || info.Repository != wantRepo || loc.Repository != wantRepo || !info.Initialized ||
		info.RecordLocation != "s3://store/p/keys/"+id+".restic.json" ||
		!strings.Contains(info.PasswordSource, "s3://store/p/keys/"+id+".key") ||
		!strings.Contains(info.PasswordSource, "encryption_key") {
		t.Fatalf("info = %+v", info)
	}
	if loc.Password != restic.PasswordFromKey(master) {
		t.Fatal("the location's password is not the bucket's")
	}
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !slices.Contains(info.EnvNames, name) {
			t.Fatalf("env names %v lack %s", info.EnvNames, name)
		}
	}
	if !slices.Contains(info.Options, "s3.bucket-lookup=path") {
		t.Fatalf("options = %v", info.Options)
	}
	raw, _ := json.Marshal(info)
	if strings.Contains(string(raw), loc.Password) || strings.Contains(string(raw), v.remote.Config().SecretKey) {
		t.Fatalf("the info discloses a secret: %s", raw)
	}
	if objs := v.list("p/restic/" + id + "/config"); len(objs) != 1 {
		t.Fatalf("repository not initialized: %v", objs)
	}

	// A lookup finds the same repository, initialized.
	again, loc2, err := b.ResticRepository(ctx, "bk", false)
	if err != nil || !again.Initialized || again.Repository != info.Repository || loc2.Password != loc.Password {
		t.Fatalf("lookup = %+v, %v", again, err)
	}
	if n := restictest.Calls(t, log, "init"); n != 1 {
		t.Fatalf("init ran %d times", n)
	}

	// The CLI unwraps the key object with the KEK alone.
	data := v.get("p/keys/" + id + ".key")
	got, err := UnwrapBucketKey(data, testKEK, b.Marker().BackingID, id)
	if err != nil || !bytes.Equal(got, master) {
		t.Fatalf("UnwrapBucketKey = %x, %v", got, err)
	}
	if _, err := UnwrapBucketKey(data, bytes.Repeat([]byte{1}, 32), b.Marker().BackingID, id); !errors.Is(err, ErrWrongKEK) {
		t.Fatalf("another KEK = %v", err)
	}
	if _, err := UnwrapBucketKey(data, testKEK, b.Marker().BackingID, "other"); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("another bucket ID = %v", err)
	}
	if _, err := UnwrapAuxKey(data, testKEK, b.Marker().BackingID, id); err == nil {
		t.Fatal("a bucket key unwrapped as an aux key")
	}
}

func TestResticHostUnsupported(t *testing.T) {
	restictest.Install(t, "s3store-host")
	v := newBenv(t)
	ctx := t.Context()
	for i, mod := range []func(*Options){
		func(o *Options) { o.KEK = nil },
		func(o *Options) { o.AccessKey, o.SecretKey = "", "" },
	} {
		opts := v.kekOptions(string(rune('a'+i)) + "/")
		mod(&opts)
		b := v.ready(opts)
		if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, create := range []bool{false, true} {
			if _, _, err := b.ResticRepository(ctx, "bk", create); !errors.Is(err, errors.ErrUnsupported) || errors.Is(err, iofs.ErrNotExist) {
				t.Fatalf("%d: ResticRepository(create=%v) = %v", i, create, err)
			}
			if _, _, err := b.ResticAuxRepository(ctx, "snap", create); !errors.Is(err, errors.ErrUnsupported) || errors.Is(err, iofs.ErrNotExist) {
				t.Fatalf("%d: ResticAuxRepository(create=%v) = %v", i, create, err)
			}
		}
		if _, _, err := b.ResticRepository(ctx, "nope", false); !errors.Is(err, storage.ErrBucketNotFound) {
			t.Fatalf("%d: unknown bucket = %v", i, err)
		}
		if keys := v.list(string(rune('a'+i)) + "/keys/"); len(keys) != 0 {
			t.Fatalf("%d: keys created: %v", i, keys)
		}
	}
}

func TestResticHostAux(t *testing.T) {
	log := restictest.Install(t, "s3store-host")
	v := newBenv(t)
	opts := v.kekOptions("p/")
	b := v.ready(opts)
	ctx := t.Context()
	for _, name := range []string{"", "Snap", "a/b", "..", "a_b", strings.Repeat("a", 65)} {
		if _, _, err := b.ResticAuxRepository(ctx, name, true); !errors.Is(err, storage.ErrInvalidName) {
			t.Fatalf("name %q = %v", name, err)
		}
	}
	_, _, err := b.ResticAuxRepository(ctx, "snap-1", false)
	wantNoRepository(t, "lookup before create", err)
	if keys := v.list("p/keys/"); len(keys) != 0 {
		t.Fatalf("a lookup created %v", keys)
	}

	info, loc, err := b.ResticAuxRepository(ctx, "snap-1", true)
	if err != nil {
		t.Fatal(err)
	}
	wantRepo := "s3:" + v.remote.URL + "/store/p/restic-aux/snap-1"
	if info.BucketID != "" || info.Repository != wantRepo || loc.Repository != wantRepo || !info.Initialized ||
		info.RecordLocation != "s3://store/p/keys/aux/snap-1.restic.json" ||
		!strings.Contains(info.PasswordSource, "s3://store/p/keys/aux/snap-1.key") ||
		!slices.Contains(info.EnvNames, "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("info = %+v", info)
	}
	if objs := v.list("p/restic-aux/snap-1/config"); len(objs) != 1 {
		t.Fatalf("repository not initialized: %v", objs)
	}
	keyRaw := v.get("p/keys/aux/snap-1.key")
	recRaw := string(v.get("p/keys/aux/snap-1.restic.json"))
	if strings.Contains(recRaw, loc.Password) || strings.Contains(recRaw, "password") {
		t.Fatalf("the record holds the password: %s", recRaw)
	}
	backingID := b.Marker().BackingID
	key, err := UnwrapAuxKey(keyRaw, testKEK, backingID, "snap-1")
	if err != nil || restic.PasswordFromKey(key) != loc.Password {
		t.Fatalf("UnwrapAuxKey = %v", err)
	}
	if _, err := UnwrapBucketKey(keyRaw, testKEK, backingID, "aux/snap-1"); err == nil {
		t.Fatal("an aux key unwrapped as a bucket key")
	}

	// Created once: again, and after a restart, the same key.
	if _, loc2, err := b.ResticAuxRepository(ctx, "snap-1", true); err != nil || loc2.Password != loc.Password {
		t.Fatalf("again = %v", err)
	}
	_ = b.Close()
	b = v.ready(opts)
	if got, loc2, err := b.ResticAuxRepository(ctx, "snap-1", false); err != nil || !got.Initialized || loc2.Password != loc.Password {
		t.Fatalf("lookup after a restart = %+v, %v", got, err)
	}
	if _, loc2, err := b.ResticAuxRepository(ctx, "snap-1", true); err != nil || loc2.Password != loc.Password {
		t.Fatalf("create after a restart = %v", err)
	}
	if !bytes.Equal(v.get("p/keys/aux/snap-1.key"), keyRaw) {
		t.Fatal("the key object was rewritten")
	}
	if n := restictest.Calls(t, log, "init"); n != 1 {
		t.Fatalf("init ran %d times", n)
	}

	// Offline: the records, and the password from the KEK alone.
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	binfo, bloc, err := b.ResticRepository(ctx, "bk", true)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ListResticRecords(ctx, v.client, storeBucket, "p/")
	if err != nil || len(recs) != 2 {
		t.Fatalf("records = %+v, %v", recs, err)
	}
	for _, rec := range recs {
		want := bloc.Password
		switch {
		case rec.Aux == "snap-1" && rec.BucketID == "":
			want = loc.Password
			if rec.Repository != info.Repository || rec.KeyObject != "p/keys/aux/snap-1.key" || rec.Bucket != "" {
				t.Fatalf("aux record = %+v", rec)
			}
		case rec.BucketID == binfo.BucketID && rec.Aux == "":
			if rec.Repository != binfo.Repository || rec.KeyObject != "p/keys/"+binfo.BucketID+".key" || rec.Bucket != "bk" {
				t.Fatalf("bucket record = %+v", rec)
			}
		default:
			t.Fatalf("unexpected record %+v", rec)
		}
		if !rec.Initialized {
			t.Fatalf("record %+v not initialized", rec)
		}
		k, err := ReadResticKey(ctx, v.client, storeBucket, testKEK, backingID, rec)
		if err != nil || restic.PasswordFromKey(k) != want {
			t.Fatalf("ReadResticKey(%+v) = %v", rec, err)
		}
	}

	// Never regenerated: with the key object lost, the repository's record
	// or data refuse a new key.
	_ = b.Close()
	if err := v.client.DeleteObject(ctx, storeBucket, "p/keys/aux/snap-1.key"); err != nil {
		t.Fatal(err)
	}
	b = v.ready(opts)
	for _, create := range []bool{false, true} {
		if _, _, err := b.ResticAuxRepository(ctx, "snap-1", create); !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "restore it") {
			t.Fatalf("lost key (create=%v) = %v", create, err)
		}
	}
	if err := v.client.DeleteObject(ctx, storeBucket, "p/keys/aux/snap-1.restic.json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.ResticAuxRepository(ctx, "snap-1", true); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("lost key and record = %v", err)
	}
	if objs := v.list("p/keys/aux/"); len(objs) != 0 {
		t.Fatalf("a key was regenerated: %v", objs)
	}
}

// The KEK check at connection changes reads whichever key object comes
// first, an aux repository's included.
func TestVerifyKEKWithAuxKey(t *testing.T) {
	restictest.Install(t, "s3store-host")
	v := newBenv(t)
	b := v.ready(v.kekOptions("q/"))
	ctx := t.Context()
	if _, _, err := b.ResticAuxRepository(ctx, "only", true); err != nil {
		t.Fatal(err)
	}
	if keys := v.list("q/keys/"); !slices.Contains(keys, "q/keys/aux/only.key") || slices.ContainsFunc(keys, func(k string) bool {
		return strings.HasSuffix(k, ".key") && k != "q/keys/aux/only.key"
	}) {
		t.Fatalf("keys = %v", keys)
	}
	id := b.Marker().BackingID
	if err := b.verifyKEK(ctx, v.client, id, testKEK); err != nil {
		t.Fatalf("verifyKEK = %v", err)
	}
	if err := b.verifyKEK(ctx, v.client, id, bytes.Repeat([]byte{3}, 32)); !errors.Is(err, ErrWrongKEK) {
		t.Fatalf("verifyKEK(other) = %v", err)
	}
}

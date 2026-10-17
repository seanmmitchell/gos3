package s3store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

func TestMain(m *testing.M) { restictest.Main(m) }

var testKEK = bytes.Repeat([]byte{0x42}, 32)

// kekOptions are options with a KEK and the credentials restic needs.
func (v *benv) kekOptions(prefix string) Options {
	o := v.options(prefix)
	o.KEK = testKEK
	cfg := v.remote.Config()
	o.Endpoint, o.Region, o.AccessKey, o.SecretKey, o.PathStyle = cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, true
	return o
}

func (v *benv) get(key string) []byte {
	v.t.Helper()
	data, _, err := remote{c: v.client, bucket: storeBucket}.getBytes(context.Background(), key, 1<<30)
	if err != nil {
		v.t.Fatal(err)
	}
	return data
}

func (v *benv) put(key string, data []byte) {
	v.t.Helper()
	if _, err := v.client.PutObject(context.Background(), storeBucket, key, bytes.NewReader(data), int64(len(data)), s3client.PutOptions{}); err != nil {
		v.t.Fatal(err)
	}
}

func bucketID(t *testing.T, b *Backend, name string) string {
	t.Helper()
	bk, err := b.lookup(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return bk.id
}

func writeWith(t *testing.T, b *Backend, bucket, key string, data []byte, opts storage.WriteOptions) storage.ObjectInfo {
	t.Helper()
	w, err := b.NewObjectWriterWith(t.Context(), bucket, key, opts)
	if err != nil {
		t.Fatalf("NewObjectWriterWith(%+v): %v", opts, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(t.Context(), storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func dataBlob(t *testing.T, v *benv, b *Backend, bucket string) []byte {
	t.Helper()
	keys := v.list(b.keys.prefix + "data/" + bucketID(t, b, bucket) + "/")
	if len(keys) != 1 {
		t.Fatalf("data blobs = %v", keys)
	}
	return v.get(keys[0])
}

var aes = storage.WriteOptions{Encryption: storage.EncryptionAES256}

func TestAESRoundTrip(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "plain"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			v := newBenv(t)
			opts := v.kekOptions("p/")
			b := v.ready(opts)
			ctx := t.Context()
			if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionAES256); err != nil {
				t.Fatal(err)
			}
			content := bytes.Repeat([]byte("secret content 0123456789 "), 8000)
			wopts := aes
			if compressed {
				wopts.Compression = storage.Compression{Algorithm: storage.CompressionDeflate, Level: 6}
			}
			info := writeWith(t, b, "bk", "k", content, wopts)
			if info.Encryption != storage.EncryptionAES256 {
				t.Fatalf("Encryption = %q", info.Encryption)
			}
			blob := dataBlob(t, v, b, "bk")
			if int64(len(blob)) != info.PhysicalSize() {
				t.Fatalf("blob has %d bytes, PhysicalSize %d", len(blob), info.PhysicalSize())
			}
			encoded := info.Size
			if compressed {
				if info.Compression != storage.CompressionDeflate || info.StoredSize >= info.Size {
					t.Fatalf("not compressed: %+v", info)
				}
				encoded = info.StoredSize
			}
			if int64(len(blob)) != storage.EncryptedSize(encoded) {
				t.Fatalf("blob has %d bytes, want EncryptedSize(%d) = %d", len(blob), encoded, storage.EncryptedSize(encoded))
			}
			if bytes.Contains(blob, []byte("secret content")) {
				t.Fatal("blob holds plaintext")
			}
			if got := readObject(t, b, "bk", "k", ""); !bytes.Equal(got, content) {
				t.Fatal("content differs")
			}
			// Random access through decryption (and decompression).
			_, r, _ := b.OpenObject(ctx, "bk", "k", "")
			if _, err := r.Seek(100000, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 50)
			if _, err := io.ReadFull(r, buf); err != nil || !bytes.Equal(buf, content[100000:100050]) {
				t.Fatalf("ranged read: %v", err)
			}
			_ = r.Close()

			// The key object, and the keymark across a reopen.
			var kf keyFile
			if err := json.Unmarshal(v.get(b.keyObject(bucketID(t, b, "bk"))), &kf); err != nil {
				t.Fatal(err)
			}
			if kf.V != 1 || kf.Alg != "A256GCM" || kf.KEK != crypt.KeyCheck(testKEK) || len(kf.Wrapped) != crypt.KeySize+crypt.WrapOverhead {
				t.Fatalf("key object = %+v", kf)
			}
			_ = b.Close()
			b = v.ready(opts)
			bk, _ := b.lookup(ctx, "bk")
			if !bk.keyed {
				t.Fatal("keymark lost across reopen")
			}
			if got := readObject(t, b, "bk", "k", ""); !bytes.Equal(got, content) {
				t.Fatal("content differs after reopen")
			}
			// An empty encrypted object stores nothing.
			info = writeWith(t, b, "bk", "empty", nil, wopts)
			if info.Encryption != storage.EncryptionAES256 || info.PhysicalSize() != 0 || len(readObject(t, b, "bk", "empty", "")) != 0 {
				t.Fatalf("empty object: %+v", info)
			}
		})
	}
}

func TestEncryptedMultipartAndImport(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.kekOptions("p/"))
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	for _, c := range []storage.Compression{{}, {Algorithm: storage.CompressionDeflate, Level: 1}} {
		key := "mp-" + c.Algorithm
		u, err := b.CreateMultipartUpload(ctx, "bk", key, storage.ObjectMeta{Encryption: storage.EncryptionAES256}, "")
		if err != nil {
			t.Fatal(err)
		}
		var want []byte
		for n, size := range []int{6 << 20, 1000} {
			part := bytes.Repeat([]byte{byte('a' + n)}, size)
			w, err := b.NewPartWriter(ctx, "bk", key, u.UploadID, n+1)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(part)
			if _, err := w.Commit(ctx, storage.PartMeta{ETag: "p"}); err != nil {
				t.Fatal(err)
			}
			want = append(want, part...)
		}
		// Parts are stored encrypted.
		var stored []int64
		_ = v.client.Walk(ctx, storeBucket, b.partPrefix(u.UploadID), "", func(o s3client.ObjectAttrs) error {
			stored = append(stored, o.Size)
			return nil
		})
		if !slices.Equal(stored, []int64{storage.EncryptedSize(6 << 20), storage.EncryptedSize(1000)}) {
			t.Fatalf("part blob sizes %v", stored)
		}
		info, err := b.CompleteMultipartUploadWith(ctx, "bk", key, u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"},
			storage.CommitOptions{}, storage.WriteOptions{Encryption: storage.EncryptionAES256, Compression: c})
		if err != nil {
			t.Fatal(err)
		}
		if info.Encryption != storage.EncryptionAES256 || info.Size != int64(len(want)) {
			t.Fatalf("info = %+v", info)
		}
		if got := readObject(t, b, "bk", key, ""); !bytes.Equal(got, want) {
			t.Fatal("completed content differs")
		}
	}
	// A plain completion of encrypted parts decrypts them (no compose).
	u, _ := b.CreateMultipartUpload(ctx, "bk", "plain", storage.ObjectMeta{Encryption: storage.EncryptionAES256}, "")
	w, _ := b.NewPartWriter(ctx, "bk", "plain", u.UploadID, 1)
	_, _ = w.Write([]byte("hello"))
	if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bk", "plain", u.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readObject(t, b, "bk", "plain", ""); string(got) != "hello" {
		t.Fatalf("plain completion = %q", got)
	}

	// Imports store encrypted.
	vw, err := b.NewVersionWriterWith(ctx, "bk", "imp", aes)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = vw.Write([]byte("imported secret"))
	info, err := vw.Commit(ctx, storage.ObjectInfo{VersionID: "imp-1", LastModified: time.Unix(1e9, 0), ObjectMeta: storage.ObjectMeta{ETag: "i"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Encryption != storage.EncryptionAES256 || info.VersionID != "imp-1" {
		t.Fatalf("import info = %+v", info)
	}
	if got := readObject(t, b, "bk", "imp", "imp-1"); string(got) != "imported secret" {
		t.Fatalf("import = %q", got)
	}
}

func TestKEKRemovedOrKeyDeleted(t *testing.T) {
	v := newBenv(t)
	opts := v.kekOptions("p/")
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	writeWith(t, b, "bk", "enc", []byte("secret"), aes)
	writeWith(t, b, "bk", "plain", []byte("open"), storage.WriteOptions{})
	id := bucketID(t, b, "bk")
	_ = b.Close()

	// Without the KEK: plain versions read, encrypted ones do not.
	noKEK := opts
	noKEK.KEK = nil
	b = v.ready(noKEK)
	if b.Engine().KEKState() != KEKMissing {
		t.Fatalf("KEKState = %v", b.Engine().KEKState())
	}
	if got := readObject(t, b, "bk", "plain", ""); string(got) != "open" {
		t.Fatalf("plain = %q", got)
	}
	if _, _, err := b.OpenObject(ctx, "bk", "enc", ""); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("encrypted read without KEK = %v", err)
	}
	if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionAES256); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("PrepareEncryption without KEK = %v", err)
	}
	if _, err := b.NewObjectWriterWith(ctx, "bk", "x", aes); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("encrypted write without KEK = %v", err)
	}
	if p, err := b.EncryptionProviders(ctx, "bk"); err != nil || len(p) != 0 {
		t.Fatalf("providers without KEK = %v, %v", p, err)
	}
	_ = b.Close()

	// The key object deleted while the bucket is marked: a hard error, and
	// no new key.
	if err := v.client.DeleteObject(ctx, storeBucket, "p/keys/"+id+".key"); err != nil {
		t.Fatal(err)
	}
	b = v.ready(opts)
	_, _, err := b.OpenObject(ctx, "bk", "enc", "")
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "restore it") {
		t.Fatalf("read with the key deleted = %v", err)
	}
	if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionAES256); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("PrepareEncryption with the key deleted = %v", err)
	}
	if got := v.list("p/keys/"); len(got) != 0 {
		t.Fatalf("a key was regenerated: %v", got)
	}
	if got := readObject(t, b, "bk", "plain", ""); string(got) != "open" {
		t.Fatalf("plain = %q", got)
	}
}

func TestKeyCreationIdempotent(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.kekOptions("p/"))
	ctx := t.Context()
	for _, name := range []string{"pre", "ambiguous", "other"} {
		if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A key object already there (a crash after creating it): the stored
	// key is used (the PUT gets 412).
	id := bucketID(t, b, "pre")
	master := bytes.Repeat([]byte{9}, 32)
	wrapped, _ := crypt.Wrap(testKEK, master, keyAAD(b.Engine().Marker().BackingID, id))
	raw, _ := json.Marshal(keyFile{V: 1, Alg: keyAlgorithm, Wrapped: wrapped, KEK: crypt.KeyCheck(testKEK)})
	v.put(b.keyObject(id), raw)
	bk, _ := b.lookup(ctx, "pre")
	got, err := b.bucketKey(ctx, bk, true)
	if err != nil || !bytes.Equal(got, master) {
		t.Fatalf("bucketKey = %x, %v", got, err)
	}
	if !bk.keyed {
		t.Fatal("bucket not marked")
	}

	// The key PUT lands but answers 500: the stored key is read back.
	f := func(r *http.Request) s3clienttest.Fault {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/keys/") {
			return s3clienttest.Status500
		}
		return s3clienttest.Pass
	}
	v.remote.After.Store(&f)
	if err := b.PrepareEncryption(ctx, "ambiguous", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	v.remote.After.Store(nil)
	abk, _ := b.lookup(ctx, "ambiguous")
	key, _ := b.bucketKey(ctx, abk, false)
	var kf keyFile
	_ = json.Unmarshal(v.get(b.keyObject(abk.id)), &kf)
	stored, err := crypt.Unwrap(testKEK, kf.Wrapped, keyAAD(b.Engine().Marker().BackingID, abk.id))
	if err != nil || !bytes.Equal(stored, key) {
		t.Fatalf("cached key differs from the stored one (%v)", err)
	}

	// AAD binding: a key object copied to another bucket does not unwrap.
	obk, _ := b.lookup(ctx, "other")
	v.put(b.keyObject(obk.id), v.get(b.keyObject(id)))
	if err := b.PrepareEncryption(ctx, "other", storage.EncryptionAES256); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("copied key object = %v", err)
	}
}

func TestEncryptionProvidersMatrix(t *testing.T) {
	restictest.Install(t, "s3store-providers")
	v := newBenv(t)
	ctx := t.Context()
	cases := []struct {
		name string
		mod  func(*Options)
		rt   func(*restic.Runtime)
		want []string
	}{
		{"no KEK", func(o *Options) { o.KEK = nil }, nil, []string{}},
		{"KEK and restic", nil, nil, []string{storage.EncryptionAES256, storage.EncryptionRestic}},
		{"KEK without restic", nil, func(rt *restic.Runtime) {
			rt.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
		}, []string{storage.EncryptionAES256}},
		{"KEK without credentials", func(o *Options) { o.AccessKey, o.SecretKey = "", "" }, nil, []string{storage.EncryptionAES256}},
	}
	for i, c := range cases {
		opts := v.kekOptions(string(rune('a'+i)) + "/")
		if c.mod != nil {
			c.mod(&opts)
		}
		b := v.ready(opts)
		if c.rt != nil {
			c.rt(b.restic)
		}
		got, err := b.EncryptionProviders(ctx, "any")
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: providers = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestResticArchive(t *testing.T) {
	log := restictest.Install(t, "s3store")
	v := newBenv(t)
	opts := v.kekOptions("p/")
	opts.OrphanRetention = time.Minute
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateEncryptionSettings(ctx, "bk", storage.EncryptionSettings{Provider: storage.EncryptionRestic}); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareEncryption(ctx, "bk", storage.EncryptionRestic); err != nil {
		t.Fatal(err)
	}
	ropts := storage.WriteOptions{Encryption: storage.EncryptionRestic, Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 5}}
	content := bytes.Repeat([]byte("archived "), 5000)
	info := writeWith(t, b, "bk", "k", content, ropts)
	if info.Encryption != storage.EncryptionRestic || info.Compression != "" || info.Size != int64(len(content)) {
		t.Fatalf("info = %+v", info)
	}
	_, r, err := b.OpenObject(ctx, "bk", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.(storage.PreparingReader); !ok || p.Prepare() != nil {
		t.Fatal("restic reader does not prepare")
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("restic content differs")
	}
	if restictest.Calls(t, log, "backup") != 1 {
		t.Fatalf("backups = %d", restictest.Calls(t, log, "backup"))
	}
	// An empty object never reaches restic.
	writeWith(t, b, "bk", "empty", nil, ropts)
	if len(readObject(t, b, "bk", "empty", "")) != 0 || restictest.Calls(t, log, "backup") != 1 {
		t.Fatal("empty restic object")
	}
	// A multipart upload archived at completion (its parts staged AES256).
	u, _ := b.CreateMultipartUpload(ctx, "bk", "mp", storage.ObjectMeta{Encryption: storage.EncryptionRestic}, "")
	for n := 1; n <= 2; n++ {
		w, _ := b.NewPartWriter(ctx, "bk", "mp", u.UploadID, n)
		_, _ = w.Write(bytes.Repeat([]byte{byte('0' + n)}, 3000))
		if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.CompleteMultipartUploadWith(ctx, "bk", "mp", u.UploadID, []int{1, 2}, storage.ObjectMeta{}, storage.CommitOptions{},
		storage.WriteOptions{Encryption: storage.EncryptionRestic}); err != nil {
		t.Fatal(err)
	}
	if got := readObject(t, b, "bk", "mp", ""); len(got) != 6000 || got[0] != '1' || got[5999] != '2' {
		t.Fatal("archived multipart content differs")
	}
	// A refused write archives nothing.
	w, _ := b.NewObjectWriterWith(ctx, "bk", "k", ropts)
	_, _ = w.Write([]byte("refused"))
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("refused write = %v", err)
	}
	if snaps := v.list("p/restic/" + bucketID(t, b, "bk") + "/snapshots/"); len(snaps) != 2 {
		t.Fatalf("snapshots = %v, want those of k and mp only", snaps)
	}

	// Credential changes apply to the next restic command.
	cfg := v.remote.Config()
	commit, err := b.PrepareConnection(ctx, Connection{Client: v.client, Endpoint: cfg.Endpoint, Region: cfg.Region,
		AccessKey: "WRONGACCESSKEY", SecretKey: "wrong", PathStyle: true, KEK: testKEK})
	if err != nil {
		t.Fatal(err)
	}
	commit()
	w, _ = b.NewObjectWriterWith(ctx, "bk", "bad", ropts)
	_, _ = w.Write([]byte("x"))
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("restic write with wrong credentials = %v", err)
	}
	commit, err = b.PrepareConnection(ctx, Connection{Client: v.client, Endpoint: cfg.Endpoint, Region: cfg.Region,
		AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, PathStyle: true, KEK: testKEK})
	if err != nil {
		t.Fatal(err)
	}
	commit()
	writeWith(t, b, "bk", "good", []byte("y"), ropts)

	id := bucketID(t, b, "bk")
	if repo := v.list("p/restic/" + id + "/"); len(repo) < 3 {
		t.Fatalf("repository objects = %v", repo)
	}
	recRaw := string(v.get("p/keys/" + id + ".restic.json"))
	bk, _ := b.lookup(ctx, "bk")
	master, _ := b.bucketKey(ctx, bk, false)
	if strings.Contains(recRaw, "password") || strings.Contains(recRaw, restic.PasswordFromKey(master)) {
		t.Fatalf("recovery record holds the password: %s", recRaw)
	}
	var rec restic.Record
	if err := json.Unmarshal([]byte(recRaw), &rec); err != nil || !rec.Initialized || rec.Repository != "s3:"+v.remote.URL+"/store/p/restic/"+id {
		t.Fatalf("recovery record = %+v (%v)", rec, err)
	}

	// The recovery pass never touches restic/ or keys/.
	before := append(v.list("p/restic/"), v.list("p/keys/")...)
	b.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if _, err := b.recoverOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if after := append(v.list("p/restic/"), v.list("p/keys/")...); !slices.Equal(before, after) {
		t.Fatalf("recovery touched restic/ or keys/: %v -> %v", before, after)
	}
	b.now = time.Now

	// Readable after a reopen.
	_ = b.Close()
	b = v.ready(opts)
	if got := readObject(t, b, "bk", "k", ""); !bytes.Equal(got, content) {
		t.Fatal("restic content differs after reopen")
	}
}

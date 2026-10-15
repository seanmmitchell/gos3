package gateway_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/gateway"
)

// fixture is a gateway store over a fresh in-process remote.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	remote *s3clienttest.Remote
	c      *s3client.Client // plain client of the remote
	opts   gateway.Options
	st     *gateway.Store
}

// newFixture opens a store; shared selects the shared-bucket mode
// (remote bucket "shared", prefix "pfx/").
func newFixture(t *testing.T, shared bool, mod func(*gateway.Options)) *fixture {
	t.Helper()
	r := s3clienttest.Start(t)
	f := &fixture{t: t, ctx: t.Context(), remote: r, c: r.Client(t)}
	f.opts = gateway.Options{Name: "gw", Client: f.c, State: gateway.MemoryState()}
	if shared {
		r.CreateBucket(t, "shared")
		f.opts.Bucket, f.opts.Prefix = "shared", "pfx/"
	}
	if mod != nil {
		mod(&f.opts)
	}
	f.open()
	return f
}

func (f *fixture) open() {
	f.t.Helper()
	st, err := gateway.Open(f.opts)
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	f.st = st
	f.t.Cleanup(func() { _ = st.Close() })
}

// remoteKey returns the remote location of bucket/key.
func (f *fixture) remoteKey(bucket, key string) (string, string) {
	if f.opts.Bucket == "" {
		return bucket, f.opts.Prefix + key
	}
	return f.opts.Bucket, f.opts.Prefix + bucket + "/" + key
}

func (f *fixture) attach(bucket string) {
	f.t.Helper()
	if err := f.st.AttachBucket(f.ctx, storage.BucketInfo{Name: bucket, Backing: "gw"}); err != nil {
		f.t.Fatalf("AttachBucket(%s): %v", bucket, err)
	}
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func (f *fixture) put(bucket, key string, data []byte, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	f.t.Helper()
	w, err := f.st.NewObjectWriter(f.ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if _, err := w.Write(data); err != nil {
		f.t.Fatalf("Write: %v", err)
	}
	info, err := w.Commit(f.ctx, meta, opts)
	if err != nil {
		_ = w.Abort()
	}
	return info, err
}

func (f *fixture) mustPut(bucket, key string, data []byte, meta storage.ObjectMeta) storage.ObjectInfo {
	f.t.Helper()
	info, err := f.put(bucket, key, data, meta, storage.CommitOptions{})
	if err != nil {
		f.t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
	return info
}

func (f *fixture) read(bucket, key string) (storage.ObjectInfo, []byte) {
	f.t.Helper()
	info, r, err := f.st.OpenObject(f.ctx, bucket, key, "")
	if err != nil {
		f.t.Fatalf("OpenObject(%s/%s): %v", bucket, key, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		f.t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return info, data
}

func (f *fixture) remoteHead(bucket, key string) s3client.ObjectAttrs {
	f.t.Helper()
	rb, rk := f.remoteKey(bucket, key)
	a, err := f.c.HeadObject(f.ctx, rb, rk)
	if err != nil {
		f.t.Fatalf("remote HEAD %s/%s: %v", rb, rk, err)
	}
	return a
}

func (f *fixture) walkKeys(bucket, prefix, after string) []string {
	f.t.Helper()
	var keys []string
	err := f.st.WalkObjects(f.ctx, bucket, prefix, after, func(o storage.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	if err != nil {
		f.t.Fatalf("WalkObjects: %v", err)
	}
	return keys
}

// fullMeta sets every field (like the conformance suite's fullMeta).
func fullMeta() storage.ObjectMeta {
	return storage.ObjectMeta{
		ETag:               "9e107d9d372bb6826bd81d3542a419d6",
		ContentType:        "text/plain; charset=utf-8",
		ContentEncoding:    "gzip",
		ContentDisposition: `attachment; filename="résumé.txt"`,
		ContentLanguage:    "de-CH",
		CacheControl:       "max-age=60, private",
		Expires:            "Wed, 21 Oct 2037 07:28:00 GMT",
		UserMeta:           map[string]string{"color": "blue", "empty": "", "unicode": "日本語", "with-dash": "a=b; c", "gos3-meta": "user value"},
		Tags:               map[string]string{"project": "gos3", "k 2": "v=2&x"},
		StorageClass:       "REDUCED_REDUNDANCY",
		ACL:                "public-read",
		Checksums:          map[string]string{"CRC32": "NhCmhg==", "SHA256": "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="},
		ChecksumType:       "COMPOSITE",
		PartSizes:          []int64{5 << 20, 5 << 20, 1},
		PartChecksums:      []map[string]string{{"CRC32": "AAAAAA=="}, {"CRC32": "BBBBBB=="}, {"CRC32": "CCCCCC=="}},
		SSE:                "aws:kms",
		SSEKMSKeyID:        "arn:aws:kms:us-east-1:123456789012:key/1234abcd",
		RetentionMode:      "COMPLIANCE",
		RetainUntil:        time.Date(2031, 2, 3, 4, 5, 6, 123456789, time.FixedZone("x", 5*3600+1800)),
		LegalHold:          true,
		WebsiteRedirect:    "/redirected/page.html",
	}
}

// metaEqual compares metadata (times with Equal, nil == empty).
func metaEqual(t *testing.T, what string, want, got storage.ObjectMeta) {
	t.Helper()
	norm := func(m storage.ObjectMeta) string {
		ru := m.RetainUntil
		m.RetainUntil = time.Time{}
		return fmt.Sprintf("%v|%d", fmtMeta(m), ru.UnixNano())
	}
	if norm(want) != norm(got) {
		t.Errorf("%s: metadata mismatch:\n got  %s\n want %s", what, norm(got), norm(want))
	}
}

func fmtMeta(m storage.ObjectMeta) string {
	if len(m.UserMeta) == 0 {
		m.UserMeta = nil
	}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	if len(m.Checksums) == 0 {
		m.Checksums = nil
	}
	return fmt.Sprintf("%+v", m)
}

func TestNativeObjects(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			f := newFixture(t, shared, nil)
			f.attach("bkt")
			data := []byte("hello gateway")
			meta := storage.ObjectMeta{
				ETag:        md5hex(data),
				ContentType: "text/plain",
				UserMeta:    map[string]string{"color": "blue"},
			}
			f.mustPut("bkt", "dir/k.txt", data, meta)

			// Plain objects carry no gos3-meta: they look like any client's.
			a := f.remoteHead("bkt", "dir/k.txt")
			if a.ETag != md5hex(data) || a.Size != int64(len(data)) || a.Header.Get("Content-Type") != "text/plain" ||
				a.Header.Get("X-Amz-Meta-Color") != "blue" || a.Header.Get("X-Amz-Meta-Gos3-Meta") != "" {
				t.Errorf("remote object = %+v %v, want native headers without gos3-meta", a, a.Header)
			}
			rb, rk := f.remoteKey("bkt", "dir/k.txt")
			body, _, err := f.c.GetObject(f.ctx, rb, rk, s3client.GetOptions{})
			if err != nil {
				t.Fatalf("remote GET: %v", err)
			}
			got, _ := io.ReadAll(body)
			body.Close()
			if !bytes.Equal(got, data) {
				t.Errorf("remote content %q, want %q", got, data)
			}

			// Tags and ACL travel in gos3-meta and come back.
			meta.Tags = map[string]string{"t": "1"}
			meta.ACL = "public-read"
			f.mustPut("bkt", "tagged", data, meta)
			a = f.remoteHead("bkt", "tagged")
			if a.Header.Get("X-Amz-Meta-Gos3-Meta") == "" || a.Header.Get("X-Amz-Meta-Color") != "blue" {
				t.Errorf("remote headers %v, want gos3-meta and native color", a.Header)
			}
			info, err := f.st.GetObjectInfo(f.ctx, "bkt", "tagged", "")
			if err != nil {
				t.Fatal(err)
			}
			metaEqual(t, "tagged", meta, info.ObjectMeta)
			if _, ok := info.UserMeta["gos3-meta"]; ok {
				t.Errorf("gos3-meta leaked into UserMeta: %v", info.UserMeta)
			}
			if shared {
				// Nothing outside the bucket's prefix.
				out, err := f.c.ListObjectsV2(f.ctx, "shared", s3client.ListInput{})
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range out.Objects {
					if !strings.HasPrefix(o.Key, "pfx/bkt/") {
						t.Errorf("remote key %q outside pfx/bkt/", o.Key)
					}
				}
			}
		})
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	data := []byte("\x1f\x8b not really gzip, but stored as such")
	for i, meta := range []storage.ObjectMeta{fullMeta(), {}, {ETag: "x", UserMeta: map[string]string{}}} {
		key := fmt.Sprintf("k%d", i)
		committed := f.mustPut("bkt", key, data, meta)
		metaEqual(t, key+" commit", meta, committed.ObjectMeta)
		head, err := f.st.GetObjectInfo(f.ctx, "bkt", key, "")
		if err != nil {
			t.Fatal(err)
		}
		metaEqual(t, key+" HEAD", meta, head.ObjectMeta)
		if !head.LastModified.Equal(committed.LastModified) || head.Size != int64(len(data)) {
			t.Errorf("%s: HEAD LastModified/Size %v/%d, commit %v", key, head.LastModified, head.Size, committed.LastModified)
		}
		info, got := f.read("bkt", key)
		metaEqual(t, key+" GET", meta, info.ObjectMeta)
		if !bytes.Equal(got, data) {
			t.Errorf("%s: content %q, want %q (Content-Encoding must not be decoded)", key, got, data)
		}
	}
}

func TestAdoptExistingBucket(t *testing.T) {
	f := newFixture(t, false, nil)
	f.remote.CreateBucket(t, "existing")
	for _, k := range []string{"a.txt", "dir/b.txt", "z"} {
		_, err := f.c.PutObject(f.ctx, "existing", k, strings.NewReader("data of "+k), int64(len("data of "+k)),
			s3client.PutOptions{Header: http.Header{"Content-Type": {"text/x-foreign"}, "X-Amz-Meta-Origin": {"other-tool"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.attach("existing")
	if got := f.walkKeys("existing", "", ""); !slices.Equal(got, []string{"a.txt", "dir/b.txt", "z"}) {
		t.Errorf("adopted keys %q", got)
	}
	info, data := f.read("existing", "dir/b.txt")
	if string(data) != "data of dir/b.txt" || info.ContentType != "text/x-foreign" || info.UserMeta["origin"] != "other-tool" ||
		info.ETag != md5hex(data) || info.VersionID != storage.NullVersionID {
		t.Errorf("adopted object %+v %q", info, data)
	}
	if err := f.st.CheckEmpty(f.ctx, "existing"); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Errorf("CheckEmpty of an adopted bucket: %v", err)
	}
	// A missing remote bucket is created.
	f.attach("fresh-bucket")
	if err := f.c.HeadBucket(f.ctx, "fresh-bucket"); err != nil {
		t.Errorf("remote bucket not created: %v", err)
	}
}

func TestAttachSharedMissing(t *testing.T) {
	f := newFixture(t, false, func(o *gateway.Options) { o.Bucket = "nope" })
	err := f.st.AttachBucket(f.ctx, storage.BucketInfo{Name: "b"})
	if !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("AttachBucket with a missing shared bucket: %v", err)
	}
}

func TestWalkRestartAfterInvalidUTF8(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			f := newFixture(t, shared, nil)
			f.attach("bkt")
			keys := []string{"dir", "dir/a", "dir/b/c", "dir/\U0010FFFF", "dir/\U0010FFFFx", "dir0", "dis", "é", "éa", "z"}
			for _, k := range keys {
				f.mustPut("bkt", k, nil, storage.ObjectMeta{ETag: md5hex(nil)})
			}
			for _, tc := range []struct {
				prefix, after string
				want          []string
			}{
				{"", "dir/\xff", []string{"dir0", "dis", "z", "é", "éa"}},
				{"dir/", "dir/\xff", nil},
				{"", "dir/b/\xff", []string{"dir/\U0010FFFF", "dir/\U0010FFFFx", "dir0", "dis", "z", "é", "éa"}},
				{"", "\xc3", []string{"é", "éa"}}, // truncated rune: é sorts after it
				{"", "é\xff", nil},
				{"", "d\xc3", []string{"z", "é", "éa"}}, // bound "d", rest filtered locally
				{"", "dir/\U0010FFFF", []string{"dir/\U0010FFFFx", "dir0", "dis", "z", "é", "éa"}},
				{"\xc3", "", []string{"é", "éa"}}, // invalid prefix
			} {
				if got := f.walkKeys("bkt", tc.prefix, tc.after); !slices.Equal(got, tc.want) {
					t.Errorf("WalkObjects(prefix %q, after %q) = %q, want %q", tc.prefix, tc.after, got, tc.want)
				}
			}
			// The listing helper skips common prefixes with "\xff" markers.
			res, err := storage.ListObjects(f.ctx, storeBackend{f.st}, "bkt", storage.ListParams{Delimiter: "/", MaxKeys: 2})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(keysOf(res.Objects), []string{"dir"}) || !slices.Equal(res.CommonPrefixes, []string{"dir/"}) || !res.IsTruncated {
				t.Errorf("ListObjects page 1 = %q %q %v", keysOf(res.Objects), res.CommonPrefixes, res.IsTruncated)
			}
			res, err = storage.ListObjects(f.ctx, storeBackend{f.st}, "bkt", storage.ListParams{Delimiter: "/", After: res.NextAfter})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"dir0", "dis", "z", "é", "éa"}; !slices.Equal(keysOf(res.Objects), want) || len(res.CommonPrefixes) != 0 {
				t.Errorf("ListObjects page 2 = %q %q, want %q", keysOf(res.Objects), res.CommonPrefixes, want)
			}
		})
	}
}

func keysOf(objs []storage.ObjectInfo) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

// storeBackend lets the storage listing helpers walk a store directly.
type storeBackend struct{ *gateway.Store }

func (storeBackend) ListBuckets(context.Context) ([]storage.BucketInfo, error) { return nil, nil }
func (storeBackend) CreateBucket(context.Context, string, storage.CreateBucketOptions) error {
	return nil
}
func (storeBackend) GetBucket(context.Context, string) (storage.BucketInfo, error) {
	return storage.BucketInfo{}, nil
}
func (storeBackend) DeleteBucket(context.Context, string) error { return nil }
func (storeBackend) SetBucketVersioning(context.Context, string, storage.VersioningStatus) error {
	return nil
}
func (storeBackend) SetBucketObjectLock(context.Context, string, bool) error { return nil }
func (storeBackend) GetBucketConfig(context.Context, string, string) ([]byte, error) {
	return nil, storage.ErrConfigNotFound
}
func (storeBackend) PutBucketConfig(context.Context, string, string, []byte) error { return nil }
func (storeBackend) DeleteBucketConfig(context.Context, string, string) error      { return nil }

func randData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func uploadPart(t *testing.T, f *fixture, bucket, key, id string, n int, data []byte) storage.PartInfo {
	t.Helper()
	w, err := f.st.NewPartWriter(f.ctx, bucket, key, id, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	p, err := w.Commit(f.ctx, storage.PartMeta{ETag: md5hex(data), Checksums: map[string]string{"CRC32": fmt.Sprint(n)}})
	if err != nil {
		t.Fatalf("part %d: %v", n, err)
	}
	return p
}

// stagedFiles returns the staged parts in the store's StateStore.
func stagedFiles(t *testing.T, f *fixture) []string {
	t.Helper()
	state := f.opts.State
	if fs, ok := state.(*faultyState); ok {
		state = fs.StateStore // bypass injected failures
	}
	names, err := state.ListStaged(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestMultipartRemoteParts(t *testing.T) {
	f := newFixture(t, true, nil)
	f.attach("bkt")
	base := storage.ObjectMeta{ContentType: "application/x-big", Tags: map[string]string{"a": "b"}}
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "big", base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{randData(5<<20, 1), randData(5<<20+3, 2), randData(1000, 3)}
	for i, p := range parts {
		uploadPart(t, f, "bkt", "big", u.UploadID, i+1, p)
	}
	if staged := stagedFiles(t, f); len(staged) != 1 {
		t.Errorf("staged files %q, want only the small last part", staged)
	}
	// The remote has an in-progress multipart upload of the target key.
	rb, rk := f.remoteKey("bkt", "big")
	ups, err := f.remote.Backend.ListMultipartUploads(f.ctx, rb)
	if err != nil || len(ups) != 1 || ups[0].Key != rk {
		t.Fatalf("remote uploads %+v, %v; want one of %s", ups, err, rk)
	}
	meta := base
	meta.ETag = "0123456789abcdef0123456789abcdef-3"
	meta.PartSizes = []int64{int64(len(parts[0])), int64(len(parts[1])), int64(len(parts[2]))}
	meta.Checksums = map[string]string{"CRC32": "AAAAAA=="}
	meta.ChecksumType = "COMPOSITE"
	info, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "big", u.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(parts...)
	if info.Size != int64(len(want)) {
		t.Errorf("Size %d", info.Size)
	}
	got, data := f.read("bkt", "big")
	if !bytes.Equal(data, want) {
		t.Error("content differs")
	}
	metaEqual(t, "completed", meta, got.ObjectMeta)
	if !got.LastModified.Equal(info.LastModified) {
		t.Errorf("LastModified %v, Complete said %v", got.LastModified, info.LastModified)
	}
	// The final metadata (ETag, part sizes, checksums) was written onto
	// the completed remote object with a server-side copy.
	a := f.remoteHead("bkt", "big")
	if a.Size != int64(len(want)) || a.Header.Get("Content-Type") != "application/x-big" || a.Header.Get("X-Amz-Meta-Gos3-Meta") == "" {
		t.Errorf("remote object %+v, want native headers and gos3-meta", a)
	}
	if ups, _ := f.remote.Backend.ListMultipartUploads(f.ctx, rb); len(ups) != 0 {
		t.Errorf("remote uploads left: %+v", ups)
	}
	if staged := stagedFiles(t, f); len(staged) != 0 {
		t.Errorf("staged files left: %q", staged)
	}
	if _, err := f.st.GetMultipartUpload(f.ctx, "bkt", "big", u.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("upload after completion: %v", err)
	}
}

func TestMultipartSmallParts(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "small", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{[]byte("one,"), []byte("two,"), []byte("three")}
	for i, p := range parts {
		uploadPart(t, f, "bkt", "small", u.UploadID, i+1, p)
	}
	if ups, _ := f.remote.Backend.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
		t.Errorf("small parts created a remote upload: %+v", ups)
	}
	meta := storage.ObjectMeta{ETag: "aaaa-3", PartSizes: []int64{4, 4, 5}}
	if _, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "small", u.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	info, data := f.read("bkt", "small")
	if string(data) != "one,two,three" || info.ETag != "aaaa-3" || !slices.Equal(info.PartSizes, meta.PartSizes) {
		t.Errorf("completed %+v %q", info, data)
	}
	if a := f.remoteHead("bkt", "small"); a.ETag != md5hex(data) {
		t.Errorf("remote ETag %q, want a single PUT", a.ETag)
	}

	// A small non-last part next to a remote part cannot be assembled.
	u, err = f.st.CreateMultipartUpload(f.ctx, "bkt", "mixed", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "mixed", u.UploadID, 1, []byte("small"))
	uploadPart(t, f, "bkt", "mixed", u.UploadID, 2, randData(5<<20, 9))
	_, err = f.st.CompleteMultipartUpload(f.ctx, "bkt", "mixed", u.UploadID, []int{1, 2}, storage.ObjectMeta{}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("mixed completion: %v, want ErrNotSupported", err)
	}
	// Completing only the small part works (the remote upload is aborted).
	if _, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "mixed", u.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if ups, _ := f.remote.Backend.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
		t.Errorf("remote uploads left: %+v", ups)
	}
}

func TestLargeSinglePutUsesMultipart(t *testing.T) {
	f := newFixture(t, false, func(o *gateway.Options) { o.PartSize = 5 << 20 })
	f.attach("bkt")
	data := randData(6<<20, 4)
	info := f.mustPut("bkt", "k", data, storage.ObjectMeta{ETag: md5hex(data)})
	if a := f.remoteHead("bkt", "k"); !strings.Contains(a.ETag, "-") {
		t.Errorf("remote ETag %q, want a multipart upload", a.ETag)
	}
	got, content := f.read("bkt", "k")
	if got.ETag != md5hex(data) || !bytes.Equal(content, data) || !got.LastModified.Equal(info.LastModified) {
		t.Errorf("GET ETag %q (want the single-PUT ETag %q), content equal %v", got.ETag, md5hex(data), bytes.Equal(content, data))
	}
	// Listings show the remote ETag (documented).
	err := f.st.WalkObjects(f.ctx, "bkt", "", "", func(o storage.ObjectInfo) error {
		if o.ETag == md5hex(data) || o.Size != int64(len(data)) {
			t.Errorf("listed %+v", o)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConditionalWrites(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	// The gos3 ETag differs from the remote one.
	first := f.mustPut("bkt", "k", []byte("one"), storage.ObjectMeta{ETag: "custom-etag"})
	if _, err := f.put("bkt", "k", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-None-Match on existing: %v", err)
	}
	if _, err := f.put("bkt", "k", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: md5hex([]byte("one"))}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match with the remote ETag: %v, want a failure (gos3 ETag is %q)", err, first.ETag)
	}
	if _, err := f.put("bkt", "k", []byte("two"), storage.ObjectMeta{ETag: md5hex([]byte("two"))}, storage.CommitOptions{IfMatch: `"custom-etag"`}); err != nil {
		t.Errorf("If-Match with the gos3 ETag: %v", err)
	}
	if _, err := f.put("bkt", "missing", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: "*"}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match on a missing key: %v", err)
	}
	if _, err := f.put("bkt", "new", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); err != nil {
		t.Errorf("If-None-Match on a new key: %v", err)
	}
	// Someone else replaces the object behind gos3's back between the
	// HEAD and the PUT: the remote If-Match pins the version.
	_, data := f.read("bkt", "k")
	if string(data) != "two" {
		t.Fatalf("content %q", data)
	}
	f.remote.Fail.Store(nil)
	rb, rk := f.remoteKey("bkt", "k")
	var replaced bool
	hook := func(r *http.Request) int {
		if r.Method == http.MethodPut && !replaced && r.Header.Get("If-Match") != "" {
			replaced = true
			if _, err := f.c.PutObject(context.Background(), rb, rk, strings.NewReader("sneaky"), 6, s3client.PutOptions{}); err != nil {
				t.Errorf("sneaky put: %v", err)
			}
		}
		return 0
	}
	f.remote.Fail.Store(&hook)
	_, err := f.put("bkt", "k", []byte("three"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: md5hex([]byte("two"))})
	f.remote.Fail.Store(nil)
	if !replaced || !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match racing a foreign write: replaced %v, err %v", replaced, err)
	}
	if _, data := f.read("bkt", "k"); string(data) != "sneaky" {
		t.Errorf("content %q, want the foreign write", data)
	}
}

func TestUpdateObjectMetaKeepsLastModified(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	data := randData(1000, 5)
	committed := f.mustPut("bkt", "k", data, storage.ObjectMeta{ETag: md5hex(data), ContentType: "text/plain", UserMeta: map[string]string{"u": "1"}})
	time.Sleep(1100 * time.Millisecond) // remote times have second precision
	updated, err := f.st.UpdateObjectMeta(f.ctx, "bkt", "k", "", func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"x": "y"}
		m.LegalHold = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.st.GetObjectInfo(f.ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []storage.ObjectInfo{updated, info} {
		if !o.LastModified.Equal(committed.LastModified) || o.ETag != committed.ETag || o.Tags["x"] != "y" || !o.LegalHold ||
			o.ContentType != "text/plain" || o.UserMeta["u"] != "1" {
			t.Errorf("after update: %+v, want LastModified %v and the new tags", o, committed.LastModified)
		}
	}
	if a := f.remoteHead("bkt", "k"); !a.LastModified.After(committed.LastModified) {
		t.Errorf("remote LastModified %v, expected the rewrite to be newer than %v", a.LastModified, committed.LastModified)
	}
	_, got := f.read("bkt", "k")
	if !bytes.Equal(got, data) {
		t.Error("content changed by a metadata update")
	}
	if _, err := f.st.UpdateObjectMeta(f.ctx, "bkt", "missing", "", func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("update of a missing object: %v", err)
	}
}

func TestRemoteUnavailable(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	f.mustPut("bkt", "k", []byte("x"), storage.ObjectMeta{})
	fail := func(*http.Request) int { return http.StatusServiceUnavailable }
	f.remote.Fail.Store(&fail)
	_, err := f.st.GetObjectInfo(f.ctx, "bkt", "k", "")
	if !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("GetObjectInfo with a failing remote: %v", err)
	}
	_, _, err = f.st.OpenObject(f.ctx, "bkt", "k", "")
	if !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("OpenObject with a failing remote: %v", err)
	}
	if _, err := f.put("bkt", "k2", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("put with a failing remote: %v", err)
	}
	if err := f.st.WalkObjects(f.ctx, "bkt", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("walk with a failing remote: %v", err)
	}
	f.remote.Fail.Store(nil)

	// A remote bucket that vanished is unavailable too (but empty).
	f.attach("gone-bucket")
	if err := f.remote.Backend.DeleteBucket(f.ctx, "gone-bucket"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.WalkObjects(f.ctx, "gone-bucket", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("walk of a vanished remote bucket: %v", err)
	}
	if _, _, err := f.st.OpenObject(f.ctx, "gone-bucket", "k", ""); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("GET in a vanished remote bucket: %v", err)
	}
	if err := f.st.CheckEmpty(f.ctx, "gone-bucket"); err != nil {
		t.Errorf("CheckEmpty of a vanished remote bucket: %v", err)
	}
}

func TestUploadStatePersists(t *testing.T) {
	f := newFixture(t, false, func(o *gateway.Options) { o.SpoolDir = filepath.Join(t.TempDir(), "tmp") })
	f.attach("bkt")
	meta := storage.ObjectMeta{ContentType: "a/b", UserMeta: map[string]string{"k": "v"}}
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "obj", meta, "bob")
	if err != nil {
		t.Fatal(err)
	}
	big, small := randData(5<<20, 6), []byte("tail")
	p1 := uploadPart(t, f, "bkt", "obj", u.UploadID, 1, big)
	uploadPart(t, f, "bkt", "obj", u.UploadID, 2, small)
	other, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "other", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// A stale spool file is removed at Open.
	stale := filepath.Join(f.opts.SpoolDir, "gos3-spool-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.open()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spool file kept: %v", err)
	}
	ups, err := f.st.ListMultipartUploads(f.ctx, "bkt")
	if err != nil || len(ups) != 2 || ups[0].UploadID != u.UploadID || ups[0].Initiator != "bob" || !ups[0].Initiated.Equal(u.Initiated) ||
		ups[0].Meta.UserMeta["k"] != "v" || ups[1].UploadID != other.UploadID {
		t.Fatalf("uploads after reopen: %+v, %v", ups, err)
	}
	parts, err := f.st.ListParts(f.ctx, "bkt", "obj", u.UploadID)
	if err != nil || len(parts) != 2 || parts[0].ETag != p1.ETag || parts[0].Size != int64(len(big)) || parts[1].Size != 4 ||
		parts[0].Checksums["CRC32"] != "1" || !parts[0].LastModified.Equal(p1.LastModified) {
		t.Fatalf("parts after reopen: %+v, %v", parts, err)
	}
	if _, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "obj", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2", ContentType: "a/b"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := f.read("bkt", "obj"); !bytes.Equal(data, slices.Concat(big, small)) {
		t.Error("content after reopen differs")
	}
	if err := f.st.AbortMultipartUpload(f.ctx, "bkt", "other", other.UploadID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if ups, _ := f.st.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
		t.Errorf("finished uploads came back: %+v", ups)
	}
}

func TestCheckEmptyAndDetach(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			f := newFixture(t, shared, nil)
			f.attach("bkt")
			f.attach("nxt") // a neighbour bucket's objects must not count
			f.mustPut("nxt", "k", []byte("x"), storage.ObjectMeta{})
			if err := f.st.CheckEmpty(f.ctx, "bkt"); err != nil {
				t.Errorf("CheckEmpty(empty): %v", err)
			}
			f.mustPut("bkt", "k", []byte("x"), storage.ObjectMeta{})
			if err := f.st.CheckEmpty(f.ctx, "bkt"); !errors.Is(err, storage.ErrBucketNotEmpty) {
				t.Errorf("CheckEmpty(non-empty): %v", err)
			}
			if _, err := f.st.DeleteObject(f.ctx, "bkt", "k", ""); err != nil {
				t.Fatal(err)
			}
			u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "up", storage.ObjectMeta{}, "")
			if err != nil {
				t.Fatal(err)
			}
			uploadPart(t, f, "bkt", "up", u.UploadID, 1, randData(5<<20, 7))
			uploadPart(t, f, "bkt", "up", u.UploadID, 2, []byte("small"))
			if err := f.st.CheckEmpty(f.ctx, "bkt"); err != nil {
				t.Errorf("CheckEmpty with only an upload: %v", err)
			}
			w, err := f.st.NewObjectWriter(f.ctx, "bkt", "late")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.DetachBucket(f.ctx, "bkt"); err != nil {
				t.Fatalf("DetachBucket: %v", err)
			}
			if _, err := w.Commit(f.ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBucketNotFound) {
				t.Errorf("commit of a writer from before DetachBucket: %v", err)
			}
			if ups, _ := f.st.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
				t.Errorf("uploads after DetachBucket: %+v", ups)
			}
			rb, _ := f.remoteKey("bkt", "")
			if ups, _ := f.remote.Backend.ListMultipartUploads(f.ctx, rb); len(ups) != 0 {
				t.Errorf("remote uploads after DetachBucket: %+v", ups)
			}
			if staged := stagedFiles(t, f); len(staged) != 0 {
				t.Errorf("staged parts after DetachBucket: %q", staged)
			}
			if got := f.walkKeys("nxt", "", ""); !slices.Equal(got, []string{"k"}) {
				t.Errorf("neighbour bucket: %q", got)
			}
		})
	}
}

func TestOpenOptions(t *testing.T) {
	r := s3clienttest.Start(t)
	if _, err := gateway.Open(gateway.Options{Name: "gw", Client: r.Client(t)}); err == nil {
		t.Error("Open without State succeeded")
	}
	if _, err := gateway.Open(gateway.Options{Name: "gw", State: gateway.MemoryState()}); err == nil {
		t.Error("Open without Client succeeded")
	}
	dir := filepath.Join(t.TempDir(), "a", "b")
	st, err := gateway.Open(gateway.Options{Name: "gw", Client: r.Client(t), State: gateway.MemoryState(), SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("spool directory %s: %v", dir, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := st.CreateMultipartUpload(t.Context(), "bkt", "k", storage.ObjectMeta{}, ""); err == nil {
		t.Error("CreateMultipartUpload after Close succeeded")
	}
}

func TestReservedBucket(t *testing.T) {
	var asked []string
	reserved := func(rb string) bool {
		asked = append(asked, rb)
		return rb == "gos3-meta"
	}
	f := newFixture(t, false, func(o *gateway.Options) { o.Reserved = reserved })
	err := f.st.AttachBucket(f.ctx, storage.BucketInfo{Name: "gos3-meta"})
	if !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("AttachBucket of a reserved bucket: %v, want ErrBucketExists", err)
	}
	if err := f.c.HeadBucket(f.ctx, "gos3-meta"); !s3client.IsNotFound(err) {
		t.Errorf("reserved remote bucket: HeadBucket = %v, want it never created", err)
	}
	f.attach("other")
	if !slices.Equal(asked, []string{"gos3-meta", "other"}) {
		t.Errorf("Reserved asked about %q", asked)
	}

	// In shared mode gos3 buckets live under a prefix: no name is reserved.
	g := newFixture(t, true, func(o *gateway.Options) { o.Reserved = reserved })
	g.attach("gos3-meta")
}

// putRemote stores an object directly on the remote.
func putRemote(t *testing.T, f *fixture, bucket, key string) {
	t.Helper()
	if _, err := f.c.PutObject(f.ctx, bucket, key, bytes.NewReader([]byte("{}")), 2, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseBackingState(t *testing.T) {
	// Per-bucket mode: an existing remote bucket with a gos3.json marker
	// at its root (an s3 backing's bucket) is not adopted, whatever
	// Reserved says.
	f := newFixture(t, false, nil)
	f.remote.CreateBucket(t, "backing-bkt")
	putRemote(t, f, "backing-bkt", "gos3.json")
	putRemote(t, f, "backing-bkt", "meta/lease.json")
	err := f.st.AttachBucket(f.ctx, storage.BucketInfo{Name: "backing-bkt"})
	if !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("AttachBucket of a backing's bucket: %v, want ErrBucketExists", err)
	}
	// A marker deeper down (an s3 backing with a prefix) is not looked
	// for: Reserved covers those buckets.
	f.remote.CreateBucket(t, "nested")
	putRemote(t, f, "nested", "some/prefix/gos3.json")
	f.attach("nested")
	// A plain existing bucket is adopted.
	f.remote.CreateBucket(t, "plain")
	putRemote(t, f, "plain", "data.json")
	f.attach("plain")

	// A marker at the gateway's own prefix is refused too.
	g := newFixture(t, false, func(o *gateway.Options) { o.Prefix = "gw/" })
	g.remote.CreateBucket(t, "pfx-bkt")
	putRemote(t, g, "pfx-bkt", "gw/gos3.json")
	if err := g.st.AttachBucket(g.ctx, storage.BucketInfo{Name: "pfx-bkt"}); !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("AttachBucket with a marker at the prefix: %v, want ErrBucketExists", err)
	}

	// Shared mode: a gos3 bucket whose prefix holds a backing's marker.
	h := newFixture(t, true, nil)
	putRemote(t, h, "shared", "pfx/meta-b/gos3.json")
	if err := h.st.AttachBucket(h.ctx, storage.BucketInfo{Name: "meta-b"}); !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("shared-mode AttachBucket over a backing's prefix: %v, want ErrBucketExists", err)
	}
	h.attach("fine")
}

// faultyState is a persistent StateStore (it outlives the stores opened
// on it) whose operations can be made to fail.
type faultyState struct {
	gateway.StateStore

	mu                                 sync.Mutex
	failRecords, failList              bool
	failPutRecord, failPutStaged       bool
	failDelete                         bool // DeleteRecords
	landThenFail                       bool // PutRecord writes, then fails
	recordsCalls, listCalls, openCalls int
}

var errInjected = errors.New("injected state failure")

func (s *faultyState) fail(flag *bool, calls *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if calls != nil {
		*calls++
	}
	if *flag {
		return errInjected
	}
	return nil
}

func (s *faultyState) set(flag *bool, v bool) {
	s.mu.Lock()
	*flag = v
	s.mu.Unlock()
}

func (s *faultyState) Records(ctx context.Context) (map[string][]byte, error) {
	if err := s.fail(&s.failRecords, &s.recordsCalls); err != nil {
		return nil, err
	}
	return s.StateStore.Records(ctx)
}

func (s *faultyState) PutRecord(ctx context.Context, key string, val []byte) error {
	if err := s.fail(&s.failPutRecord, nil); err != nil {
		return err
	}
	if err := s.StateStore.PutRecord(ctx, key, val); err != nil {
		return err
	}
	return s.fail(&s.landThenFail, nil)
}

func (s *faultyState) DeleteRecords(ctx context.Context, prefix string) error {
	if err := s.fail(&s.failDelete, nil); err != nil {
		return err
	}
	return s.StateStore.DeleteRecords(ctx, prefix)
}

func (s *faultyState) PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error {
	if err := s.fail(&s.failPutStaged, nil); err != nil {
		return err
	}
	return s.StateStore.PutStaged(ctx, name, r, size)
}

func (s *faultyState) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	s.openCalls++
	s.mu.Unlock()
	return s.StateStore.OpenStaged(ctx, name, size)
}

func (s *faultyState) ListStaged(ctx context.Context) ([]string, error) {
	if err := s.fail(&s.failList, &s.listCalls); err != nil {
		return nil, err
	}
	return s.StateStore.ListStaged(ctx)
}

// stagedOf returns the staged parts of upload id.
func stagedOf(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	var out []string
	for _, name := range stagedFiles(t, f) {
		if strings.HasPrefix(name, id+"-") {
			out = append(out, name)
		}
	}
	return out
}

func TestStateStorePersistence(t *testing.T) {
	state := &faultyState{StateStore: gateway.MemoryState()}
	f := newFixture(t, false, func(o *gateway.Options) { o.State = state })
	f.attach("bkt")
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "obj", storage.ObjectMeta{ContentType: "a/b"}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	big, small := randData(5<<20, 6), []byte("tail")
	uploadPart(t, f, "bkt", "obj", u.UploadID, 1, big)
	uploadPart(t, f, "bkt", "obj", u.UploadID, 2, []byte("replaced"))
	uploadPart(t, f, "bkt", "obj", u.UploadID, 2, small)
	if staged := stagedOf(t, f, u.UploadID); len(staged) != 1 {
		t.Errorf("staged parts %q, want one (the replaced part's blob deleted)", staged)
	}
	other, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "other", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "other", other.UploadID, 1, []byte("lost"))
	uploadPart(t, f, "bkt", "other", other.UploadID, 2, randData(5<<20, 8))

	recs, err := state.Records(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"u/" + u.UploadID, "p/" + u.UploadID + "/00001", "p/" + u.UploadID + "/00002", "u/" + other.UploadID} {
		if _, ok := recs[k]; !ok {
			t.Errorf("record %q missing (have %d records)", k, len(recs))
		}
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}

	// Damage the state while the store is closed: an unreferenced staged
	// blob, part records without an upload, a part whose blob is gone.
	ctx := f.ctx
	if err := state.PutStaged(ctx, "orphan", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	if err := state.PutRecord(ctx, "p/nosuchupload/00001", []byte(`{"n":1,"size":1,"etag":"e","file":"x"}`)); err != nil {
		t.Fatal(err)
	}
	lost := stagedOf(t, f, other.UploadID)
	if len(lost) != 1 {
		t.Fatalf("staged parts of the other upload: %q", lost)
	}
	if err := state.DeleteStaged(ctx, lost[0]); err != nil {
		t.Fatal(err)
	}

	// Failing to read the records fails Open.
	state.set(&state.failRecords, true)
	if _, err := gateway.Open(f.opts); !errors.Is(err, errInjected) {
		t.Errorf("Open with failing records: %v", err)
	}
	state.set(&state.failRecords, false)

	// Failing to list staged parts does not: stale state is kept.
	state.set(&state.failList, true)
	f.open()
	if !slices.Contains(stagedFiles(t, f), "orphan") {
		t.Error("orphan blob removed although listing failed")
	}
	if parts, err := f.st.ListParts(ctx, "bkt", "other", other.UploadID); err != nil || len(parts) != 2 {
		t.Errorf("parts of the other upload without a listing: %+v, %v", parts, err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	state.set(&state.failList, false)

	// A clean Open removes the stale state.
	f.open()
	if slices.Contains(stagedFiles(t, f), "orphan") {
		t.Error("orphan blob kept")
	}
	recs, _ = state.Records(ctx)
	if _, ok := recs["p/nosuchupload/00001"]; ok {
		t.Error("part record without an upload kept")
	}
	if _, ok := recs[fmt.Sprintf("p/%s/00001", other.UploadID)]; ok {
		t.Error("part record of a missing staged part kept")
	}
	parts, err := f.st.ListParts(ctx, "bkt", "other", other.UploadID)
	if err != nil || len(parts) != 1 || parts[0].PartNumber != 2 {
		t.Errorf("parts of the other upload: %+v, %v; want part 2 only", parts, err)
	}
	ups, err := f.st.ListMultipartUploads(ctx, "bkt")
	if err != nil || len(ups) != 2 || ups[0].UploadID != u.UploadID || ups[0].Initiator != "bob" {
		t.Fatalf("uploads after reopen: %+v, %v", ups, err)
	}
	if _, err := f.st.CompleteMultipartUpload(ctx, "bkt", "obj", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2", ContentType: "a/b"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := f.read("bkt", "obj"); !bytes.Equal(data, slices.Concat(big, small)) {
		t.Error("content after reopen differs")
	}
	if staged := stagedOf(t, f, u.UploadID); len(staged) != 0 {
		t.Errorf("staged parts left after completion: %q", staged)
	}
	recs, _ = state.Records(ctx)
	for k := range recs {
		if strings.Contains(k, u.UploadID) {
			t.Errorf("record %q left after completion", k)
		}
	}

	// Write failures fail the operation and change nothing.
	state.set(&state.failPutStaged, true)
	w, err := f.st.NewPartWriter(ctx, "bkt", "other", other.UploadID, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("small"))
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: "x"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit with failing staging: %v", err)
	}
	state.set(&state.failPutStaged, false)
	state.set(&state.failPutRecord, true)
	w, err = f.st.NewPartWriter(ctx, "bkt", "other", other.UploadID, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("small"))
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: "x"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit with failing records: %v", err)
	}
	// The part record may have been written: its staged blob is kept
	// (until an Open finds no record referring to it).
	if staged := stagedOf(t, f, other.UploadID); len(staged) != 1 {
		t.Errorf("staged blobs after a failed part record write: %q, want the new one kept", staged)
	}
	if _, err := f.st.CreateMultipartUpload(ctx, "bkt", "new", storage.ObjectMeta{}, ""); !errors.Is(err, errInjected) {
		t.Errorf("CreateMultipartUpload with failing records: %v", err)
	}
	state.set(&state.failPutRecord, false)
	if parts, _ := f.st.ListParts(ctx, "bkt", "other", other.UploadID); len(parts) != 1 {
		t.Errorf("parts after failed commits: %+v", parts)
	}
	if ups, _ := f.st.ListMultipartUploads(ctx, "bkt"); len(ups) != 1 {
		t.Errorf("uploads after a failed create: %+v", ups)
	}
}

func TestStagedCompletionReadsLazily(t *testing.T) {
	state := &faultyState{StateStore: gateway.MemoryState()}
	f := newFixture(t, false, func(o *gateway.Options) { o.State = state })
	f.attach("bkt")
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for n := 1; n <= 4; n++ {
		p := randData(1<<20+n, byte(n))
		want = append(want, p...)
		uploadPart(t, f, "bkt", "k", u.UploadID, n, p)
	}
	// Complete parts 1, 3 and 4 only: part 2 is never opened.
	if _, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "k", u.UploadID, []int{1, 3, 4}, storage.ObjectMeta{ETag: "e-3"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	p2 := 1<<20 + 1
	want = slices.Concat(want[:p2], want[p2+1<<20+2:])
	if _, data := f.read("bkt", "k"); !bytes.Equal(data, want) {
		t.Error("content differs")
	}
	state.mu.Lock()
	opened := state.openCalls
	state.mu.Unlock()
	if opened != 3 {
		t.Errorf("staged parts opened %d times, want 3", opened)
	}
}

func TestReaderFailsOnReplacedObject(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	v1 := randData(100<<10, 1)
	f.mustPut("bkt", "k", v1, storage.ObjectMeta{ETag: md5hex(v1)})
	_, r, err := f.st.OpenObject(f.ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f.mustPut("bkt", "k", []byte("v2"), storage.ObjectMeta{})
	if _, err := r.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("read after replacement: %v, want an error wrapping ErrPreconditionFailed", err)
	}
}

func TestInvalidVersionsAndKeys(t *testing.T) {
	f := newFixture(t, true, nil)
	f.attach("bkt")
	f.mustPut("bkt", "k", []byte("x"), storage.ObjectMeta{})
	if _, err := f.st.GetObjectInfo(f.ctx, "bkt", "k", "v123"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Errorf("unknown version: %v", err)
	}
	if res, err := f.st.DeleteObject(f.ctx, "bkt", "k", storage.NullVersionID); err != nil || res.VersionID != storage.NullVersionID {
		t.Errorf("delete null: %+v %v", res, err)
	}
	if _, err := f.st.DeleteObject(f.ctx, "bkt", "k", storage.NullVersionID); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Errorf("delete missing null: %v", err)
	}
	long := strings.Repeat("k", 1024-len("pfx/bkt/")+1)
	if _, err := f.st.NewObjectWriter(f.ctx, "bkt", long); !errors.Is(err, storage.ErrInvalidName) {
		t.Errorf("too long key: %v", err)
	}
	if _, err := f.st.NewObjectWriter(f.ctx, "bkt", long[1:]); err != nil {
		t.Errorf("longest key: %v", err)
	}
}

// TestConditionalRaces covers the unversioned halves of the conformance
// races the suite only runs on versioned buckets.
func TestConditionalRaces(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	base := f.mustPut("bkt", "k", []byte("base"), storage.ObjectMeta{ETag: "gos3-etag"})
	race := func(n int, commit func(i int) error) []int {
		var (
			mu      sync.Mutex
			wg      sync.WaitGroup
			winners []int
			start   = make(chan struct{})
		)
		for i := range n {
			wg.Go(func() {
				<-start
				err := commit(i)
				if err != nil && !errors.Is(err, storage.ErrPreconditionFailed) {
					t.Errorf("racer %d: %v", i, err)
				}
				if err == nil {
					mu.Lock()
					winners = append(winners, i)
					mu.Unlock()
				}
			})
		}
		close(start)
		wg.Wait()
		return winners
	}
	writers := make([]storage.ObjectWriter, 8)
	for i := range writers {
		w, err := f.st.NewObjectWriter(f.ctx, "bkt", "k")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "racer %d", i)
		writers[i] = w
	}
	winners := race(len(writers), func(i int) error {
		_, err := writers[i].Commit(f.ctx, storage.ObjectMeta{ETag: md5hex(fmt.Appendf(nil, "racer %d", i))}, storage.CommitOptions{IfMatch: base.ETag})
		return err
	})
	if len(winners) != 1 {
		t.Fatalf("If-Match winners %v, want exactly one", winners)
	}
	if _, data := f.read("bkt", "k"); string(data) != fmt.Sprintf("racer %d", winners[0]) {
		t.Errorf("content %q, winner %d", data, winners[0])
	}

	ids := make([]string, 6)
	for i := range ids {
		u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "mp", storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		uploadPart(t, f, "bkt", "mp", u.UploadID, 1, fmt.Appendf(nil, "part of %d", i))
		ids[i] = u.UploadID
	}
	winners = race(len(ids), func(i int) error {
		_, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "mp", ids[i], []int{1}, storage.ObjectMeta{ETag: "x-1"}, storage.CommitOptions{IfNoneMatch: true})
		return err
	})
	if len(winners) != 1 {
		t.Fatalf("If-None-Match completion winners %v, want exactly one", winners)
	}
	if _, data := f.read("bkt", "mp"); string(data) != fmt.Sprintf("part of %d", winners[0]) {
		t.Errorf("content %q, winner %d", data, winners[0])
	}
	// Losing uploads are kept (the client may retry or abort).
	if ups, _ := f.st.ListMultipartUploads(f.ctx, "bkt"); len(ups) != len(ids)-1 {
		t.Errorf("%d uploads left, want %d", len(ups), len(ids)-1)
	}
}

// TestAmbiguousStateWrites covers state writes that report an error but
// landed: nothing they refer to may be deleted.
func TestAmbiguousStateWrites(t *testing.T) {
	state := &faultyState{StateStore: gateway.MemoryState()}
	f := newFixture(t, false, func(o *gateway.Options) { o.State = state })
	f.attach("bkt")
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "k", u.UploadID, 1, []byte("first,"))

	state.set(&state.landThenFail, true)
	if _, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "ghost", storage.ObjectMeta{}, ""); !errors.Is(err, errInjected) {
		t.Errorf("CreateMultipartUpload: %v", err)
	}
	w, err := f.st.NewPartWriter(f.ctx, "bkt", "k", u.UploadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("second"))
	if _, err := w.Commit(f.ctx, storage.PartMeta{ETag: "e2"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit: %v", err)
	}
	state.set(&state.landThenFail, false)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}

	// After a restart the landed part record is back and its staged part
	// readable; the failed upload creation left nothing behind.
	f.open()
	ups, err := f.st.ListMultipartUploads(f.ctx, "bkt")
	if err != nil || len(ups) != 1 || ups[0].UploadID != u.UploadID {
		t.Fatalf("uploads after reopen: %+v, %v", ups, err)
	}
	if parts, err := f.st.ListParts(f.ctx, "bkt", "k", u.UploadID); err != nil || len(parts) != 2 {
		t.Fatalf("parts after reopen: %+v, %v", parts, err)
	}
	if _, err := f.st.CompleteMultipartUpload(f.ctx, "bkt", "k", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := f.read("bkt", "k"); string(data) != "first,second" {
		t.Errorf("content %q", data)
	}
}

func TestAbortRetriesRecordDeletion(t *testing.T) {
	state := &faultyState{StateStore: gateway.MemoryState()}
	f := newFixture(t, false, func(o *gateway.Options) { o.State = state })
	f.attach("bkt")
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "k", u.UploadID, 1, randData(5<<20, 1))
	uploadPart(t, f, "bkt", "k", u.UploadID, 2, []byte("tail"))

	state.set(&state.failDelete, true)
	if err := f.st.AbortMultipartUpload(f.ctx, "bkt", "k", u.UploadID); !errors.Is(err, errInjected) {
		t.Errorf("Abort with failing record deletion: %v", err)
	}
	// The upload is kept with its staged part, so the abort can be retried.
	if _, err := f.st.GetMultipartUpload(f.ctx, "bkt", "k", u.UploadID); err != nil {
		t.Errorf("upload after a failed abort: %v", err)
	}
	if staged := stagedOf(t, f, u.UploadID); len(staged) != 1 {
		t.Errorf("staged parts after a failed abort: %q", staged)
	}
	state.set(&state.failDelete, false)
	// The remote upload is already aborted: the retry still succeeds.
	if err := f.st.AbortMultipartUpload(f.ctx, "bkt", "k", u.UploadID); err != nil {
		t.Fatalf("retried Abort: %v", err)
	}
	if staged := stagedOf(t, f, u.UploadID); len(staged) != 0 {
		t.Errorf("staged parts after abort: %q", staged)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if ups, _ := f.st.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
		t.Errorf("aborted upload came back: %+v", ups)
	}
}

func TestVanishedRemoteUpload(t *testing.T) {
	f := newFixture(t, false, nil)
	f.attach("bkt")
	abortRemote := func(key string) {
		t.Helper()
		ups, err := f.remote.Backend.ListMultipartUploads(f.ctx, "bkt")
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range ups {
			if u.Key == key {
				if err := f.c.AbortMultipartUpload(f.ctx, "bkt", key, u.UploadID); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatalf("no remote upload of %s", key)
	}

	// The next remote part finds the remote upload gone.
	u, err := f.st.CreateMultipartUpload(f.ctx, "bkt", "a", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "a", u.UploadID, 1, randData(5<<20, 1))
	abortRemote("a")
	w, err := f.st.NewPartWriter(f.ctx, "bkt", "a", u.UploadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(randData(5<<20, 2))
	if _, err := w.Commit(f.ctx, storage.PartMeta{ETag: "e"}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("part commit to a vanished remote upload: %v, want ErrUploadNotFound", err)
	}
	if _, err := f.st.GetMultipartUpload(f.ctx, "bkt", "a", u.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("upload after its remote upload vanished: %v", err)
	}

	// So does the completion.
	u, err = f.st.CreateMultipartUpload(f.ctx, "bkt", "b", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, f, "bkt", "b", u.UploadID, 1, randData(5<<20, 3))
	uploadPart(t, f, "bkt", "b", u.UploadID, 2, []byte("tail"))
	abortRemote("b")
	_, err = f.st.CompleteMultipartUpload(f.ctx, "bkt", "b", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("completion of a vanished remote upload: %v, want ErrUploadNotFound", err)
	}
	if ups, _ := f.st.ListMultipartUploads(f.ctx, "bkt"); len(ups) != 0 {
		t.Errorf("uploads left: %+v", ups)
	}
	if staged := stagedFiles(t, f); len(staged) != 0 {
		t.Errorf("staged parts left: %q", staged)
	}
}

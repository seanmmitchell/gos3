package native_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// --- helpers ---

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func randData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

// mk creates gos3 bucket name (remote bucket nb-<name>).
func (h *harness) mk(name string, opts storage.CreateBucketOptions) {
	h.t.Helper()
	if err := h.CreateBucket(h.t.Context(), name, opts); err != nil {
		h.t.Fatalf("CreateBucket(%s): %v", name, err)
	}
}

// mkVersioned creates a bucket with versioning enabled.
func (h *harness) mkVersioned(name string) {
	h.t.Helper()
	h.mk(name, storage.CreateBucketOptions{})
	if err := h.SetBucketVersioning(h.t.Context(), name, storage.VersioningEnabled); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) put(bucket, key string, data []byte, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	h.t.Helper()
	return h.putCtx(h.t.Context(), bucket, key, data, meta, opts)
}

func (h *harness) putCtx(ctx context.Context, bucket, key string, data []byte, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w, err := h.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if _, err := w.Write(data); err != nil {
		return storage.ObjectInfo{}, err
	}
	info, err := w.Commit(ctx, meta, opts)
	if err != nil {
		_ = w.Abort()
	}
	return info, err
}

func (h *harness) mustPut(bucket, key string, data []byte, meta storage.ObjectMeta) storage.ObjectInfo {
	h.t.Helper()
	info, err := h.put(bucket, key, data, meta, storage.CommitOptions{})
	if err != nil {
		h.t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
	return info
}

func (h *harness) read(bucket, key, vid string) (storage.ObjectInfo, []byte) {
	h.t.Helper()
	info, r, err := h.OpenObject(h.t.Context(), bucket, key, vid)
	if err != nil {
		h.t.Fatalf("OpenObject(%s/%s@%s): %v", bucket, key, vid, err)
	}
	if r == nil {
		return info, nil
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		h.t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return info, data
}

func (h *harness) info(bucket, key, vid string) storage.ObjectInfo {
	h.t.Helper()
	info, err := h.GetObjectInfo(h.t.Context(), bucket, key, vid)
	if err != nil {
		h.t.Fatalf("GetObjectInfo(%s/%s@%s): %v", bucket, key, vid, err)
	}
	return info
}

func (h *harness) remoteHead(bucket, key, vid string) s3client.ObjectAttrs {
	h.t.Helper()
	a, err := h.c.HeadObjectWith(h.t.Context(), remoteName(bucket), key, s3client.HeadOptions{VersionID: vid})
	if err != nil {
		h.t.Fatalf("remote HEAD %s/%s: %v", bucket, key, err)
	}
	return a
}

// remotePut writes an object directly on the remote (another client).
func (h *harness) remotePut(rb, key string, data []byte, hdr http.Header) s3client.WriteResult {
	h.t.Helper()
	res, err := h.c.Put(h.t.Context(), rb, key, bytes.NewReader(data), int64(len(data)), s3client.PutOptions{Header: hdr})
	if err != nil {
		h.t.Fatalf("remote PUT %s/%s: %v", rb, key, err)
	}
	return res
}

func (h *harness) walkKeys(bucket, prefix, after string) []string {
	h.t.Helper()
	var keys []string
	err := h.WalkObjects(h.t.Context(), bucket, prefix, after, func(o storage.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	if err != nil {
		h.t.Fatalf("WalkObjects: %v", err)
	}
	return keys
}

func (h *harness) versions(bucket, prefix, km, vm string) []storage.ObjectInfo {
	h.t.Helper()
	var out []storage.ObjectInfo
	err := h.WalkVersions(h.t.Context(), bucket, prefix, km, vm, func(o storage.ObjectInfo) error {
		out = append(out, o)
		return nil
	})
	if err != nil {
		h.t.Fatalf("WalkVersions: %v", err)
	}
	return out
}

// vrefs renders versions as key@id[*: latest][(dm)].
func vrefs(vs []storage.ObjectInfo) []string {
	var out []string
	for _, v := range vs {
		s := v.Key + "@" + v.VersionID
		if v.IsLatest {
			s += "*"
		}
		if v.DeleteMarker {
			s += "(dm)"
		}
		out = append(out, s)
	}
	return out
}

// remoteVersions lists the remote bucket's versions directly.
func (h *harness) remoteVersions(rb string) []storage.ObjectInfo {
	h.t.Helper()
	var out []storage.ObjectInfo
	in := s3client.VersionListInput{}
	for {
		page, err := h.c.ListObjectVersions(h.t.Context(), rb, in)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, e := range page.Entries {
			vid := e.VersionID
			if vid == "" {
				vid = storage.NullVersionID
			}
			out = append(out, storage.ObjectInfo{Key: e.Key, VersionID: vid, IsLatest: e.IsLatest, DeleteMarker: e.DeleteMarker})
		}
		if !page.IsTruncated {
			return out
		}
		in.KeyMarker, in.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
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
		if len(m.UserMeta) == 0 {
			m.UserMeta = nil
		}
		if len(m.Tags) == 0 {
			m.Tags = nil
		}
		if len(m.Checksums) == 0 {
			m.Checksums = nil
		}
		return fmt.Sprintf("%+v|%d", m, ru.UnixNano())
	}
	if norm(want) != norm(got) {
		t.Errorf("%s: metadata mismatch:\n got  %s\n want %s", what, norm(got), norm(want))
	}
}

// reqLog records the requests a remote serves (s3clienttest Wrap).
type reqLog struct {
	mu   sync.Mutex
	reqs []loggedReq
}

type loggedReq struct {
	method string
	path   string // unescaped, "/<bucket>/<key>"
	query  url.Values
	header http.Header
}

func (l *reqLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.reqs = append(l.reqs, loggedReq{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone()})
		l.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (l *reqLog) reset() {
	l.mu.Lock()
	l.reqs = nil
	l.mu.Unlock()
}

func (l *reqLog) all() []loggedReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// --- objects and metadata ---

func TestNativeObjects(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	data := []byte("hello native")
	meta := storage.ObjectMeta{ETag: md5hex(data), ContentType: "text/plain", UserMeta: map[string]string{"color": "blue"}}
	h.mustPut("bkt", "dir/k.txt", data, meta)

	// Plain objects carry no gos3-meta: they look like any client's.
	a := h.remoteHead("bkt", "dir/k.txt", "")
	if a.ETag != md5hex(data) || a.Size != int64(len(data)) || a.Header.Get("Content-Type") != "text/plain" ||
		a.Header.Get("X-Amz-Meta-Color") != "blue" || a.Header.Get("X-Amz-Meta-Gos3-Meta") != "" {
		t.Errorf("remote object = %+v %v, want native headers without gos3-meta", a, a.Header)
	}

	// Valid tags are native, the ACL is gos3's own (an overlay record).
	meta.Tags = map[string]string{"t": "1", "with space": "a+b=c"}
	meta.ACL = "public-read"
	h.mustPut("bkt", "tagged", data, meta)
	a = h.remoteHead("bkt", "tagged", "")
	if a.Header.Get("X-Amz-Meta-Gos3-Meta") != "" || a.Header.Get("X-Amz-Tagging-Count") != "2" {
		t.Errorf("remote headers %v, want 2 native tags and no gos3-meta", a.Header)
	}
	if n := native.OverlayCount(h.st, "bkt", "tagged"); n != 1 {
		t.Errorf("%d overlays for the object with an ACL, want 1", n)
	}
	tags, err := h.c.GetObjectTagging(t.Context(), "nb-bkt", "tagged", "")
	if err != nil || !maps.Equal(tags, meta.Tags) {
		t.Errorf("remote tags %v, %v; want %v", tags, err, meta.Tags)
	}
	metaEqual(t, "tagged", meta, h.info("bkt", "tagged", "").ObjectMeta)

	// Tags AWS refuses stay in gos3-meta.
	meta.Tags = map[string]string{"bad": "a&b"}
	h.mustPut("bkt", "badtags", data, meta)
	if a := h.remoteHead("bkt", "badtags", ""); a.Header.Get("X-Amz-Tagging-Count") != "" {
		t.Errorf("invalid tags sent natively: %v", a.Header)
	}
	metaEqual(t, "badtags", meta, h.info("bkt", "badtags", "").ObjectMeta)
}

func TestMetadataRoundTrip(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("plain", storage.CreateBucketOptions{})
	h.mkVersioned("ver")
	h.mk("lock", storage.CreateBucketOptions{ObjectLockEnabled: true})
	data := []byte("\x1f\x8b not really gzip, but stored as such")
	big := strings.Repeat("v", 1000)
	for _, bucket := range []string{"plain", "ver", "lock"} {
		metas := []storage.ObjectMeta{fullMeta(), {}, {ETag: "x", UserMeta: map[string]string{}},
			// Native user metadata leaving little room for gos3-meta: the
			// rest goes to an overlay.
			{ETag: "y", UserMeta: map[string]string{"a": big, "b": big[:900], "c": "ü" + big[:40]}, ACL: "public-read",
				PartSizes: []int64{1, 2, 3}, Checksums: map[string]string{"CRC32": "AAAAAA=="}},
			{ContentType: "binary/octet-stream", RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour).Round(time.Microsecond)},
		}
		if bucket == "lock" {
			// COMPLIANCE could never be removed again on a real service;
			// the in-memory remote does not care.
			metas[0].RetainUntil = time.Now().Add(time.Hour)
		}
		for i, meta := range metas {
			key := fmt.Sprintf("k%d", i)
			committed := h.mustPut(bucket, key, data, meta)
			metaEqual(t, bucket+"/"+key+" commit", meta, committed.ObjectMeta)
			head := h.info(bucket, key, "")
			metaEqual(t, bucket+"/"+key+" HEAD", meta, head.ObjectMeta)
			if !head.LastModified.Equal(committed.LastModified) || head.Size != int64(len(data)) || head.VersionID != committed.VersionID {
				t.Errorf("%s/%s: HEAD %v/%d/%s, commit %v/%s", bucket, key, head.LastModified, head.Size, head.VersionID, committed.LastModified, committed.VersionID)
			}
			info, got := h.read(bucket, key, committed.VersionID)
			metaEqual(t, bucket+"/"+key+" GET", meta, info.ObjectMeta)
			if !bytes.Equal(got, data) {
				t.Errorf("%s/%s: content %q, want %q (Content-Encoding must not be decoded)", bucket, key, got, data)
			}
		}
		if bucket == "lock" {
			// Retention and legal hold are native: the remote enforces them.
			a := h.remoteHead(bucket, "k0", "")
			if a.Header.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" || a.Header.Get("X-Amz-Object-Lock-Legal-Hold") != "ON" {
				t.Errorf("lock bucket: remote headers %v, want native object lock settings", a.Header)
			}
		}
	}
	if n := native.OverlayCount(h.st, "plain", "k3"); n != 1 {
		t.Errorf("%d overlays for the object whose metadata did not fit, want 1", n)
	}
}

func TestWalkRestartAfterInvalidUTF8(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%v", versioned), func(t *testing.T) {
			h := newHarness(t, s3clienttest.Options{}, nil)
			if versioned {
				h.mkVersioned("bkt")
			} else {
				h.mk("bkt", storage.CreateBucketOptions{})
			}
			native.SetListPage(h.st, 3)
			keys := []string{"dir", "dir/a", "dir/b/c", "dir/\U0010FFFF", "dir/\U0010FFFFx", "dir0", "dis", "é", "éa", "z"}
			for _, k := range keys {
				h.mustPut("bkt", k, nil, storage.ObjectMeta{ETag: md5hex(nil)})
			}
			for _, tc := range []struct {
				prefix, after string
				want          []string
			}{
				{"", "dir/\xff", []string{"dir0", "dis", "z", "é", "éa"}},
				{"dir/", "dir/\xff", nil},
				{"", "dir/b/\xff", []string{"dir/\U0010FFFF", "dir/\U0010FFFFx", "dir0", "dis", "z", "é", "éa"}},
				{"", "\xc3", []string{"é", "éa"}},
				{"", "é\xff", nil},
				{"", "d\xc3", []string{"z", "é", "éa"}},
				{"", "dir/\U0010FFFF", []string{"dir/\U0010FFFFx", "dir0", "dis", "z", "é", "éa"}},
				{"\xc3", "", []string{"é", "éa"}},
			} {
				if got := h.walkKeys("bkt", tc.prefix, tc.after); !slices.Equal(got, tc.want) {
					t.Errorf("WalkObjects(prefix %q, after %q) = %q, want %q", tc.prefix, tc.after, got, tc.want)
				}
			}
			res, err := storage.ListObjects(t.Context(), h, "bkt", storage.ListParams{Delimiter: "/", MaxKeys: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Objects) != 1 || res.Objects[0].Key != "dir" || !slices.Equal(res.CommonPrefixes, []string{"dir/"}) || !res.IsTruncated {
				t.Errorf("ListObjects page 1 = %v %q %v", res.Objects, res.CommonPrefixes, res.IsTruncated)
			}
			vres, err := storage.ListObjectVersions(t.Context(), h, "bkt", storage.VersionListParams{Delimiter: "/", KeyMarker: "dir/"})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, v := range vres.Versions {
				got = append(got, v.Key)
			}
			if want := []string{"dir0", "dis", "z", "é", "éa"}; !slices.Equal(got, want) {
				t.Errorf("ListObjectVersions after the common prefix dir/ = %q, want %q", got, want)
			}
		})
	}
}

func TestLargeSinglePutUsesMultipart(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.PartSize = 5 << 20 })
	h.mk("bkt", storage.CreateBucketOptions{})
	data := randData(6<<20, 4)
	info := h.mustPut("bkt", "k", data, storage.ObjectMeta{ETag: md5hex(data)})
	if a := h.remoteHead("bkt", "k", ""); !strings.Contains(a.ETag, "-") {
		t.Errorf("remote ETag %q, want a multipart upload", a.ETag)
	}
	got, content := h.read("bkt", "k", "")
	if got.ETag != md5hex(data) || !bytes.Equal(content, data) || !got.LastModified.Equal(info.LastModified) {
		t.Errorf("GET ETag %q (want the single-PUT ETag %q), content equal %v", got.ETag, md5hex(data), bytes.Equal(content, data))
	}
	// Listings apply the overlay holding the gos3 ETag.
	err := h.WalkObjects(t.Context(), "bkt", "", "", func(o storage.ObjectInfo) error {
		if o.ETag != md5hex(data) || o.Size != int64(len(data)) {
			t.Errorf("listed %+v, want the gos3 ETag", o)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConditionalWrites(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	// The gos3 ETag differs from the remote one.
	first := h.mustPut("bkt", "k", []byte("one"), storage.ObjectMeta{ETag: "custom-etag"})
	if _, err := h.put("bkt", "k", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-None-Match on existing: %v", err)
	}
	if _, err := h.put("bkt", "k", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: md5hex([]byte("one"))}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match with the remote ETag: %v, want a failure (gos3 ETag is %q)", err, first.ETag)
	}
	if _, err := h.put("bkt", "k", []byte("two"), storage.ObjectMeta{ETag: md5hex([]byte("two"))}, storage.CommitOptions{IfMatch: `"custom-etag"`}); err != nil {
		t.Errorf("If-Match with the gos3 ETag: %v", err)
	}
	if _, err := h.put("bkt", "missing", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: "*"}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match on a missing key: %v", err)
	}
	// Someone else replaces the object behind gos3's back between the
	// HEAD and the PUT: the remote If-Match pins the version.
	var replaced bool
	hook := func(r *http.Request) int {
		if r.Method == http.MethodPut && !replaced && r.Header.Get("If-Match") != "" {
			replaced = true
			h.remotePut("nb-bkt", "k", []byte("sneaky"), nil)
		}
		return 0
	}
	h.remote.Fail.Store(&hook)
	_, err := h.put("bkt", "k", []byte("three"), storage.ObjectMeta{}, storage.CommitOptions{IfMatch: md5hex([]byte("two"))})
	h.remote.Fail.Store(nil)
	if !replaced || !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-Match racing a foreign write: replaced %v, err %v", replaced, err)
	}
	if _, data := h.read("bkt", "k", ""); string(data) != "sneaky" {
		t.Errorf("content %q, want the foreign write", data)
	}
}

func TestReaderFailsOnReplacedObject(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	v1 := randData(100<<10, 1)
	h.mustPut("bkt", "k", v1, storage.ObjectMeta{ETag: md5hex(v1)})
	_, r, err := h.OpenObject(t.Context(), "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h.mustPut("bkt", "k", []byte("v2"), storage.ObjectMeta{})
	if _, err := r.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("read after replacement: %v, want an error wrapping ErrPreconditionFailed", err)
	}
}

func TestInvalidVersionsAndKeys(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	h.mustPut("bkt", "k", []byte("x"), storage.ObjectMeta{})
	if _, err := h.GetObjectInfo(t.Context(), "bkt", "k", "v123"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Errorf("unknown version: %v", err)
	}
	if res, err := h.DeleteObject(t.Context(), "bkt", "k", storage.NullVersionID); err != nil || res.VersionID != storage.NullVersionID {
		t.Errorf("delete null: %+v %v", res, err)
	}
	if _, err := h.DeleteObject(t.Context(), "bkt", "k", storage.NullVersionID); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Errorf("delete missing null: %v", err)
	}
	long := strings.Repeat("k", 1025)
	if _, err := h.NewObjectWriter(t.Context(), "bkt", long); !errors.Is(err, storage.ErrInvalidName) {
		t.Errorf("too long key: %v", err)
	}
	if _, err := h.NewObjectWriter(t.Context(), "bkt", long[1:]); err != nil {
		t.Errorf("longest key: %v", err)
	}
	if _, err := h.NewObjectWriter(t.Context(), "bkt", "nul\x00"); !errors.Is(err, storage.ErrInvalidName) {
		t.Errorf("key with a NUL byte: %v", err)
	}
	if _, err := h.GetObjectInfo(t.Context(), "bkt", "nul\x00", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("HEAD of a key with a NUL byte: %v", err)
	}
}

// TestConditionalRaces covers the unversioned halves of the conformance
// races the suite only runs on versioned buckets.
func TestConditionalRaces(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	base := h.mustPut("bkt", "k", []byte("base"), storage.ObjectMeta{ETag: "gos3-etag"})
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
		w, err := h.NewObjectWriter(t.Context(), "bkt", "k")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "racer %d", i)
		writers[i] = w
	}
	winners := race(len(writers), func(i int) error {
		_, err := writers[i].Commit(t.Context(), storage.ObjectMeta{ETag: md5hex(fmt.Appendf(nil, "racer %d", i))}, storage.CommitOptions{IfMatch: base.ETag})
		return err
	})
	if len(winners) != 1 {
		t.Fatalf("If-Match winners %v, want exactly one", winners)
	}
	if _, data := h.read("bkt", "k", ""); string(data) != fmt.Sprintf("racer %d", winners[0]) {
		t.Errorf("content %q, winner %d", data, winners[0])
	}
}

// TestSetClient: operations after SetClient use the new client and part
// size.
func TestSetClient(t *testing.T) {
	log := &reqLog{}
	h := newHarness(t, s3clienttest.Options{Wrap: log.wrap}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("x"), storage.ObjectMeta{})
	credential := func() string {
		reqs := log.all()
		auth := reqs[len(reqs)-1].header.Get("Authorization")
		_, cred, _ := strings.Cut(auth, "Credential=")
		key, _, _ := strings.Cut(cred, "/")
		return key
	}

	cfg := h.remote.Config()
	cfg.AccessKey, cfg.SecretKey = "OTHERACCESSKEY000001", "other-secret-key-00000000000000000000001"
	other, err := s3client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.st.SetClient(other, 0)
	if _, err := h.GetObjectInfo(ctx, "b", "k", ""); err == nil {
		t.Error("GetObjectInfo with unknown credentials succeeded")
	}
	if got := credential(); got != cfg.AccessKey {
		t.Errorf("request signed by %q, want the new client's %q", got, cfg.AccessKey)
	}

	h.st.SetClient(h.remote.Client(t), 5<<20)
	data := randData(6<<20, 3)
	if _, err := h.put("b", "big", data, storage.ObjectMeta{ETag: md5hex(data)}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := credential(); got != s3clienttest.AccessKey {
		t.Errorf("request signed by %q, want %q", got, s3clienttest.AccessKey)
	}
	if a := h.remoteHead("b", "big", ""); !strings.Contains(a.ETag, "-") {
		t.Errorf("remote ETag %q, want a multipart upload with the new part size", a.ETag)
	}
	if info := h.info("b", "big", ""); info.ETag != md5hex(data) {
		t.Errorf("ETag %q after SetClient", info.ETag)
	}
}

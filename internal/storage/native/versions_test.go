package native_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// TestAdoptVersionedAlias adopts a versioned remote bucket, populated by
// another client, under a different gos3 name: gos3 shows its exact
// history (remote version IDs and delete markers), reads every version,
// and sees later writes of the other client.
func TestAdoptVersionedAlias(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.remote.CreateBucket(t, "foreign-data")
	if err := h.c.PutBucketVersioning(ctx, "foreign-data", "Enabled"); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{}
	put := func(key, data string) string {
		res := h.remotePut("foreign-data", key, []byte(data), http.Header{"Content-Type": {"text/x-foreign"}, "X-Amz-Meta-Origin": {"other-tool"}})
		contents[res.VersionID] = []byte(data)
		return res.VersionID
	}
	put("a.txt", "a1")
	put("a.txt", "a2")
	put("dir/b.txt", "b1")
	if _, err := h.c.DeleteObjectWith(ctx, "foreign-data", "dir/b.txt", s3client.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	put("z", "z1")

	st, err := h.adopt(ctx, "alias", "foreign-data", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Versioning != storage.VersioningEnabled || st.Owned || st.Remote != "foreign-data" || st.Region == "" {
		t.Errorf("remote state %+v, want an adopted versioned bucket with its region", st)
	}
	want := vrefs(h.remoteVersions("foreign-data"))
	if got := vrefs(h.versions("alias", "", "", "")); !slices.Equal(got, want) {
		t.Errorf("history through gos3:\n got  %v\n want %v (the remote's)", got, want)
	}
	for _, v := range h.versions("alias", "", "", "") {
		info, data := h.read("alias", v.Key, v.VersionID)
		if v.DeleteMarker {
			if !info.DeleteMarker || info.LastModified.IsZero() {
				t.Errorf("%s@%s: %+v, want a delete marker", v.Key, v.VersionID, info)
			}
			continue
		}
		if !bytes.Equal(data, contents[v.VersionID]) || info.ContentType != "text/x-foreign" || info.UserMeta["origin"] != "other-tool" ||
			info.ETag != md5hex(data) || info.IsLatest != v.IsLatest {
			t.Errorf("%s@%s: %+v %q", v.Key, v.VersionID, info, data)
		}
	}
	if got := h.walkKeys("alias", "", ""); !slices.Equal(got, []string{"a.txt", "z"}) {
		t.Errorf("WalkObjects = %q", got)
	}
	// Writes of the other client after the adoption are visible.
	vid := put("a.txt", "a3")
	if info := h.info("alias", "a.txt", ""); info.VersionID != vid || !info.IsLatest {
		t.Errorf("latest after a foreign write: %+v, want %s", info, vid)
	}
	// gos3's writes are visible to the other client, with the remote's IDs.
	mine := h.mustPut("alias", "a.txt", []byte("gos3"), storage.ObjectMeta{ETag: md5hex([]byte("gos3"))})
	a, err := h.c.HeadObject(ctx, "foreign-data", "a.txt")
	if err != nil || a.VersionID != mine.VersionID {
		t.Errorf("remote HEAD %+v %v, want gos3's version %s", a, err, mine.VersionID)
	}
	// Deleting the gos3 bucket never deletes an adopted remote bucket.
	h.deleteAll("alias")
	if err := h.DeleteBucket(ctx, "alias"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.HeadBucket(ctx, "foreign-data"); err != nil {
		t.Errorf("adopted remote bucket after DeleteBucket: %v", err)
	}
}

// deleteAll deletes every version of bucket.
func (h *harness) deleteAll(bucket string) {
	h.t.Helper()
	for _, v := range h.versions(bucket, "", "", "") {
		if _, err := h.DeleteObject(h.t.Context(), bucket, v.Key, v.VersionID); err != nil {
			h.t.Fatal(err)
		}
	}
}

// stripHeaders removes response headers from answers about delete
// markers (a service that does not send them).
func stripHeaders(names ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&stripWriter{ResponseWriter: w, names: names}, r)
		})
	}
}

type stripWriter struct {
	http.ResponseWriter
	names []string
	done  bool
}

func (w *stripWriter) strip() {
	if !w.done && w.Header().Get("X-Amz-Delete-Marker") != "" {
		for _, n := range w.names {
			w.Header().Del(n)
		}
	}
	w.done = true
}

func (w *stripWriter) WriteHeader(code int) {
	w.strip()
	w.ResponseWriter.WriteHeader(code)
}

func (w *stripWriter) Write(p []byte) (int, error) {
	w.strip()
	return w.ResponseWriter.Write(p)
}

func TestDeleteMarkerPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{"headers", nil},
		{"no-marker-header", stripHeaders("X-Amz-Delete-Marker")},
		{"no-marker-headers", stripHeaders("X-Amz-Delete-Marker", "X-Amz-Version-Id")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, s3clienttest.Options{Wrap: tc.wrap}, nil)
			ctx := t.Context()
			h.mkVersioned("b")
			v1 := h.mustPut("b", "k", []byte("one"), storage.ObjectMeta{})
			// The marker is created by another client: gos3 has to find it.
			if _, err := h.remote.Backend.DeleteObject(ctx, "nb-b", "k", ""); err != nil {
				t.Fatal(err)
			}
			hist := h.remoteVersions("nb-b")
			if len(hist) != 2 || !hist[0].DeleteMarker {
				t.Fatalf("remote history %v", vrefs(hist))
			}
			mvid := hist[0].VersionID
			start := time.Now()
			for _, vid := range []string{"", mvid} {
				info, err := h.GetObjectInfo(ctx, "b", "k", vid)
				if err != nil || !info.DeleteMarker || info.VersionID != mvid || !info.IsLatest || info.LastModified.Before(start.Add(-2*time.Second)) {
					t.Errorf("GetObjectInfo(@%q) = %+v, %v; want the latest delete marker %s", vid, info, err, mvid)
				}
				info, r, err := h.OpenObject(ctx, "b", "k", vid)
				if err != nil || r != nil || !info.DeleteMarker || info.VersionID != mvid {
					t.Errorf("OpenObject(@%q) = %+v, %v, %v; want the delete marker", vid, info, r, err)
				}
			}
			if info := h.info("b", "k", v1.VersionID); info.IsLatest || info.DeleteMarker {
				t.Errorf("version behind the marker: %+v", info)
			}
			// gos3's own delete reports its marker.
			res, err := h.DeleteObject(ctx, "b", "k", "")
			if err != nil || !res.DeleteMarker || res.VersionID == "" || res.VersionID == mvid {
				t.Errorf("DeleteObject = %+v, %v; want a new marker", res, err)
			}
			// Deleting a marker by ID.
			res, err = h.DeleteObject(ctx, "b", "k", mvid)
			if err != nil || !res.DeleteMarker || res.VersionID != mvid {
				t.Errorf("DeleteObject(marker) = %+v, %v", res, err)
			}
			if _, err := h.GetObjectInfo(ctx, "b", "k", mvid); !errors.Is(err, storage.ErrVersionNotFound) {
				t.Errorf("deleted marker: %v", err)
			}
			if _, err := h.GetObjectInfo(ctx, "b", "missing", ""); !errors.Is(err, storage.ErrObjectNotFound) {
				t.Errorf("missing key: %v", err)
			}
		})
	}
}

// awsVersionMarkers makes the remote treat a version-ID marker naming a
// version that no longer exists like AWS may: with 400 InvalidArgument
// (reject) or by skipping the rest of the key marker's versions.
func awsVersionMarkers(remote *atomic.Pointer[s3clienttest.Remote], reject bool) func(http.Handler) http.Handler {
	entry := regexp.MustCompile(`(?s)<(Version|DeleteMarker)>.*?</(?:Version|DeleteMarker)>`)
	keyRe := regexp.MustCompile(`<Key>([^<]*)</Key>`)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			km, vm := q.Get("key-marker"), q.Get("version-id-marker")
			if r.Method != http.MethodGet || !q.Has("versions") || vm == "" {
				next.ServeHTTP(w, r)
				return
			}
			bucket := strings.Trim(r.URL.Path, "/")
			_, err := remote.Load().Backend.GetObjectInfo(r.Context(), bucket, km, vm)
			if !errors.Is(err, storage.ErrVersionNotFound) && !errors.Is(err, storage.ErrObjectNotFound) {
				next.ServeHTTP(w, r)
				return
			}
			if reject {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<Error><Code>InvalidArgument</Code><Message>Invalid version id specified</Message></Error>`)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := entry.ReplaceAllStringFunc(rec.Body.String(), func(e string) string {
				if m := keyRe.FindStringSubmatch(e); m != nil {
					if k, err := url.QueryUnescape(m[1]); err == nil && k == km {
						return ""
					}
				}
				return e
			})
			for k, vs := range rec.Header() {
				w.Header()[k] = vs
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(rec.Code)
			_, _ = io.WriteString(w, body)
		})
	}
}

func TestVersionMarkerFallback(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%v", reject), func(t *testing.T) {
			var remote atomic.Pointer[s3clienttest.Remote]
			h := newHarness(t, s3clienttest.Options{Wrap: awsVersionMarkers(&remote, reject)}, nil)
			remote.Store(h.remote)
			ctx := t.Context()
			h.mkVersioned("b")
			var k []storage.ObjectInfo
			for i := range 3 {
				k = append(k, h.mustPut("b", "k", fmt.Appendf(nil, "v%d", i), storage.ObjectMeta{}))
			}
			l := h.mustPut("b", "l", []byte("l"), storage.ObjectMeta{})
			// The marker version (the middle one) is deleted between pages.
			if _, err := h.DeleteObject(ctx, "b", "k", k[1].VersionID); err != nil {
				t.Fatal(err)
			}
			got := h.versions("b", "", "k", k[1].VersionID)
			ids := vrefs(got)
			if !slices.Contains(ids, "k@"+k[0].VersionID) || !slices.Contains(ids, "l@"+l.VersionID+"*") {
				t.Errorf("walk from a deleted marker = %v: skipped versions (k@%s, l@%s)", ids, k[0].VersionID, l.VersionID)
			}
			for i, v := range got {
				if v.IsLatest && i > 0 && got[i-1].Key == v.Key {
					t.Errorf("IsLatest on a later entry of %s: %v", v.Key, ids)
				}
			}
			// A marker that still exists resumes right after it.
			if got := vrefs(h.versions("b", "", "k", k[2].VersionID)); !slices.Equal(got, []string{"k@" + k[0].VersionID, "l@" + l.VersionID + "*"}) {
				t.Errorf("walk from an existing marker = %v", got)
			}
			// The emptying loop removes everything, page by page.
			for _, n := range []int{1, 2} {
				for i := range 3 {
					h.mustPut("b", fmt.Sprintf("e%d", i%2), []byte("x"), storage.ObjectMeta{})
				}
				var p storage.VersionListParams
				p.MaxKeys = n
				for pages := 0; ; pages++ {
					if pages > 20 {
						t.Fatalf("emptying loop does not terminate")
					}
					res, err := storage.ListObjectVersions(ctx, h, "b", p)
					if err != nil {
						t.Fatal(err)
					}
					for _, v := range res.Versions {
						if _, err := h.DeleteObject(ctx, "b", v.Key, v.VersionID); err != nil {
							t.Fatal(err)
						}
					}
					if !res.IsTruncated {
						break
					}
					p.KeyMarker, p.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
				}
				if left := h.versions("b", "", "", ""); len(left) != 0 {
					t.Errorf("MaxKeys %d: emptying page by page left %v", n, vrefs(left))
				}
			}
		})
	}
}

// TestWalkVersionsPages walks histories spanning many listing pages.
func TestWalkVersionsPages(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mkVersioned("b")
	native.SetListPage(h.st, 2)
	for _, k := range []string{"a", "a", "a", "b", "c", "c"} {
		h.mustPut("b", k, []byte(k), storage.ObjectMeta{})
	}
	if _, err := h.DeleteObject(ctx, "b", "b", ""); err != nil {
		t.Fatal(err)
	}
	h.mustPut("b", "d", []byte("d"), storage.ObjectMeta{})
	all := h.versions("b", "", "", "")
	if got, want := vrefs(all), vrefs(h.remoteVersions("nb-b")); !slices.Equal(got, want) {
		t.Fatalf("WalkVersions = %v, want %v", got, want)
	}
	for i, v := range all {
		if got := vrefs(h.versions("b", "", v.Key, v.VersionID)); !slices.Equal(got, vrefs(all[i+1:])) {
			t.Errorf("WalkVersions after %s@%s = %v, want %v", v.Key, v.VersionID, got, vrefs(all[i+1:]))
		}
	}
	if got := h.walkKeys("b", "", ""); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Errorf("WalkObjects = %q", got)
	}
	// Removing a delete marker while the walk delivers it: no later entry
	// of the key claims to be the latest.
	var got []storage.ObjectInfo
	err := h.WalkVersions(ctx, "b", "", "", "", func(o storage.ObjectInfo) error {
		got = append(got, o)
		if o.Key == "b" && o.DeleteMarker {
			_, err := h.DeleteObject(ctx, "b", "b", o.VersionID)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range got {
		if o.IsLatest && i > 0 && got[i-1].Key == o.Key {
			t.Errorf("IsLatest on a later entry of %s: %v", o.Key, vrefs(got))
		}
	}
}

func TestBypassGovernanceAndCompliance(t *testing.T) {
	log := &reqLog{}
	h := newHarness(t, s3clienttest.Options{Wrap: log.wrap}, nil)
	ctx := t.Context()
	h.mk("lk", storage.CreateBucketOptions{ObjectLockEnabled: true})
	until := time.Now().Add(time.Hour)
	gov := h.mustPut("lk", "gov", []byte("g"), storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: until})
	bypassSent := func() (n int) {
		for _, r := range log.all() {
			if r.header.Get("X-Amz-Bypass-Governance-Retention") != "" {
				n++
			}
		}
		return n
	}
	log.reset()
	if _, err := h.DeleteObject(ctx, "lk", "gov", gov.VersionID); !errors.Is(err, storage.ErrRemoteDenied) {
		t.Errorf("delete of a GOVERNANCE version without bypass: %v, want ErrRemoteDenied", err)
	}
	if n := bypassSent(); n != 0 {
		t.Errorf("bypass header sent %d times without the context flag", n)
	}
	// Shortening needs the bypass too.
	_, err := h.UpdateObjectMeta(ctx, "lk", "gov", gov.VersionID, func(m *storage.ObjectMeta) error {
		m.RetainUntil = m.RetainUntil.Add(-time.Minute)
		return nil
	})
	if !errors.Is(err, storage.ErrRemoteDenied) {
		t.Errorf("shortening GOVERNANCE retention without bypass: %v", err)
	}
	bctx := storage.WithGovernanceBypass(ctx)
	shorter := until.Add(-time.Minute).Add(123 * time.Nanosecond)
	info, err := h.UpdateObjectMeta(bctx, "lk", "gov", gov.VersionID, func(m *storage.ObjectMeta) error {
		m.RetainUntil = shorter
		return nil
	})
	if err != nil || !info.RetainUntil.Equal(shorter) {
		t.Fatalf("shortening with bypass: %+v, %v", info.RetainUntil, err)
	}
	if got := h.info("lk", "gov", gov.VersionID); !got.RetainUntil.Equal(shorter) || got.RetentionMode != "GOVERNANCE" {
		t.Errorf("retention after the update: %s %v, want %v (exact)", got.RetentionMode, got.RetainUntil, shorter)
	}
	if res, err := h.DeleteObject(bctx, "lk", "gov", gov.VersionID); err != nil || res.VersionID != gov.VersionID {
		t.Errorf("delete with bypass: %+v, %v", res, err)
	}
	if n := bypassSent(); n != 2 {
		t.Errorf("bypass header sent %d times, want 2 (retention, delete)", n)
	}

	// COMPLIANCE retention (only against an in-memory remote: it can never
	// be removed on a real service) cannot be bypassed.
	comp := h.mustPut("lk", "comp", []byte("c"), storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: until})
	if _, err := h.DeleteObject(bctx, "lk", "comp", comp.VersionID); !errors.Is(err, storage.ErrRemoteDenied) {
		t.Errorf("delete of a COMPLIANCE version with bypass: %v", err)
	}
	// A legal hold, set and removed natively.
	if _, err := h.UpdateObjectMeta(ctx, "lk", "comp", "", func(m *storage.ObjectMeta) error { m.LegalHold = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if a := h.remoteHead("lk", "comp", ""); a.Header.Get("X-Amz-Object-Lock-Legal-Hold") != "ON" {
		t.Errorf("legal hold not native: %v", a.Header)
	}

	// ForbidCompliance refuses COMPLIANCE on lock-enabled buckets.
	g := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.ForbidCompliance = true })
	g.mk("lk", storage.CreateBucketOptions{ObjectLockEnabled: true})
	if _, err := g.put("lk", "k", []byte("x"), storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: until}, storage.CommitOptions{}); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("COMPLIANCE with ForbidCompliance: %v", err)
	}
	cfg := []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	if err := g.st.PutObjectLockConfig(ctx, "lk", cfg); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("COMPLIANCE default with ForbidCompliance: %v", err)
	}
	g.mustPut("lk", "gov", []byte("x"), storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: until})
}

func TestContentMD5(t *testing.T) {
	log := &reqLog{}
	h := newHarness(t, s3clienttest.Options{Wrap: log.wrap}, func(o *native.Options) { o.PartSize = 5 << 20 })
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "small", []byte("x"), storage.ObjectMeta{})
	h.mustPut("b", "big", randData(6<<20, 1), storage.ObjectMeta{})
	u, err := h.CreateMultipartUpload(t.Context(), "b", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "b", "mp", u.UploadID, 1, randData(5<<20, 2))
	uploadPart(t, h, "b", "mp", u.UploadID, 2, []byte("tail"))
	if _, err := h.CompleteMultipartUpload(t.Context(), "b", "mp", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range log.all() {
		if r.method != http.MethodPut || r.query.Has("uploadId") == false && len(r.query) > 0 || strings.Count(r.path, "/") < 2 {
			continue // not an object or part upload
		}
		n++
		if r.header.Get("Content-Md5") == "" {
			t.Errorf("%s %s?%s without Content-MD5", r.method, r.path, r.query.Encode())
		}
	}
	if n < 5 {
		t.Errorf("%d uploads seen, want at least 5", n)
	}
}

func TestStrictRegion(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{StrictRegion: true}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	h.remotePut("nb-b", "k", []byte("data"), nil)
	// Adoption discovers the region.
	h.remote.CreateBucket(t, "other")
	st, err := h.adopt(ctx, "alias", "other", "")
	if err != nil || st.Region != s3clienttest.Region {
		t.Fatalf("adopt: %+v, %v; want region %s", st, err, s3clienttest.Region)
	}
	// A stale recorded region: the store learns the right one and reports
	// the drift, whatever request comes first.
	for _, first := range []string{"head", "put", "list", "get"} {
		h.mu.Lock()
		h.buckets["b"].bind.Region = "eu-west-1"
		h.drifts = nil
		h.mu.Unlock()
		h.open()
		var err error
		switch first {
		case "head":
			_, err = h.GetObjectInfo(ctx, "b", "k", "")
		case "put":
			_, err = h.put("b", "k2", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{})
		case "list":
			err = h.WalkObjects(ctx, "b", "", "", func(storage.ObjectInfo) error { return nil })
		case "get":
			_, _ = h.read("b", "k", "")
		}
		if err != nil {
			t.Errorf("%s with a stale region: %v", first, err)
		}
		if d := h.driftLog(); len(d) == 0 || !strings.Contains(d[0], s3clienttest.Region) {
			t.Errorf("%s: drift reports %q, want the region", first, d)
		}
		if got := h.walkKeys("b", "", ""); !slices.Contains(got, "k") {
			t.Errorf("%s: keys %q", first, got)
		}
		rs, err := h.st.Refresh(ctx, "b")
		if err != nil || rs.Region != s3clienttest.Region {
			t.Errorf("%s: Refresh = %+v, %v", first, rs, err)
		}
	}
}

func TestMetadataUpdatesNoRemoteVersions(t *testing.T) {
	log := &reqLog{}
	h := newHarness(t, s3clienttest.Options{Wrap: log.wrap}, nil)
	ctx := t.Context()
	h.mkVersioned("ver")
	h.mk("lk", storage.CreateBucketOptions{ObjectLockEnabled: true})
	for _, bucket := range []string{"ver", "lk"} {
		committed := h.mustPut(bucket, "k", []byte("data"), storage.ObjectMeta{ETag: md5hex([]byte("data")), ContentType: "text/plain"})
		time.Sleep(1100 * time.Millisecond) // a rewrite would move the remote's LastModified
		log.reset()
		until := time.Now().Add(time.Hour)
		for _, fn := range []func(*storage.ObjectMeta){
			func(m *storage.ObjectMeta) { m.Tags = map[string]string{"x": "y"} },
			func(m *storage.ObjectMeta) { m.ACL = "public-read" },
			func(m *storage.ObjectMeta) { m.LegalHold = true },
			func(m *storage.ObjectMeta) { m.RetentionMode, m.RetainUntil = "GOVERNANCE", until },
			func(m *storage.ObjectMeta) { m.RetainUntil = until.Add(time.Hour) },
			func(m *storage.ObjectMeta) { m.LegalHold = false; m.Tags = nil; m.CacheControl = "no-cache" },
		} {
			if _, err := h.UpdateObjectMeta(ctx, bucket, "k", committed.VersionID, func(m *storage.ObjectMeta) error { fn(m); return nil }); err != nil {
				t.Fatalf("%s: UpdateObjectMeta: %v", bucket, err)
			}
		}
		for _, r := range log.all() {
			if r.header.Get("X-Amz-Copy-Source") != "" || r.method == http.MethodPut && len(r.query) == 0 {
				t.Errorf("%s: metadata update rewrote the object: %s %s", bucket, r.method, r.path)
			}
		}
		if vs := h.remoteVersions(remoteName(bucket)); len(vs) != 1 {
			t.Errorf("%s: remote versions %v, want one", bucket, vrefs(vs))
		}
		info := h.info(bucket, "k", "")
		if !info.LastModified.Equal(committed.LastModified) || info.ETag != committed.ETag || info.ACL != "public-read" ||
			info.CacheControl != "no-cache" || len(info.Tags) != 0 || info.LegalHold || info.RetentionMode != "GOVERNANCE" ||
			!info.RetainUntil.Equal(until.Add(time.Hour)) || info.ContentType != "text/plain" {
			t.Errorf("%s: after the updates %+v", bucket, info)
		}
		if bucket == "lk" {
			if a := h.remoteHead(bucket, "k", ""); a.Header.Get("X-Amz-Object-Lock-Mode") != "GOVERNANCE" {
				t.Errorf("retention not native on a lock bucket: %v", a.Header)
			}
			if _, err := h.DeleteObject(storage.WithGovernanceBypass(ctx), bucket, "k", committed.VersionID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestOverlayNotInheritedAfterForeignOverwrite: metadata gos3 keeps in an
// overlay (a public-read ACL) belongs to the remote object it was set on;
// an object another client writes in its place does not inherit it.
func TestOverlayNotInheritedAfterForeignOverwrite(t *testing.T) {
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningSuspended} {
		t.Run(string(status)+"null", func(t *testing.T) {
			h := newHarness(t, s3clienttest.Options{}, nil)
			ctx := t.Context()
			h.mk("b", storage.CreateBucketOptions{})
			if status != storage.VersioningUnversioned {
				if err := h.SetBucketVersioning(ctx, "b", status); err != nil {
					t.Fatal(err)
				}
			}
			h.mustPut("b", "k", []byte("private data"), storage.ObjectMeta{})
			if _, err := h.UpdateObjectMeta(ctx, "b", "k", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); err != nil {
				t.Fatal(err)
			}
			// The overlay survives a restart.
			h.open()
			listedACL := func() string {
				var acl string
				if err := h.WalkObjects(ctx, "b", "", "", func(o storage.ObjectInfo) error { acl = o.ACL; return nil }); err != nil {
					t.Fatal(err)
				}
				return acl
			}
			if acl := h.info("b", "k", "").ACL; acl != "public-read" {
				t.Fatalf("ACL %q after the update", acl)
			}
			if acl := listedACL(); acl != "public-read" {
				t.Errorf("listed ACL %q after the update", acl)
			}
			// Another client replaces the object (same key, null version).
			h.remotePut("nb-b", "k", []byte("someone else's secret"), nil)
			if info := h.info("b", "k", storage.NullVersionID); info.ACL != "" {
				t.Errorf("foreign object inherited the ACL %q", info.ACL)
			}
			if acl := listedACL(); acl != "" {
				t.Errorf("foreign object listed with the ACL %q", acl)
			}
			// gos3's own overwrite with identical content in the same
			// second does not inherit it either.
			h.mustPut("b", "j", []byte("same"), storage.ObjectMeta{})
			if _, err := h.UpdateObjectMeta(ctx, "b", "j", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); err != nil {
				t.Fatal(err)
			}
			h.mustPut("b", "j", []byte("same"), storage.ObjectMeta{})
			if acl := h.info("b", "j", "").ACL; acl != "" {
				t.Errorf("rewritten object inherited the ACL %q", acl)
			}
		})
	}
}

func TestDriftOnRemoteVersioning(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	if err := h.c.PutBucketVersioning(t.Context(), "nb-b", "Enabled"); err != nil {
		t.Fatal(err)
	}
	info := h.mustPut("b", "k", []byte("x"), storage.ObjectMeta{})
	if d := h.driftLog(); len(d) == 0 || !strings.Contains(d[0], "versioned") {
		t.Errorf("drift reports %q after a write to a remote versioned behind gos3's back", d)
	}
	if info.VersionID == storage.NullVersionID {
		t.Errorf("VersionID %q, want the remote's", info.VersionID)
	}
	rs, err := h.st.Refresh(t.Context(), "b")
	if err != nil || rs.Versioning != storage.VersioningEnabled {
		t.Errorf("Refresh = %+v, %v", rs, err)
	}

	// A bucket recorded with object lock whose remote has none: the write
	// with retention fails (never silently without it) and is reported.
	h.mk("nolock", storage.CreateBucketOptions{})
	h.mu.Lock()
	h.buckets["nolock"].info.ObjectLockEnabled = true
	h.drifts = nil
	h.mu.Unlock()
	_, err = h.put("nolock", "k", []byte("x"), storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour)}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrRemoteRejected) {
		t.Errorf("write with refused lock headers: %v, want ErrRemoteRejected", err)
	}
	if d := h.driftLog(); len(d) != 1 || !strings.Contains(d[0], "object lock") {
		t.Errorf("drift reports %q, want the refused object lock settings", d)
	}
}

func TestErrorClasses(t *testing.T) {
	notImpl := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("versioning") {
				w.WriteHeader(http.StatusNotImplemented)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := newHarness(t, s3clienttest.Options{Wrap: notImpl}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("x"), storage.ObjectMeta{})

	// 503: the backing is unavailable, not just the bucket.
	fail := func(*http.Request) int { return http.StatusServiceUnavailable }
	h.remote.Fail.Store(&fail)
	_, err := h.GetObjectInfo(ctx, "b", "k", "")
	if !errors.Is(err, storage.ErrBackingUnavailable) || errors.Is(err, storage.ErrBucketUnavailable) {
		t.Errorf("GetObjectInfo with a failing remote: %v", err)
	}
	// A 403 on the bucket makes the bucket unavailable; on an object it
	// is that object's.
	forbidden := func(r *http.Request) int {
		if r.Method == http.MethodGet && strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 0 {
			return http.StatusForbidden
		}
		return 0
	}
	h.remote.Fail.Store(&forbidden)
	if err := h.WalkObjects(ctx, "b", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Errorf("walk with a forbidden bucket: %v", err)
	}
	h.remote.Fail.Store(nil)

	// 501 (no versioning support).
	if err := h.SetBucketVersioning(ctx, "b", storage.VersioningEnabled); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("SetVersioning on a remote without versioning: %v", err)
	}
	rs, err := h.st.Refresh(ctx, "b")
	if err != nil || rs.Caps.Versioning || !rs.Caps.ObjectLock {
		t.Errorf("Refresh = %+v, %v; want no versioning capability", rs, err)
	}

	// A remote bucket deleted behind gos3's back: the bucket (only) is
	// unavailable, and empty for deletion.
	if _, err := h.DeleteObject(ctx, "b", "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.remote.Backend.DeleteBucket(ctx, "nb-b"); err != nil {
		t.Fatal(err)
	}
	if err := h.WalkObjects(ctx, "b", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Errorf("walk of a vanished remote bucket: %v", err)
	}
	if _, err := h.put("b", "k", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Errorf("put into a vanished remote bucket: %v", err)
	}
	if _, _, err := h.OpenObject(ctx, "b", "k", ""); err == nil {
		t.Error("GET in a vanished remote bucket succeeded")
	}
	if err := h.st.CheckEmpty(ctx, "b"); err != nil {
		t.Errorf("CheckEmpty of a vanished remote bucket: %v", err)
	}
	// Unknown buckets are the resolver's answer.
	if _, err := h.GetObjectInfo(ctx, "nope", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("unknown bucket: %v", err)
	}
}

// TestAdoptWithObjectScopedCredentials adopts a remote bucket with
// credentials that may use its objects but not read its configuration (a
// bucket-scoped Cloudflare R2 token): versioning and object lock are left
// to the remote. Credentials denied the bucket itself are refused as such
// (not as an unavailable backing).
func TestAdoptWithObjectScopedCredentials(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.remote.CreateBucket(t, "scoped")
	configOnly := func(r *http.Request) int {
		if q := r.URL.Query(); q.Has("versioning") || q.Has("object-lock") {
			return http.StatusForbidden
		}
		return 0
	}
	h.remote.Fail.Store(&configOnly)
	st, err := h.adopt(ctx, "sc", "scoped", "")
	if err != nil {
		t.Fatalf("adopt with object-scoped credentials: %v", err)
	}
	if st.Caps.Versioning || st.Caps.ObjectLock || st.Versioning != storage.VersioningUnversioned || st.ObjectLock {
		t.Errorf("adopted state %+v, want no versioning or object lock capability", st)
	}
	h.mustPut("sc", "k", []byte("x"), storage.ObjectMeta{})
	if rs, err := h.st.Refresh(ctx, "sc"); err != nil || rs.Caps.Versioning || rs.Caps.ObjectLock {
		t.Errorf("Refresh = %+v, %v", rs, err)
	}

	// Denied the bucket as a whole.
	h.remote.CreateBucket(t, "denied")
	bucketDenied := func(r *http.Request) int {
		if strings.HasPrefix(strings.Trim(r.URL.Path, "/"), "denied") {
			return http.StatusForbidden
		}
		return 0
	}
	h.remote.Fail.Store(&bucketDenied)
	if _, err := h.adopt(ctx, "dn", "denied", "us-east-1"); !errors.Is(err, storage.ErrRemoteDenied) || errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("adopt a denied remote bucket: %v, want ErrRemoteDenied", err)
	}
	// A bound bucket denied later is unavailable.
	h.remote.Fail.Store(&configOnly)
	if _, err := h.adopt(ctx, "dn", "denied", ""); err != nil {
		t.Fatal(err)
	}
	h.remote.Fail.Store(&bucketDenied)
	if _, err := h.st.Refresh(ctx, "dn"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Errorf("Refresh of a denied bound bucket: %v, want ErrBucketUnavailable", err)
	}
}

func TestAttachBucket(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.remote.CreateBucket(t, "existing")
	_, err := h.st.AttachBucket(ctx, native.AttachRequest{Bucket: "x", Remote: "existing", Intent: native.Create})
	if !errors.Is(err, storage.ErrRemoteBucketExists) || !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("create over an existing remote bucket: %v", err)
	}
	if _, err := h.adopt(ctx, "x", "missing-remote", ""); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Errorf("adopt a missing remote bucket: %v", err)
	}
	h.remote.CreateBucket(t, "store")
	h.remotePut("store", "gos3.json", []byte("{}"), nil)
	if _, err := h.adopt(ctx, "x", "store", ""); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Errorf("adopt a gos3 store: %v", err)
	}
	if _, err := h.adopt(ctx, "x", "existing", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adopt(ctx, "y", "existing", ""); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Errorf("adopt a remote bucket bound to another bucket: %v", err)
	}

	// A remote bucket with object lock and default retention.
	if err := h.c.CreateBucketWith(ctx, "locked", s3client.CreateBucketOptions{ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.PutObjectLockConfiguration(ctx, "locked", s3client.ObjectLockConfig{Enabled: true, Mode: "GOVERNANCE", Days: 3}); err != nil {
		t.Fatal(err)
	}
	st, err := h.adopt(ctx, "lk", "locked", "")
	if err != nil {
		t.Fatal(err)
	}
	if !st.ObjectLock || st.Versioning != storage.VersioningEnabled || !strings.Contains(string(st.LockConfig), "<Days>3</Days>") ||
		!strings.Contains(string(st.LockConfig), "<Mode>GOVERNANCE</Mode>") {
		t.Errorf("adopted lock bucket state %+v %s", st, st.LockConfig)
	}
	// The default retention is mirrored to the remote.
	cfg := []byte(`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`)
	if err := h.st.PutObjectLockConfig(ctx, "lk", cfg); err != nil {
		t.Fatal(err)
	}
	if got, _, err := h.c.GetObjectLockConfiguration(ctx, "locked"); err != nil || got.Years != 1 || got.Days != 0 {
		t.Errorf("remote configuration %+v, %v", got, err)
	}

	// Created buckets are owned: deleting the gos3 bucket deletes them.
	h.mk("mine", storage.CreateBucketOptions{})
	if b := h.record("mine"); !b.bind.Owned {
		t.Errorf("created binding %+v, want owned", b.bind)
	}
	if err := h.DeleteBucket(ctx, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.HeadBucket(ctx, "nb-mine"); !s3client.IsNotFound(err) {
		t.Errorf("owned remote bucket after DeleteBucket: %v", err)
	}
	// Object lock enabled later on a versioned bucket.
	h.mkVersioned("later")
	if err := h.SetBucketObjectLock(ctx, "later", true); err != nil {
		t.Fatal(err)
	}
	if rs, err := h.st.Refresh(ctx, "later"); err != nil || !rs.ObjectLock {
		t.Errorf("Refresh after EnableObjectLock = %+v, %v", rs, err)
	}
}

func TestTaggingUnsupported(t *testing.T) {
	noTags := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("tagging") || r.Header.Get("X-Amz-Tagging") != "" {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = io.WriteString(w, `<Error><Code>NotImplemented</Code><Message>no tagging</Message></Error>`)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := newHarness(t, s3clienttest.Options{Wrap: noTags}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	tags := map[string]string{"a": "1"}
	h.mustPut("b", "k", []byte("x"), storage.ObjectMeta{Tags: tags})
	if got := h.info("b", "k", "").Tags; len(got) != 1 || got["a"] != "1" {
		t.Errorf("tags %v, want them kept by gos3", got)
	}
	if _, err := h.UpdateObjectMeta(ctx, "b", "k", "", func(m *storage.ObjectMeta) error { m.Tags = map[string]string{"b": "2"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := h.info("b", "k", "").Tags; len(got) != 1 || got["b"] != "2" {
		t.Errorf("tags after the update %v", got)
	}
	if rs, err := h.st.Refresh(ctx, "b"); err != nil || rs.Caps.Tagging {
		t.Errorf("Refresh = %+v, %v; want no tagging capability", rs, err)
	}
}

// TestDeleteVersionIfGoneVersionDoesNotDeadlock: DeleteVersionIf lists the
// key under its object lock; when the version was removed outside gos3 and
// the key has overlays, the listing prunes them without taking that lock
// again (H-08). Before, the call never returned and wedged the key.
func TestDeleteVersionIfGoneVersionDoesNotDeadlock(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mkVersioned("b")
	v1 := h.mustPut("b", "k", []byte("one"), storage.ObjectMeta{})
	h.mustPut("b", "k", []byte("two"), storage.ObjectMeta{})
	if _, err := h.UpdateObjectMeta(ctx, "b", "k", v1.VersionID, func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); err != nil {
		t.Fatal(err)
	}
	overlays := native.OverlayCount(h.st, "b", "k")
	time.Sleep(10 * time.Millisecond) // the overlay predates the listing
	// Another client (or the remote's lifecycle) removes the version.
	if _, err := h.c.DeleteObjectWith(ctx, remoteName("b"), "k", s3client.DeleteOptions{VersionID: v1.VersionID}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.DeleteVersionIf(ctx, "b", "k", v1.VersionID, 1, func(storage.ObjectInfo, []storage.ObjectInfo) error { return nil })
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, storage.ErrVersionNotFound) {
			t.Fatalf("DeleteVersionIf of a gone version = %v, want ErrVersionNotFound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteVersionIf deadlocked")
	}
	if n := native.OverlayCount(h.st, "b", "k"); n != overlays-1 {
		t.Errorf("%d overlays after pruning the gone version's, want %d", n, overlays-1)
	}
	// The key is not wedged.
	h.mustPut("b", "k", []byte("three"), storage.ObjectMeta{})
}

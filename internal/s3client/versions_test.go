package s3client_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// versionedSetup returns a remote with the bucket "bkt" with versioning
// enabled, and a client for it.
func versionedSetup(t *testing.T) *s3client.Client {
	t.Helper()
	_, c := setup(t)
	if err := c.PutBucketVersioning(t.Context(), "bkt", "Enabled"); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	return c
}

// putString writes data as bucket/key with Put.
func putString(t *testing.T, c *s3client.Client, bucket, key, data string) s3client.WriteResult {
	t.Helper()
	res, err := c.Put(t.Context(), bucket, key, strings.NewReader(data), int64(len(data)), s3client.PutOptions{})
	if err != nil {
		t.Fatalf("Put %s/%s: %v", bucket, key, err)
	}
	return res
}

// readVersion reads bucket/key at version vid ("" = current).
func readVersion(t *testing.T, c *s3client.Client, bucket, key, vid string) (string, s3client.ObjectAttrs) {
	t.Helper()
	rc, a, err := c.GetObject(t.Context(), bucket, key, s3client.GetOptions{VersionID: vid})
	if err != nil {
		t.Fatalf("GetObject %s/%s@%s: %v", bucket, key, vid, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), a
}

// entry is a ListObjectVersions entry reduced to what the tests compare.
type entry struct {
	Key, VersionID string
	Marker, Latest bool
}

func entriesOf(as []s3client.ObjectAttrs) []entry {
	out := make([]entry, len(as))
	for i, a := range as {
		out[i] = entry{a.Key, a.VersionID, a.DeleteMarker, a.IsLatest}
	}
	return out
}

func TestVersionedWrites(t *testing.T) {
	c := versionedSetup(t)
	ctx := t.Context()

	v1 := putString(t, c, "bkt", "k", "one")
	v2 := putString(t, c, "bkt", "k", "two")
	if v1.VersionID == "" || v2.VersionID == "" || v1.VersionID == v2.VersionID || v1.Multipart {
		t.Fatalf("Put results %+v, %+v: want two distinct version IDs", v1, v2)
	}

	// Reads by version ID.
	if got, a := readVersion(t, c, "bkt", "k", v1.VersionID); got != "one" || a.VersionID != v1.VersionID || a.ETag != v1.ETag {
		t.Errorf("GET v1 = %q %+v", got, a)
	}
	if got, a := readVersion(t, c, "bkt", "k", ""); got != "two" || a.VersionID != v2.VersionID {
		t.Errorf("GET current = %q, version %q", got, a.VersionID)
	}
	a, err := c.HeadObjectWith(ctx, "bkt", "k", s3client.HeadOptions{VersionID: v1.VersionID, IfMatch: v1.ETag})
	if err != nil || a.VersionID != v1.VersionID || a.Size != 3 || a.DeleteMarker {
		t.Fatalf("HEAD v1: %+v, %v", a, err)
	}
	if _, err := c.HeadObjectWith(ctx, "bkt", "k", s3client.HeadOptions{VersionID: v1.VersionID, IfMatch: v2.ETag}); !s3client.IsPreconditionFailed(err) {
		t.Errorf("HEAD v1 with v2's ETag: %v, want 412", err)
	}

	// A reader pinned to a version keeps reading it after an overwrite; one
	// pinned to "null" falls back to the ETag.
	r := c.NewVersionReader(ctx, "bkt", "k", 3, v1.VersionID, "ignored-etag")
	if got, err := io.ReadAll(r); err != nil || string(got) != "one" {
		t.Errorf("version reader: %q, %v", got, err)
	}
	r.Close()
	r = c.NewVersionReader(ctx, "bkt", "k", 3, "null", v1.ETag)
	if _, err := io.ReadAll(r); !s3client.IsPreconditionFailed(err) {
		t.Errorf("null-version reader pinned to an old ETag: %v, want 412", err)
	}
	r.Close()

	// Multipart writes report the version too.
	data := randBytes(6<<20 + 1)
	up, err := c.UploadObject(ctx, "bkt", "big", bytes.NewReader(data), int64(len(data)), s3client.UploadOptions{PartSize: s3client.MinPartSize})
	if err != nil || !up.Multipart || up.VersionID == "" || !strings.HasSuffix(up.ETag, "-2") {
		t.Fatalf("UploadObject: %+v, %v", up, err)
	}
	small, err := c.UploadObject(ctx, "bkt", "small", bytes.NewReader([]byte("x")), 1, s3client.UploadOptions{})
	if err != nil || small.Multipart || small.VersionID == "" || small.ETag != md5hex([]byte("x")) {
		t.Fatalf("UploadObject (single PUT): %+v, %v", small, err)
	}
	id, err := c.CreateMultipartUpload(ctx, "bkt", "mp", nil)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := c.UploadPartWith(ctx, "bkt", "mp", id, 1, strings.NewReader("part"), 4, s3client.PartOptions{ContentMD5: md5raw([]byte("part"))})
	if err != nil {
		t.Fatal(err)
	}
	done, err := c.Complete(ctx, "bkt", "mp", id, []s3client.CompletedPart{{Number: 1, ETag: e1}}, s3client.CompleteOptions{})
	if err != nil || done.VersionID == "" || !done.Multipart {
		t.Fatalf("Complete: %+v, %v", done, err)
	}
	if _, a := readVersion(t, c, "bkt", "mp", done.VersionID); a.ETag != done.ETag {
		t.Errorf("completed object ETag %q, Complete said %q", a.ETag, done.ETag)
	}

	// Copies from a source version, also across buckets.
	cp, err := c.CopyObjectWith(ctx, "bkt", "copy", "k", s3client.CopyOptions{SourceVersionID: v1.VersionID})
	if err != nil || cp.VersionID == "" || cp.ETag != v1.ETag {
		t.Fatalf("CopyObjectWith v1: %+v, %v", cp, err)
	}
	if got, _ := readVersion(t, c, "bkt", "copy", cp.VersionID); got != "one" {
		t.Errorf("copy of v1 = %q", got)
	}
	if err := c.CreateBucket(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	cp, err = c.CopyObjectWith(ctx, "other", "copy", "k", s3client.CopyOptions{SourceBucket: "bkt", SourceVersionID: v1.VersionID})
	if err != nil || cp.VersionID != "" {
		t.Fatalf("CopyObjectWith into an unversioned bucket: %+v, %v (want no version ID)", cp, err)
	}
	if got, _ := readVersion(t, c, "other", "copy", ""); got != "one" {
		t.Errorf("cross-bucket copy of v1 = %q", got)
	}
	// UploadPartCopy of a source version.
	id, err = c.CreateMultipartUpload(ctx, "bkt", "pc", nil)
	if err != nil {
		t.Fatal(err)
	}
	pe, err := c.UploadPartCopyWith(ctx, "bkt", "pc", id, 1, s3client.CopyRange{Bucket: "bkt", Key: "k", VersionID: v1.VersionID, Offset: 1, Length: 2, IfMatch: v1.ETag})
	if err != nil {
		t.Fatalf("UploadPartCopyWith: %v", err)
	}
	if _, err := c.Complete(ctx, "bkt", "pc", id, []s3client.CompletedPart{{Number: 1, ETag: pe}}, s3client.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readVersion(t, c, "bkt", "pc", ""); got != "ne" {
		t.Errorf("part copy of v1[1:3] = %q", got)
	}
	id, err = c.CreateMultipartUpload(ctx, "bkt", "pc2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadPartCopyWith(ctx, "bkt", "pc2", id, 1, s3client.CopyRange{Key: "k", VersionID: v1.VersionID, IfMatch: v2.ETag}); !s3client.IsPreconditionFailed(err) {
		t.Errorf("UploadPartCopyWith with another version's ETag: %v, want 412", err)
	}
	// Without a range the whole source version is copied.
	pe, err = c.UploadPartCopyWith(ctx, "bkt", "pc2", id, 1, s3client.CopyRange{Key: "k", VersionID: v1.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(ctx, "bkt", "pc2", id, []s3client.CompletedPart{{Number: 1, ETag: pe}}, s3client.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readVersion(t, c, "bkt", "pc2", ""); got != "one" {
		t.Errorf("whole part copy of v1 = %q", got)
	}
}

// TestUnversionedWriteResults documents the version IDs the gos3 remote
// reports: none for unversioned buckets, "null" once versioning is
// suspended.
func TestUnversionedWriteResults(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	if res := putString(t, c, "bkt", "k", "x"); res.VersionID != "" {
		t.Errorf("Put into an unversioned bucket: version %q, want none", res.VersionID)
	}
	if _, a := readVersion(t, c, "bkt", "k", ""); a.VersionID != "" {
		t.Errorf("GET from an unversioned bucket: version %q, want none", a.VersionID)
	}
	if err := c.PutBucketVersioning(ctx, "bkt", "Suspended"); err != nil {
		t.Fatal(err)
	}
	if res := putString(t, c, "bkt", "k", "y"); res.VersionID != "null" {
		t.Errorf("Put into a suspended bucket: version %q, want null", res.VersionID)
	}
	res, err := c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{})
	if err != nil || !res.DeleteMarker || res.VersionID != "null" {
		t.Errorf("DELETE in a suspended bucket: %+v, %v; want a null delete marker", res, err)
	}
}

func TestDeleteMarkers(t *testing.T) {
	c := versionedSetup(t)
	ctx := t.Context()
	v1 := putString(t, c, "bkt", "k", "one")

	res, err := c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{})
	if err != nil || !res.DeleteMarker || res.VersionID == "" {
		t.Fatalf("DELETE: %+v, %v; want a delete marker", res, err)
	}
	marker := res.VersionID

	// The current version is the marker: 404 with the marker headers.
	_, err = c.HeadObject(ctx, "bkt", "k")
	if vid, ok := s3client.IsDeleteMarker(err); !ok || vid != marker || !s3client.IsNotFound(err) {
		t.Errorf("HEAD of a deleted key: %v (marker %q, %v)", err, vid, ok)
	}
	_, _, err = c.GetObject(ctx, "bkt", "k", s3client.GetOptions{})
	if vid, ok := s3client.IsDeleteMarker(err); !ok || vid != marker || !s3client.IsCode(err, "NoSuchKey") {
		t.Errorf("GET of a deleted key: %v (marker %q, %v)", err, vid, ok)
	}

	// Addressed by version ID, a marker answers 405; HEAD without a body.
	_, err = c.HeadObjectWith(ctx, "bkt", "k", s3client.HeadOptions{VersionID: marker})
	var e *s3client.Error
	if !errors.As(err, &e) || e.StatusCode != http.StatusMethodNotAllowed || e.Code != "MethodNotAllowed" ||
		e.Header.Get("X-Amz-Delete-Marker") != "true" || e.Header.Get("X-Amz-Version-Id") != marker {
		t.Fatalf("HEAD of the marker version: %#v", err)
	}
	if vid, ok := s3client.IsDeleteMarker(err); !ok || vid != marker {
		t.Errorf("IsDeleteMarker(HEAD of the marker) = %q, %v", vid, ok)
	}
	_, _, err = c.GetObject(ctx, "bkt", "k", s3client.GetOptions{VersionID: marker})
	if vid, ok := s3client.IsDeleteMarker(err); !ok || vid != marker || !s3client.IsCode(err, "MethodNotAllowed") {
		t.Errorf("GET of the marker version: %v", err)
	}
	// Other errors are no delete markers.
	if _, err := c.HeadObject(ctx, "bkt", "missing"); !s3client.IsNotFound(err) {
		t.Fatalf("HEAD of a missing key: %v", err)
	} else if _, ok := s3client.IsDeleteMarker(err); ok {
		t.Errorf("IsDeleteMarker(missing key) = true")
	}

	// The old version is still there.
	if got, _ := readVersion(t, c, "bkt", "k", v1.VersionID); got != "one" {
		t.Errorf("GET v1 after the delete = %q", got)
	}

	// Deleting the marker restores the object; deleting by version ID
	// reports what was deleted.
	res, err = c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{VersionID: marker})
	if err != nil || !res.DeleteMarker || res.VersionID != marker {
		t.Fatalf("DELETE of the marker: %+v, %v", res, err)
	}
	if got, _ := readVersion(t, c, "bkt", "k", ""); got != "one" {
		t.Errorf("GET after removing the marker = %q", got)
	}
	res, err = c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{VersionID: v1.VersionID})
	if err != nil || res.DeleteMarker || res.VersionID != v1.VersionID {
		t.Fatalf("DELETE of v1: %+v, %v", res, err)
	}
	if _, err := c.HeadObject(ctx, "bkt", "k"); !s3client.IsNotFound(err) {
		t.Errorf("HEAD after deleting every version: %v", err)
	}
	// Deleting a version that does not exist: gos3 answers 204 as AWS.
	if _, err := c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{VersionID: v1.VersionID}); err != nil {
		t.Errorf("DELETE of a deleted version: %v", err)
	}
	// Without a version ID a missing bucket is still an error.
	if _, err := c.DeleteObjectWith(ctx, "nobucket", "k", s3client.DeleteOptions{}); !s3client.IsCode(err, "NoSuchBucket") {
		t.Errorf("DELETE in a missing bucket: %v", err)
	}
}

func TestListObjectVersions(t *testing.T) {
	c := versionedSetup(t)
	ctx := t.Context()
	keys := []string{"a%25b", "a+b", "a b", "dir/ünï 日本", "plain"}
	type op struct {
		key    string
		delete bool
	}
	// Interleave writes and deletes, so markers sit between versions.
	ops := []op{
		{keys[0], false}, {keys[0], true}, {keys[0], false},
		{keys[1], true}, {keys[1], false},
		{keys[2], false}, {keys[2], false}, {keys[2], true},
		{keys[3], false}, {keys[3], true}, {keys[3], false}, {keys[3], true},
		{keys[4], false},
	}
	byKey := map[string][]entry{} // newest first
	for i, o := range ops {
		var e entry
		if o.delete {
			res, err := c.DeleteObjectWith(ctx, "bkt", o.key, s3client.DeleteOptions{})
			if err != nil || !res.DeleteMarker {
				t.Fatalf("delete %q: %+v, %v", o.key, res, err)
			}
			e = entry{o.key, res.VersionID, true, false}
		} else {
			res := putString(t, c, "bkt", o.key, fmt.Sprint("data ", i))
			e = entry{o.key, res.VersionID, false, false}
		}
		byKey[o.key] = append([]entry{e}, byKey[o.key]...)
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	var want []entry
	for _, k := range sorted {
		es := byKey[k]
		es[0].Latest = true
		want = append(want, es...)
	}

	page, err := c.ListObjectVersions(ctx, "bkt", s3client.VersionListInput{})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if got := entriesOf(page.Entries); !slices.Equal(got, want) || page.IsTruncated {
		t.Fatalf("ListObjectVersions =\n%+v (truncated %v)\nwant\n%+v", got, page.IsTruncated, want)
	}
	for _, a := range page.Entries {
		switch {
		case a.LastModified.IsZero():
			t.Errorf("%q@%s: no LastModified", a.Key, a.VersionID)
		case a.DeleteMarker && (a.ETag != "" || a.Size != 0):
			t.Errorf("delete marker %q@%s: ETag %q, size %d", a.Key, a.VersionID, a.ETag, a.Size)
		case !a.DeleteMarker && (a.ETag == "" || strings.Contains(a.ETag, `"`) || a.Size == 0 || a.StorageClass == ""):
			t.Errorf("version %q@%s: %+v", a.Key, a.VersionID, a)
		}
	}

	// Paging with markers resumes exactly where the last page ended, also
	// within a key's versions.
	for _, max := range []int{1, 2, 3} {
		var got []entry
		in := s3client.VersionListInput{MaxKeys: max}
		for pages := 0; ; pages++ {
			if pages > len(want) {
				t.Fatalf("max-keys %d: paging does not end", max)
			}
			p, err := c.ListObjectVersions(ctx, "bkt", in)
			if err != nil {
				t.Fatalf("max-keys %d: %v", max, err)
			}
			if len(p.Entries) > max {
				t.Fatalf("max-keys %d: page of %d", max, len(p.Entries))
			}
			got = append(got, entriesOf(p.Entries)...)
			if !p.IsTruncated {
				break
			}
			in.KeyMarker, in.VersionIDMarker = p.NextKeyMarker, p.NextVersionIDMarker
		}
		if !slices.Equal(got, want) {
			t.Errorf("max-keys %d: paged listing\n%+v\nwant\n%+v", max, got, want)
		}
	}

	// Prefix and delimiter (encoded on the wire, decoded here).
	p, err := c.ListObjectVersions(ctx, "bkt", s3client.VersionListInput{Prefix: "a", Delimiter: " "})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.CommonPrefixes, []string{"a "}) {
		t.Errorf("common prefixes %q, want [\"a \"]", p.CommonPrefixes)
	}
	for _, a := range p.Entries {
		if a.Key != "a%25b" && a.Key != "a+b" {
			t.Errorf("prefix listing has %q", a.Key)
		}
	}
	p, err = c.ListObjectVersions(ctx, "bkt", s3client.VersionListInput{Prefix: "dir/", Delimiter: "/"})
	if err != nil || len(p.Entries) != 4 || p.Entries[0].Key != keys[3] {
		t.Errorf("listing of dir/: %+v, %v", p, err)
	}

	// DeleteVersions removes versions and markers by ID; results line up
	// with the request, missing versions included.
	var ids []s3client.ObjectID
	for _, e := range want {
		ids = append(ids, s3client.ObjectID{Key: e.Key, VersionID: e.VersionID})
	}
	ids = append(ids, s3client.ObjectID{Key: "plain", VersionID: want[0].VersionID})
	results, err := c.DeleteVersions(ctx, "bkt", ids, false)
	if err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	if len(results) != len(ids) {
		t.Fatalf("DeleteVersions: %d results for %d entries", len(results), len(ids))
	}
	for i, e := range want {
		if results[i].VersionID != e.VersionID || results[i].DeleteMarker != e.Marker {
			t.Errorf("DeleteVersions result %d = %+v, want %+v", i, results[i], e)
		}
	}
	if last := results[len(ids)-1]; last.VersionID != want[0].VersionID {
		t.Errorf("DeleteVersions of a missing version: %+v", last)
	}
	if p, err := c.ListObjectVersions(ctx, "bkt", s3client.VersionListInput{}); err != nil || len(p.Entries) != 0 {
		t.Errorf("versions left: %+v, %v", p.Entries, err)
	}
	// Without a version ID, DeleteVersions adds a delete marker.
	putString(t, c, "bkt", "k", "x")
	results, err = c.DeleteVersions(ctx, "bkt", []s3client.ObjectID{{Key: "k"}}, false)
	if err != nil || len(results) != 1 || !results[0].DeleteMarker || results[0].VersionID == "" {
		t.Errorf("DeleteVersions without a version ID: %+v, %v", results, err)
	}
	if _, err := c.DeleteVersions(ctx, "bkt", []s3client.ObjectID{{Key: ""}}, false); err == nil {
		t.Error("DeleteVersions with an empty key succeeded")
	}
}

func TestListUploadsAndParts(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	keys := []string{"u/a%b", "u/a+b", "u/a b", "u/日本"}
	ids := map[string]string{}
	for _, k := range keys {
		id, err := c.CreateMultipartUpload(ctx, "bkt", k, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[k] = id
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	var got []string
	in := s3client.ListUploadsInput{Prefix: "u/", MaxUploads: 3}
	for {
		page, err := c.ListMultipartUploads(ctx, "bkt", in)
		if err != nil {
			t.Fatalf("ListMultipartUploads: %v", err)
		}
		for _, u := range page.Uploads {
			if u.UploadID != ids[u.Key] || u.Initiated.IsZero() {
				t.Errorf("upload %+v, want ID %q", u, ids[u.Key])
			}
			got = append(got, u.Key)
		}
		if !page.IsTruncated {
			break
		}
		in.KeyMarker, in.UploadIDMarker = page.NextKeyMarker, page.NextUploadIDMarker
	}
	if !slices.Equal(got, sorted) {
		t.Errorf("uploads %q, want %q", got, sorted)
	}

	key, id := keys[0], ids[keys[0]]
	var etags []string
	for n := 1; n <= 3; n++ {
		data := randBytes(n)
		etag, err := c.UploadPart(ctx, "bkt", key, id, n, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		etags = append(etags, etag)
	}
	var parts []s3client.PartEntry
	marker := 0
	for {
		page, err := c.ListParts(ctx, "bkt", key, id, marker, 2)
		if err != nil {
			t.Fatalf("ListParts: %v", err)
		}
		parts = append(parts, page.Parts...)
		if !page.IsTruncated {
			break
		}
		marker = page.NextPartNumberMarker
	}
	if len(parts) != 3 {
		t.Fatalf("ListParts: %+v", parts)
	}
	for i, p := range parts {
		if p.Number != i+1 || p.ETag != etags[i] || p.Size != int64(i+1) || p.LastModified.IsZero() {
			t.Errorf("part %d: %+v, want ETag %q", i+1, p, etags[i])
		}
	}
	if _, err := c.ListParts(ctx, "bkt", key, "no-such-upload", 0, 0); !s3client.IsCode(err, "NoSuchUpload") {
		t.Errorf("ListParts of an unknown upload: %v", err)
	}
}

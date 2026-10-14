package s3client_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(n), 7))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func setup(t *testing.T) (*s3clienttest.Remote, *s3client.Client) {
	t.Helper()
	r := s3clienttest.Start(t)
	c := r.Client(t)
	if err := c.CreateBucket(t.Context(), "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Creating an existing own bucket is fine.
	if err := c.CreateBucket(t.Context(), "bkt"); err != nil {
		t.Fatalf("CreateBucket again: %v", err)
	}
	if err := c.HeadBucket(t.Context(), "bkt"); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	return r, c
}

func TestObjectRoundTrip(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	keys := []string{"plain", "dir/sub/file.txt", "spaces and+plus%percent~tilde", "ünïcödé/日本", "a//double", "trailing/"}
	for _, key := range keys {
		data := []byte("content of " + key)
		etag, err := c.PutObject(ctx, "bkt", key, bytes.NewReader(data), int64(len(data)), s3client.PutOptions{
			Header: http.Header{"Content-Type": {"text/plain"}, "X-Amz-Meta-Color": {"blue"}},
		})
		if err != nil {
			t.Fatalf("PutObject %q: %v", key, err)
		}
		if etag != md5hex(data) {
			t.Errorf("PutObject %q: etag %q", key, etag)
		}
		a, err := c.HeadObject(ctx, "bkt", key)
		if err != nil {
			t.Fatalf("HeadObject %q: %v", key, err)
		}
		if a.Size != int64(len(data)) || a.ETag != etag || a.Header.Get("Content-Type") != "text/plain" ||
			a.Header.Get("X-Amz-Meta-Color") != "blue" || a.LastModified.IsZero() {
			t.Errorf("HeadObject %q: %+v", key, a)
		}
		body, a, err := c.GetObject(ctx, "bkt", key, s3client.GetOptions{Offset: 3, Length: 4})
		if err != nil {
			t.Fatalf("GetObject %q: %v", key, err)
		}
		got, _ := io.ReadAll(body)
		body.Close()
		if string(got) != string(data[3:7]) || a.Size != int64(len(data)) {
			t.Errorf("ranged GetObject %q = %q (size %d)", key, got, a.Size)
		}
	}
	var listed []string
	err := c.Walk(ctx, "bkt", "", "", func(o s3client.ObjectAttrs) error {
		listed = append(listed, o.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := slices.Clone(keys)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("Walk = %q, want %q", listed, want)
	}
	for _, key := range keys {
		if err := c.DeleteObject(ctx, "bkt", key); err != nil {
			t.Fatalf("DeleteObject %q: %v", key, err)
		}
	}
	if _, err := c.HeadObject(ctx, "bkt", "plain"); !s3client.IsNotFound(err) {
		t.Errorf("HeadObject after delete: %v", err)
	}
	if err := c.DeleteObject(ctx, "bkt", "plain"); err != nil {
		t.Errorf("DeleteObject of a missing key: %v", err)
	}
	if _, _, err := c.GetObject(ctx, "nobucket", "x", s3client.GetOptions{}); !s3client.IsNotFound(err) || !s3client.IsCode(err, "NoSuchBucket") {
		t.Errorf("GetObject on a missing bucket: %v", err)
	}
}

func TestListPaging(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	for i := range 25 {
		key := fmt.Sprintf("p/%02d", i)
		if _, err := c.PutObject(ctx, "bkt", key, bytes.NewReader(nil), 0, s3client.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.ListObjectsV2(ctx, "bkt", s3client.ListInput{Prefix: "p/", MaxKeys: 10, StartAfter: "p/04"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 10 || page.Objects[0].Key != "p/05" || !page.IsTruncated {
		t.Fatalf("page 1: %d objects, first %q, truncated %v", len(page.Objects), page.Objects[0].Key, page.IsTruncated)
	}
	page, err = c.ListObjectsV2(ctx, "bkt", s3client.ListInput{Prefix: "p/", MaxKeys: 10, ContinuationToken: page.NextContinuationToken})
	if err != nil {
		t.Fatal(err)
	}
	if page.Objects[0].Key != "p/15" {
		t.Fatalf("page 2 starts at %q", page.Objects[0].Key)
	}
	dirs, err := c.ListObjectsV2(ctx, "bkt", s3client.ListInput{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs.CommonPrefixes, []string{"p/"}) {
		t.Errorf("common prefixes %q", dirs.CommonPrefixes)
	}
}

func TestConditionalPut(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	data := []byte("v1")
	etag, err := c.PutObject(ctx, "bkt", "k", bytes.NewReader(data), 2, s3client.PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PutObject(ctx, "bkt", "k", bytes.NewReader(data), 2, s3client.PutOptions{IfNoneMatch: true})
	if !s3client.IsPreconditionFailed(err) {
		t.Fatalf("second If-None-Match put: %v", err)
	}
	if _, err := c.PutObject(ctx, "bkt", "k", bytes.NewReader([]byte("v2")), 2, s3client.PutOptions{IfMatch: "0123"}); !s3client.IsPreconditionFailed(err) {
		t.Fatalf("If-Match with a wrong etag: %v", err)
	}
	if _, err := c.PutObject(ctx, "bkt", "k", bytes.NewReader([]byte("v2")), 2, s3client.PutOptions{IfMatch: etag}); err != nil {
		t.Fatalf("If-Match with the right etag: %v", err)
	}
}

func TestMultipartUploadAndCopy(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	data := randBytes(11<<20 + 123)
	etag, multipart, err := c.Upload(ctx, "bkt", "big", bytes.NewReader(data), int64(len(data)), s3client.UploadOptions{
		PartSize:   s3client.MinPartSize,
		PutOptions: s3client.PutOptions{Header: http.Header{"Content-Type": {"application/x-big"}}},
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !multipart || !strings.HasSuffix(etag, "-3") {
		t.Fatalf("Upload: multipart %v, etag %q", multipart, etag)
	}
	a, err := c.HeadObject(ctx, "bkt", "big")
	if err != nil || a.Size != int64(len(data)) || a.Header.Get("Content-Type") != "application/x-big" {
		t.Fatalf("HeadObject: %+v %v", a, err)
	}

	// The reader seeks lazily and pins the etag.
	r := c.NewReader(ctx, "bkt", "big", a.Size, a.ETag)
	if _, err := r.Seek(5<<20-10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 20)
	if _, err := io.ReadFull(r, buf); err != nil || !bytes.Equal(buf, data[5<<20-10:5<<20+10]) {
		t.Fatalf("read after seek: %v", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("ReadAll: %d bytes, %v", len(all), err)
	}
	r.Close()

	// Copy with replaced metadata.
	if _, err := c.Copy(ctx, "bkt", "copy", "big", a.Size, s3client.CopyOptions{
		ReplaceMetadata: true, Header: http.Header{"Content-Type": {"text/x-copy"}}, IfMatch: a.ETag,
	}); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	b, err := c.HeadObject(ctx, "bkt", "copy")
	if err != nil || b.Size != a.Size || b.Header.Get("Content-Type") != "text/x-copy" {
		t.Fatalf("copied object: %+v %v", b, err)
	}

	// A reader pinned to an old etag fails once the object changes.
	r = c.NewReader(ctx, "bkt", "big", a.Size, a.ETag)
	defer r.Close()
	if _, err := c.PutObject(ctx, "bkt", "big", bytes.NewReader([]byte("new")), 3, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(buf); !s3client.IsPreconditionFailed(err) {
		t.Fatalf("read of a replaced object: %v", err)
	}

	// Part copy assembling an object server-side.
	id, err := c.CreateMultipartUpload(ctx, "bkt", "assembled", nil)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := c.UploadPartCopy(ctx, "bkt", "assembled", id, 1, "copy", 0, 6<<20, "")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := c.UploadPartCopy(ctx, "bkt", "assembled", id, 2, "copy", 6<<20, int64(len(data))-6<<20, b.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, "bkt", "assembled", id, []s3client.CompletedPart{{1, e1}, {2, e2}}, s3client.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	body, _, err := c.GetObject(ctx, "bkt", "assembled", s3client.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("assembled object differs")
	}
	if err := c.AbortMultipartUpload(ctx, "bkt", "assembled", id); err != nil {
		t.Errorf("abort of a completed upload: %v", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	r, c := setup(t)
	var n atomic.Int32
	fail := func(*http.Request) int {
		if n.Add(1) == 1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r.Fail.Store(&fail)
	if _, err := c.PutObject(t.Context(), "bkt", "k", bytes.NewReader([]byte("x")), 1, s3client.PutOptions{}); err != nil {
		t.Fatalf("PutObject after one 503: %v", err)
	}
	always := func(*http.Request) int { return http.StatusInternalServerError }
	r.Fail.Store(&always)
	_, err := c.HeadObject(t.Context(), "bkt", "k")
	if !s3client.IsUnavailable(err) {
		t.Fatalf("persistent 500: %v", err)
	}
	var e *s3client.Error
	if !errors.As(err, &e) || e.StatusCode != 500 {
		t.Fatalf("error %#v", err)
	}
}

func TestSpool(t *testing.T) {
	for _, n := range []int{0, 10, 100, 1000} {
		s := s3client.NewSpool(t.TempDir(), 64)
		data := randBytes(n)
		for chunk := range slices.Chunk(data, 7) {
			if _, err := s.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Finish(); err != nil {
			t.Fatal(err)
		}
		if s.Size() != int64(n) {
			t.Fatalf("size %d", s.Size())
		}
		got, err := io.ReadAll(io.NewSectionReader(s, 0, s.Size()))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: read back %d bytes, %v", n, len(got), err)
		}
		if _, err := s.Write([]byte("x")); err == nil {
			t.Fatal("write after Finish succeeded")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

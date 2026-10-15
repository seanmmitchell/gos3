package s3client_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func TestListBuckets(t *testing.T) {
	r := s3clienttest.Start(t)
	c := r.Client(t)
	ctx := t.Context()
	names := []string{"list-a", "list-b", "list-c", "other"}
	for _, n := range names {
		if err := c.CreateBucketWith(ctx, n, s3client.CreateBucketOptions{}); err != nil {
			t.Fatalf("CreateBucketWith %s: %v", n, err)
		}
	}
	if err := c.CreateBucketWith(ctx, "list-eu", s3client.CreateBucketOptions{Region: "eu-west-1"}); err != nil {
		t.Fatalf("CreateBucketWith eu-west-1: %v", err)
	}

	all, err := c.ListBuckets(ctx, s3client.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if all.ContinuationToken != "" || len(all.Buckets) != 5 {
		t.Fatalf("ListBuckets: %+v", all)
	}
	for _, b := range all.Buckets {
		wantRegion := "us-east-1"
		if b.Name == "list-eu" {
			wantRegion = "eu-west-1"
		}
		if b.Region != wantRegion || b.CreationDate.IsZero() || time.Since(b.CreationDate) > time.Hour {
			t.Errorf("bucket %+v, want region %s", b, wantRegion)
		}
	}

	// One bucket per page.
	var paged []string
	in := s3client.ListBucketsInput{Prefix: "list-", MaxBuckets: 1}
	for {
		page, err := c.ListBuckets(ctx, in)
		if err != nil {
			t.Fatalf("ListBuckets page: %v", err)
		}
		if len(page.Buckets) != 1 {
			t.Fatalf("page of %d buckets", len(page.Buckets))
		}
		paged = append(paged, page.Buckets[0].Name)
		if page.ContinuationToken == "" {
			break
		}
		in.ContinuationToken = page.ContinuationToken
	}
	want := []string{"list-a", "list-b", "list-c", "list-eu"}
	if !slices.Equal(paged, want) {
		t.Errorf("paged ListBuckets = %q, want %q", paged, want)
	}

	eu, err := c.ListBuckets(ctx, s3client.ListBucketsInput{BucketRegion: "eu-west-1"})
	if err != nil || len(eu.Buckets) != 1 || eu.Buckets[0].Name != "list-eu" {
		t.Errorf("ListBuckets in eu-west-1: %+v, %v", eu, err)
	}

	var walked []string
	if err := c.WalkBuckets(ctx, "list-", func(b s3client.BucketEntry) error {
		walked = append(walked, b.Name)
		return nil
	}); err != nil {
		t.Fatalf("WalkBuckets: %v", err)
	}
	if !slices.Equal(walked, want) {
		t.Errorf("WalkBuckets = %q, want %q", walked, want)
	}
	stop := errors.New("stop")
	if err := c.WalkBuckets(ctx, "", func(s3client.BucketEntry) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("WalkBuckets with a failing callback: %v", err)
	}
}

// TestWalkBucketsPaging checks WalkBuckets against a scripted service that
// pages by itself, and one that repeats its continuation token.
func TestWalkBucketsPaging(t *testing.T) {
	var repeat atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("continuation-token")
		next := map[string]string{"": "t1", "t1": "t2", "t2": ""}[tok]
		if repeat.Load() && tok == "t1" {
			next = "t1"
		}
		fmt.Fprintf(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b-%s</Name><CreationDate>2024-01-02T03:04:05.000Z</CreationDate></Bucket>`+
			`<Bucket><Name>x-ignored-prefix</Name></Bucket></Buckets>`, tok)
		if next != "" {
			fmt.Fprintf(w, "<ContinuationToken>%s</ContinuationToken>", next)
		}
		io.WriteString(w, "</ListAllMyBucketsResult>")
	}))
	defer srv.Close()
	c, err := s3client.New(s3client.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "b", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	err = c.WalkBuckets(t.Context(), "b-", func(b s3client.BucketEntry) error {
		names = append(names, b.Name)
		if !b.CreationDate.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) || b.Region != "" {
			t.Errorf("entry %+v", b)
		}
		return nil
	})
	if err != nil || !slices.Equal(names, []string{"b-", "b-t1", "b-t2"}) {
		t.Fatalf("WalkBuckets: %q, %v", names, err)
	}
	repeat.Store(true)
	if err := c.WalkBuckets(t.Context(), "", func(s3client.BucketEntry) error { return nil }); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("WalkBuckets with a repeating token: %v", err)
	}
}

func TestCreateAndDeleteBucket(t *testing.T) {
	r := s3clienttest.Start(t)
	c := r.Client(t)
	ctx := t.Context()
	if err := c.CreateBucketWith(ctx, "lock", s3client.CreateBucketOptions{ObjectLock: true}); err != nil {
		t.Fatalf("CreateBucketWith: %v", err)
	}
	if v, err := c.GetBucketVersioning(ctx, "lock"); err != nil || v != "Enabled" {
		t.Errorf("versioning of an object lock bucket: %q, %v", v, err)
	}
	if cfg, found, err := c.GetObjectLockConfiguration(ctx, "lock"); err != nil || !found || !cfg.Enabled {
		t.Errorf("object lock configuration: %+v, %v, %v", cfg, found, err)
	}
	// An existing bucket is an error, unlike with CreateBucket.
	if err := c.CreateBucketWith(ctx, "lock", s3client.CreateBucketOptions{}); !s3client.IsCode(err, "BucketAlreadyOwnedByYou") {
		t.Errorf("CreateBucketWith of an existing bucket: %v", err)
	}
	if err := c.CreateBucket(ctx, "lock"); err != nil {
		t.Errorf("CreateBucket of an existing own bucket: %v", err)
	}

	// A non-empty bucket cannot be deleted; once its versions are gone it
	// can.
	v := putString(t, c, "lock", "k", "x")
	if err := c.DeleteBucket(ctx, "lock"); !s3client.IsCode(err, "BucketNotEmpty") {
		t.Errorf("DeleteBucket of a non-empty bucket: %v", err)
	}
	if _, err := c.DeleteVersions(ctx, "lock", []s3client.ObjectID{{Key: "k", VersionID: v.VersionID}}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(ctx, "lock"); err != nil {
		t.Errorf("DeleteBucket of an emptied object lock bucket: %v", err)
	}
	if err := c.CreateBucketWith(ctx, "empty", s3client.CreateBucketOptions{Region: "eu-central-1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(ctx, "empty"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if err := c.HeadBucket(ctx, "empty"); !s3client.IsNotFound(err) {
		t.Errorf("HeadBucket after delete: %v", err)
	}
	if err := c.DeleteBucket(ctx, "empty"); !s3client.IsCode(err, "NoSuchBucket") {
		t.Errorf("DeleteBucket of a missing bucket: %v", err)
	}
}

func TestBucketVersioning(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	if v, err := c.GetBucketVersioning(ctx, "bkt"); err != nil || v != "" {
		t.Fatalf("versioning of a new bucket: %q, %v", v, err)
	}
	for _, status := range []string{"Enabled", "Suspended", "Enabled"} {
		if err := c.PutBucketVersioning(ctx, "bkt", status); err != nil {
			t.Fatalf("PutBucketVersioning %s: %v", status, err)
		}
		if v, err := c.GetBucketVersioning(ctx, "bkt"); err != nil || v != status {
			t.Errorf("GetBucketVersioning = %q, %v; want %s", v, err, status)
		}
	}
	if err := c.PutBucketVersioning(ctx, "bkt", "Bogus"); err == nil {
		t.Error("PutBucketVersioning Bogus succeeded")
	}
	if _, err := c.GetBucketVersioning(ctx, "nobucket"); !s3client.IsCode(err, "NoSuchBucket") {
		t.Errorf("GetBucketVersioning of a missing bucket: %v", err)
	}
}

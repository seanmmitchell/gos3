package s3client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func newClient(t *testing.T, endpoint, region string) *s3client.Client {
	t.Helper()
	c, err := s3client.New(s3client.Config{Endpoint: endpoint, Region: region, AccessKey: "a", SecretKey: "b", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestForRegionEndpoints checks the endpoint rewriting of ForRegion.
func TestForRegionEndpoints(t *testing.T) {
	for _, tc := range []struct {
		endpoint, region, target, want string
	}{
		{"https://s3.amazonaws.com", "us-east-1", "eu-west-1", "https://s3.eu-west-1.amazonaws.com"},
		{"https://s3.eu-west-1.amazonaws.com", "eu-west-1", "us-west-2", "https://s3.us-west-2.amazonaws.com"},
		{"https://s3.eu-west-1.amazonaws.com", "eu-west-1", "us-east-1", "https://s3.us-east-1.amazonaws.com"},
		{"https://S3.EU-WEST-1.AmazonAWS.com", "eu-west-1", "us-east-2", "https://s3.us-east-2.amazonaws.com"},
		{"https://s3-eu-west-1.amazonaws.com", "eu-west-1", "ap-south-1", "https://s3.ap-south-1.amazonaws.com"},
		{"https://s3-external-1.amazonaws.com", "us-east-1", "eu-west-1", "https://s3.eu-west-1.amazonaws.com"},
		{"https://s3.dualstack.eu-west-1.amazonaws.com", "eu-west-1", "us-west-2", "https://s3.dualstack.us-west-2.amazonaws.com"},
		{"https://s3-fips.us-east-1.amazonaws.com", "us-east-1", "us-west-2", "https://s3-fips.us-west-2.amazonaws.com"},
		{"https://s3.cn-north-1.amazonaws.com.cn", "cn-north-1", "cn-northwest-1", "https://s3.cn-northwest-1.amazonaws.com.cn"},
		{"https://s3.amazonaws.com:8443", "us-east-1", "eu-west-1", "https://s3.eu-west-1.amazonaws.com:8443"},
		// Other hosts keep their endpoint; only the signing region changes.
		{"https://minio.example.com:9000", "us-east-1", "eu-west-1", "https://minio.example.com:9000"},
		{"http://127.0.0.1:9000", "us-east-1", "eu-west-1", "http://127.0.0.1:9000"},
		{"https://s3.amazonaws.com.example.com", "us-east-1", "eu-west-1", "https://s3.amazonaws.com.example.com"},
		{"https://bucket.s3.amazonaws.com", "us-east-1", "eu-west-1", "https://bucket.s3.amazonaws.com"},
		{"https://s3-control.us-east-1.amazonaws.com", "us-east-1", "eu-west-1", "https://s3-control.us-east-1.amazonaws.com"},
		{"https://s3-website-us-east-1.amazonaws.com", "us-east-1", "eu-west-1", "https://s3-website-us-east-1.amazonaws.com"},
	} {
		c := newClient(t, tc.endpoint, tc.region)
		got := c.ForRegion(tc.target)
		if got.Endpoint() != tc.want || got.Region() != tc.target {
			t.Errorf("%s ForRegion(%s) = %s signing %s, want %s", tc.endpoint, tc.target, got.Endpoint(), got.Region(), tc.want)
		}
		if c.Endpoint() != tc.endpoint || c.Region() != tc.region {
			t.Errorf("ForRegion changed the base client: %s %s", c.Endpoint(), c.Region())
		}
	}
}

// TestForRegionMemoized checks that clones are shared per region.
func TestForRegionMemoized(t *testing.T) {
	c := newClient(t, "https://s3.amazonaws.com", "")
	if c.Region() != "us-east-1" {
		t.Fatalf("default region %q", c.Region())
	}
	if c.ForRegion("") != c || c.ForRegion("us-east-1") != c {
		t.Error("ForRegion of the client's own region is not the client")
	}
	eu := c.ForRegion("eu-west-1")
	if eu == c || c.ForRegion("eu-west-1") != eu {
		t.Error("ForRegion(eu-west-1) is not memoized")
	}
	if eu.ForRegion("eu-west-1") != eu || eu.ForRegion("") != eu || eu.ForRegion("us-east-1") != c {
		t.Error("a regional clone does not lead back to the shared clients")
	}
	if eu.ForRegion("ap-south-1") != c.ForRegion("ap-south-1") {
		t.Error("clones of clones are not shared")
	}
	if got := eu.ForRegion("us-west-2").Endpoint(); got != "https://s3.us-west-2.amazonaws.com" {
		t.Errorf("clone of a clone addresses %s", got)
	}

	// Concurrent first calls agree on one clone.
	var wg sync.WaitGroup
	got := make([]*s3client.Client, 16)
	for i := range got {
		wg.Go(func() { got[i] = c.ForRegion("sa-east-1") })
	}
	wg.Wait()
	for _, g := range got {
		if g != got[0] {
			t.Fatal("concurrent ForRegion calls returned different clients")
		}
	}
}

func TestBucketRegion(t *testing.T) {
	r := s3clienttest.Start(t)
	c := r.Client(t)
	ctx := t.Context()
	if err := c.CreateBucketWith(ctx, "east", s3client.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucketWith(ctx, "west", s3client.CreateBucketOptions{Region: "eu-west-1"}); err != nil {
		t.Fatal(err)
	}
	for bucket, want := range map[string]string{"east": "us-east-1", "west": "eu-west-1"} {
		if got, err := c.BucketRegion(ctx, bucket); err != nil || got != want {
			t.Errorf("BucketRegion(%s) = %q, %v; want %s", bucket, got, err, want)
		}
	}
	if loc, err := c.GetBucketLocation(ctx, "east"); err != nil || loc != "" {
		t.Errorf("GetBucketLocation(east) = %q, %v; want \"\"", loc, err)
	}
	if loc, err := c.GetBucketLocation(ctx, "west"); err != nil || loc != "eu-west-1" {
		t.Errorf("GetBucketLocation(west) = %q, %v", loc, err)
	}
	if _, err := c.BucketRegion(ctx, "missing"); !s3client.IsNotFound(err) {
		t.Errorf("BucketRegion of a missing bucket: %v", err)
	}
}

// TestBucketRegionFallback checks BucketRegion against services that do
// not send x-amz-bucket-region.
func TestBucketRegionFallback(t *testing.T) {
	var noLocation atomic.Bool
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			location := req.Method == http.MethodGet && req.URL.Query().Has("location")
			switch {
			case req.Method == http.MethodHead && strings.Count(req.URL.Path, "/") == 1:
				// HEAD bucket without the region header.
				w.WriteHeader(http.StatusOK)
			case location && noLocation.Load():
				w.WriteHeader(http.StatusNotImplemented)
			case location && req.URL.Path == "/euro":
				// The legacy name of eu-west-1, which gos3 never sends.
				io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">EU</LocationConstraint>`)
			default:
				h.ServeHTTP(w, req)
			}
		})
	}})
	c := r.Client(t)
	ctx := t.Context()
	if err := c.CreateBucketWith(ctx, "east", s3client.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucketWith(ctx, "euro", s3client.CreateBucketOptions{Region: "eu-west-1"}); err != nil {
		t.Fatal(err)
	}
	// "" from GetBucketLocation means us-east-1, "EU" eu-west-1.
	for bucket, want := range map[string]string{"east": "us-east-1", "euro": "eu-west-1"} {
		if got, err := c.BucketRegion(ctx, bucket); err != nil || got != want {
			t.Errorf("BucketRegion(%s) = %q, %v; want %s", bucket, got, err, want)
		}
	}
	// A service that reports nothing.
	noLocation.Store(true)
	if got, err := c.BucketRegion(ctx, "east"); err != nil || got != "" {
		t.Errorf("BucketRegion without any region information = %q, %v", got, err)
	}
}

// TestBucketRegionRedirect checks the region of a 301 answer to HEAD
// bucket, as AWS sends for buckets in another region.
func TestBucketRegionRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Bucket-Region", "ap-southeast-2")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, "us-east-1")
	err := c.HeadBucket(t.Context(), "bkt")
	if region, ok := s3client.IsWrongRegion(err); !ok || region != "ap-southeast-2" || !s3client.IsCode(err, "PermanentRedirect") {
		t.Errorf("HeadBucket: %v; IsWrongRegion = %q, %v", err, region, ok)
	}
	if got, err := c.BucketRegion(t.Context(), "bkt"); err != nil || got != "ap-southeast-2" {
		t.Errorf("BucketRegion = %q, %v", got, err)
	}
}

// TestStrictRegion checks a remote rejecting requests signed for another
// region, and how a client recovers the right one.
func TestStrictRegion(t *testing.T) {
	r := s3clienttest.StartWith(t, s3clienttest.Options{StrictRegion: true})
	good := r.Client(t)
	ctx := t.Context()
	if err := good.CreateBucketWith(ctx, "bkt", s3client.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := r.Config()
	cfg.Region = "eu-west-1"
	wrong, err := s3client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrong.GetBucketVersioning(ctx, "bkt")
	region, ok := s3client.IsWrongRegion(err)
	if !ok || region != s3clienttest.Region || !s3client.IsCode(err, "AuthorizationHeaderMalformed") {
		t.Fatalf("request signed for the wrong region: %v; IsWrongRegion = %q, %v", err, region, ok)
	}
	if got, err := wrong.BucketRegion(ctx, "bkt"); err != nil || got != s3clienttest.Region {
		t.Errorf("BucketRegion with the wrong region = %q, %v", got, err)
	}
	if _, err := wrong.ForRegion(region).GetBucketVersioning(ctx, "bkt"); err != nil {
		t.Errorf("request through ForRegion(%s): %v", region, err)
	}
}

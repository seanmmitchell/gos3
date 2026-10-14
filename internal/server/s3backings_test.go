package server

import (
	"context"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

// TestS3BackingsEndToEnd runs a server with a blob and a gateway s3
// backing on an in-process remote S3 service and stores one bucket on each
// (chosen with x-gos3-backing), next to one on the default backing.
func TestS3BackingsEndToEnd(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "blobs")
	rc := remote.Client(t)

	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	backing := func(name, mode, bucket, prefix string) config.BackingConfig {
		return config.BackingConfig{
			Name: name, Backend: config.BackendS3, Path: t.TempDir(),
			S3: &config.S3BackingConfig{
				Mode: mode, Endpoint: remote.URL, Region: s3clienttest.Region,
				Bucket: bucket, Prefix: prefix, Addressing: "path",
				AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
			},
		}
	}
	cfg.Storage.Backings = []config.BackingConfig{
		backing("cold", config.S3ModeBlob, "blobs", "p/"),
		backing("gw", config.S3ModeGateway, "", ""),
	}
	srv := start(t, cfg)
	creds := rootCreds

	buckets := map[string]string{"b-default": "", "b-cold": "cold", "b-gw": "gw"}
	for bucket, b := range buckets {
		hdr := map[string]string{}
		if b != "" {
			hdr["X-Gos3-Backing"] = b
		}
		if r := do(t, "PUT", srv.url+"/"+bucket, nil, &creds, hdr); r.status != 200 {
			t.Fatalf("CreateBucket %s: %d %s", bucket, r.status, r.body)
		}
		if r := do(t, "PUT", srv.url+"/"+bucket+"/dir/obj.txt", []byte("hello "+bucket), &creds, nil); r.status != 200 {
			t.Fatalf("PutObject %s: %d %s", bucket, r.status, r.body)
		}
	}
	if r := do(t, "PUT", srv.url+"/b-nope", nil, &creds, map[string]string{"X-Gos3-Backing": "nope"}); r.status != 400 {
		t.Fatalf("unknown backing: %d %s", r.status, r.body)
	}

	check := func(srv *running) {
		t.Helper()
		for bucket, b := range buckets {
			r := do(t, "GET", srv.url+"/"+bucket+"/dir/obj.txt", nil, &creds, nil)
			if r.status != 200 || string(r.body) != "hello "+bucket {
				t.Errorf("GetObject %s: %d %q", bucket, r.status, r.body)
			}
			want := b
			if want == "" {
				want = config.DefaultBackingName
			}
			if got := do(t, "HEAD", srv.url+"/"+bucket, nil, &creds, nil).header.Get("X-Gos3-Backing"); got != want {
				t.Errorf("HeadBucket %s: x-gos3-backing %q, want %q", bucket, got, want)
			}
		}
	}
	check(srv)

	// The blob bucket's data is on the remote under the prefix; the gateway
	// bucket is a native remote bucket.
	var blobKeys []string
	if err := rc.Walk(context.Background(), "blobs", "", "", func(o s3client.ObjectAttrs) error {
		blobKeys = append(blobKeys, o.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(blobKeys) != 1 || !strings.HasPrefix(blobKeys[0], "p/data/") {
		t.Errorf("remote blobs = %q", blobKeys)
	}
	body, _, err := rc.GetObject(context.Background(), "b-gw", "dir/obj.txt", s3client.GetOptions{})
	if err != nil {
		t.Fatalf("native gateway object: %v", err)
	}
	body.Close()

	// Gateway buckets are unversioned.
	versioning := []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	if r := do(t, "PUT", srv.url+"/b-gw?versioning", versioning, &creds, nil); r.status != 501 {
		t.Errorf("versioning on a gateway bucket: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", srv.url+"/b-cold?versioning", versioning, &creds, nil); r.status != 200 {
		t.Errorf("versioning on a blob bucket: %d %s", r.status, r.body)
	}

	// Everything survives a restart.
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	check(start(t, cfg))
}

// TestS3BackingConfigErrors checks configuration errors surface at New.
func TestS3BackingConfigErrors(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Backings = []config.BackingConfig{{
		Name: "cold", Backend: config.BackendS3, Path: t.TempDir(),
		S3: &config.S3BackingConfig{Mode: config.S3ModeBlob, Endpoint: "http://127.0.0.1:1", AccessKey: "a", SecretKey: "s"},
	}}
	if _, err := New(cfg, testLogger(t)); err == nil || !strings.Contains(err.Error(), "bucket: required in blob mode") {
		t.Fatalf("blob backing without a bucket: %v", err)
	}
	cfg.Storage.Backings[0].S3 = nil
	if _, err := New(cfg, testLogger(t)); err == nil || !strings.Contains(err.Error(), "s3: required") {
		t.Fatalf("s3 backing without s3 settings: %v", err)
	}
}

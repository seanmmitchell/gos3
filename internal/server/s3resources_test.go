package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// remoteKeys lists the keys of the remote's bucket (nil when it does not
// exist).
func remoteKeys(t *testing.T, remote *s3clienttest.Remote, bucket string) []string {
	t.Helper()
	var keys []string
	err := remote.Backend.WalkObjects(context.Background(), bucket, "", "", func(o storage.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	if err != nil {
		return nil
	}
	return keys
}

// A gateway backing refuses gos3 buckets named like the remote bucket of
// another s3 backing (or its own metadata bucket): it would adopt that
// backing's data as objects.
func TestGatewayRefusesBackingBuckets(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	remote.CreateBucket(t, "gwmeta")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	gw := s3Settings(remote, "gwmeta", "")
	gw.Gateway = true
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "coldstore", "cold/")},
		{Name: "gw", Backend: config.BackendS3, S3: gw},
	}
	srv := start(t, cfg)
	mustStatus(t, "create on cold", do(t, "PUT", srv.url+"/frozen", nil, &rootCreds, map[string]string{"x-gos3-backing": "cold"}), 200)
	mustStatus(t, "put on cold", do(t, "PUT", srv.url+"/frozen/k", []byte("ice"), &rootCreds, nil), 200)
	before := remoteKeys(t, remote, "coldstore")

	for _, name := range []string{"coldstore", "gwmeta"} {
		res := do(t, "PUT", srv.url+"/"+name, nil, &rootCreds, map[string]string{"x-gos3-backing": "gw"})
		if res.status != http.StatusConflict && res.status != http.StatusBadRequest {
			t.Fatalf("create %s on gw: %d %s", name, res.status, res.body)
		}
		mustStatus(t, "HEAD "+name, do(t, "HEAD", srv.url+"/"+name, nil, &rootCreds, nil), 404)
	}
	if after := remoteKeys(t, remote, "coldstore"); !slices.Equal(before, after) {
		t.Fatalf("cold's remote bucket changed:\nbefore %v\nafter  %v", before, after)
	}
	getBody(t, srv.url+"/frozen/k", "ice")
	// Other names still work on the gateway.
	mustStatus(t, "create on gw", do(t, "PUT", srv.url+"/native", nil, &rootCreds, map[string]string{"x-gos3-backing": "gw"}), 200)
}

// The connection settings of a gateway backing whose metadata store has
// not loaded change only when no bucket is placed on it.
func TestGatewayReconfigureWhileUnloaded(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "gwa-meta")
	remote.CreateBucket(t, "gwb-meta")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	gwa, gwb := s3Settings(remote, "gwa-meta", ""), s3Settings(remote, "gwb-meta", "")
	gwa.Gateway, gwb.Gateway = true, true
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "gwa", Backend: config.BackendS3, S3: gwa},
		{Name: "gwb", Backend: config.BackendS3, S3: gwb},
	}
	cfg.Storage.Replication.HealthInterval = time.Second
	srv := start(t, cfg)
	mustStatus(t, "create on gwa", do(t, "PUT", srv.url+"/inuse", nil, &rootCreds, map[string]string{"x-gos3-backing": "gwa"}), 200)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}

	// The service is unreachable under its configured name, but reachable
	// as localhost.
	down := func(r *http.Request) int {
		if strings.HasPrefix(r.Host, "127.0.0.1") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	remote.Fail.Store(&down)
	srv = start(t, cfg)
	st := backingStatuses(t, srv.url)
	if st["gwa"].Healthy || st["gwb"].Healthy || st["gwa"].Buckets != 1 {
		t.Fatalf("backings while down: %+v", st)
	}
	const endpoint = "/_gos3/admin/v1/backings/config"
	localhost := strings.Replace(remote.URL, "127.0.0.1", "localhost", 1)
	change := func(name string) response {
		var in multi.BackingConfiguration
		do(t, "GET", srv.url+endpoint, nil, &rootCreds, nil).json(t, &in)
		for i := range in.Backings {
			if in.Backings[i].Name == name {
				in.Backings[i].S3.Endpoint = localhost
			}
		}
		raw, _ := json.Marshal(in)
		return do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
	}
	mustStatus(t, "change gwa (a bucket is placed on it)", change("gwa"), 409)
	mustStatus(t, "change gwb (no bucket on it)", change("gwb"), 200)
	waitFor(t, 60*time.Second, "gwb healthy", func() bool { return backingStatuses(t, srv.url)["gwb"].Healthy })
	// gwb's gateway store uses the new endpoint too: once gwa has been
	// loaded (so that its buckets are known), a bucket can be created on
	// gwb while the old name is still unreachable.
	remote.Fail.Store(nil)
	waitFor(t, 60*time.Second, "gwa healthy", func() bool { return backingStatuses(t, srv.url)["gwa"].Healthy })
	remote.Fail.Store(&down)
	mustStatus(t, "create on gwb", do(t, "PUT", srv.url+"/fresh", nil, &rootCreds, map[string]string{"x-gos3-backing": "gwb"}), 200)
	mustStatus(t, "put on gwb", do(t, "PUT", srv.url+"/fresh/k", []byte("new"), &rootCreds, nil), 200)
	getBody(t, srv.url+"/fresh/k", "new")
}

// Temporary directories of s3 backings are removed when the backing is
// closed (a refused live open, a removed backing), and those a previous
// run of the server left behind are removed at startup.
func TestS3BackingTempDirs(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "live")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	srv := start(t, cfg) // stores the server ID
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	id := srv.ServerID()
	stale, err := os.MkdirTemp("", "gos3-"+id+"~cold-")
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.MkdirTemp("", "gos3-0123456789abcdef~cold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other); _ = os.RemoveAll(stale) })

	srv = start(t, cfg)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temporary directory of this server: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("temporary directory of another server: %v", err)
	}
	ours := func() []string {
		dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "gos3-"+id+"~*"))
		return dirs
	}

	const endpoint = "/_gos3/admin/v1/backings/config"
	put := func(defs ...config.BackingDefinition) response {
		var in multi.BackingConfiguration
		do(t, "GET", srv.url+endpoint, nil, &rootCreds, nil).json(t, &in)
		in.Backings = defs
		raw, _ := json.Marshal(in)
		return do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
	}
	def := config.BackingDefinition{Name: "cold", Backend: config.BackendS3, S3: &config.S3Definition{
		Endpoint: remote.URL, Bucket: "live", Prefix: "cold/", Addressing: "path",
		AccessKey: s3clienttest.AccessKey, SecretKey: "wrong-secret-key-000000000000000000000",
	}}
	mustStatus(t, "add with a wrong secret", put(def), 400)
	if d := ours(); len(d) != 0 {
		t.Fatalf("temporary directories after a refused open: %v", d)
	}
	def.S3.SecretKey = s3clienttest.SecretKey
	mustStatus(t, "add", put(def), 200)
	if d := ours(); len(d) != 1 {
		t.Fatalf("temporary directories of the open backing: %v", d)
	}
	mustStatus(t, "remove", put(), 200)
	if d := ours(); len(d) != 0 {
		t.Fatalf("temporary directories after removing the backing: %v", d)
	}
}

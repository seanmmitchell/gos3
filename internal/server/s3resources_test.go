package server

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/seanmmitchell/gos3/internal/storage/localstate"
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

// Remote locations are claimed server-wide: no native bucket can use the
// home of another s3 backing (or a remote bucket another native bucket
// uses), whichever backing asks, so that gos3 never serves a backing's
// data as objects.
func TestS3ClaimsRefuseUsedLocations(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "coldstore", "cold/")},
		{Name: "edge", Backend: config.BackendS3, S3: s3Settings(remote, "", "")},
	}
	srv := start(t, cfg)
	createOn(t, srv.url, "frozen", "cold", nil)
	mustStatus(t, "put on cold", do(t, "PUT", srv.url+"/frozen/k", []byte("ice"), &rootCreds, nil), 200)
	createOn(t, srv.url, "native", "edge", nil)
	before := remoteKeys(t, remote, "coldstore")

	// Through the S3 API: a native bucket whose remote bucket is cold's
	// home, or another native bucket's remote bucket.
	for _, c := range []struct{ bucket, remoteBucket, backing string }{
		{"coldstore", "", "edge"}, {"other", "coldstore", "edge"}, {"copycat", "native", "edge"},
		{"viacold", "coldstore", "cold"}, {"native2", "native", "cold"},
	} {
		hdr := map[string]string{"x-gos3-backing": c.backing, "x-gos3-bucket-kind": "native"}
		if c.remoteBucket != "" {
			hdr["x-gos3-remote-bucket"] = c.remoteBucket
		}
		res := do(t, "PUT", srv.url+"/"+c.bucket, nil, &rootCreds, hdr)
		mustStatusIn(t, "create "+c.bucket+" on "+c.backing, res, http.StatusBadRequest, http.StatusConflict)
		mustStatus(t, "HEAD "+c.bucket, do(t, "HEAD", srv.url+"/"+c.bucket, nil, &rootCreds, nil), 404)
	}
	// Through the admin API: adopting them is a conflict.
	for _, rb := range []string{"coldstore", "native"} {
		res := adminDo(t, srv.url, "POST", "/backings/edge/buckets", map[string]any{
			"bucket": "adopted-" + rb, "kind": "native", "remoteBucket": rb, "adopt": true,
		})
		mustStatus(t, "adopt "+rb, res, http.StatusConflict)
	}
	// A container inside cold's home is a conflict too.
	res := adminDo(t, srv.url, "POST", "/backings/edge/containers", map[string]any{"bucket": "coldstore", "prefix": "cold/inner/"})
	mustStatus(t, "container inside cold's home", res, http.StatusConflict)

	if after := remoteKeys(t, remote, "coldstore"); !slices.Equal(before, after) {
		t.Fatalf("cold's remote bucket changed:\nbefore %v\nafter  %v", before, after)
	}
	getBody(t, srv.url+"/frozen/k", "ice")
	// Other names still work.
	createOn(t, srv.url, "fresh", "edge", nil)
	headBucket(t, srv.url, "fresh", "edge", "native")
}

// The connection settings of an s3 backing whose home has not loaded
// (wrong settings, an unreachable service) change in place, also while
// buckets are placed on it; so do those of a backing without a home.
func TestS3ReconfigureWhileUnloaded(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "cold-home")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "cold-home", "")},
		{Name: "edge", Backend: config.BackendS3, S3: s3Settings(remote, "", "")},
	}
	cfg.Storage.Replication.HealthInterval = time.Second
	srv := start(t, cfg)
	createOn(t, srv.url, "inuse", "cold", nil)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/inuse/k", []byte("kept"), &rootCreds, nil), 200)
	createOn(t, srv.url, "nearby", "edge", nil)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/nearby/k", []byte("close"), &rootCreds, nil), 200)
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
	if st := backingStatuses(t, srv.url); st["cold"].Healthy || st["cold"].Buckets != 1 {
		t.Fatalf("backings while down: %+v", st)
	}
	mustStatus(t, "get while down", do(t, "GET", srv.url+"/inuse/k", nil, &rootCreds, nil), 503)
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
	mustStatus(t, "change cold (a bucket is placed on it)", change("cold"), 200)
	waitFor(t, 60*time.Second, "cold healthy", func() bool { return backingStatuses(t, srv.url)["cold"].Healthy })
	getBody(t, srv.url+"/inuse/k", "kept")
	mustStatus(t, "put after the change", do(t, "PUT", srv.url+"/inuse/k2", []byte("new"), &rootCreds, nil), 200)
	createOn(t, srv.url, "fresh", "cold", nil)

	mustStatus(t, "change edge", change("edge"), 200)
	waitFor(t, 60*time.Second, "edge healthy", func() bool { return backingStatuses(t, srv.url)["edge"].Healthy })
	getBody(t, srv.url+"/nearby/k", "close")
	createOn(t, srv.url, "fresh-native", "edge", nil)
	if !remoteHas(remote, "fresh-native") {
		t.Fatal("no remote bucket for the new native bucket")
	}
}

// The records of an s3 backing without a home are locked while the server
// runs: a second process cannot open them.
func TestS3LocalRecordsLocked(t *testing.T) {
	remote := s3clienttest.Start(t)
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendS3
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.S3 = s3Settings(remote, "", "")
	srv := start(t, cfg)
	if b := backingStatuses(t, srv.url)["default"]; b.Location != "local:" || b.DefaultKind != "native" || !b.Healthy {
		t.Fatalf("default backing: %+v", b)
	}
	mustStatus(t, "create", do(t, "PUT", srv.url+"/native", nil, &rootCreds, nil), 200)
	headBucket(t, srv.url, "native", "default", "native")
	dir := S3RecordsDir(filepath.Join(cfg.Storage.Path, config.SystemDirName), config.DefaultBackingName)
	db, err := localstate.Open(dir, localstate.Options{LockTimeout: 100 * time.Millisecond})
	if err == nil {
		_ = db.Close()
		t.Fatal("the records of a running backing could be opened")
	}
	if !errors.Is(err, localstate.ErrLocked) {
		t.Fatalf("opening the records of a running backing: %v", err)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	headBucket(t, srv.url, "native", "default", "native")
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
	def := s3Definition(remote, "cold", "live", "cold/")
	def.S3.SecretKey = "wrong-secret-key-000000000000000000000"
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

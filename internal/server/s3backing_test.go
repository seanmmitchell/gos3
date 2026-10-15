package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// End-to-end tests of s3 backings against an in-process S3 service
// (s3clienttest): the default backing on s3, additional s3 and gateway
// backings, live configuration, restarts and an unreachable service.

// s3Settings returns the settings of an s3 backing on remote's bucket.
func s3Settings(remote *s3clienttest.Remote, bucket, prefix string) *config.S3Config {
	return &config.S3Config{
		Endpoint:   remote.URL,
		Bucket:     bucket,
		Prefix:     prefix,
		Addressing: "path",
		AccessKey:  s3clienttest.AccessKey,
		SecretKey:  s3clienttest.SecretKey,
	}
}

// gos3Client returns an S3 client for the gos3 server at url.
func gos3Client(t *testing.T, url string) *s3client.Client {
	t.Helper()
	c, err := s3client.New(s3client.Config{
		Endpoint: url, Region: "us-east-1", AccessKey: testAccessKey, SecretKey: testSecretKey, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustStatus fails unless res has the status want.
func mustStatus(t *testing.T, what string, res response, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("%s: got %d, want %d: %s", what, res.status, want, res.body)
	}
}

// getBody reads url and fails unless it answers 200 with want.
func getBody(t *testing.T, url, want string) {
	t.Helper()
	res := do(t, "GET", url, nil, &rootCreds, nil)
	if res.status != 200 || string(res.body) != want {
		t.Fatalf("GET %s: %d %q, want %q", url, res.status, res.body, want)
	}
}

const versioningEnabled = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`

// backingStatuses returns GET /_gos3/admin/v1/backings by name.
func backingStatuses(t *testing.T, url string) map[string]multi.BackingStatus {
	t.Helper()
	var out struct {
		Backings []multi.BackingStatus `json:"backings"`
	}
	res := do(t, "GET", url+"/_gos3/admin/v1/backings", nil, &rootCreds, nil)
	mustStatus(t, "list backings", res, 200)
	res.json(t, &out)
	m := map[string]multi.BackingStatus{}
	for _, b := range out.Backings {
		m[b.Name] = b
	}
	return m
}

// (a) storage.backend s3 with storage.path: objects, listings, multipart
// uploads and versioning; data and placements survive a restart.
func TestS3DefaultBacking(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "main")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendS3
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.S3 = s3Settings(remote, "main", "gos3/")
	srv := start(t, cfg)
	if srv.BackendName() != config.BackendS3 || srv.DataPath() == "" {
		t.Fatalf("backend %q, data path %q", srv.BackendName(), srv.DataPath())
	}
	mustStatus(t, "ready", do(t, "GET", srv.url+"/_gos3/ready", nil, nil, nil), 200)
	if b := backingStatuses(t, srv.url)["default"]; b.Kind != "s3" || b.Location != "s3://main/gos3/" || !b.Healthy || b.Path != "" {
		t.Fatalf("default backing: %+v", b)
	}

	mustStatus(t, "create bucket", do(t, "PUT", srv.url+"/photos", nil, &rootCreds, nil), 200)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/photos/a.txt", []byte("alpha"), &rootCreds, nil), 200)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/photos/dir/b.txt", []byte("beta"), &rootCreds, nil), 200)
	getBody(t, srv.url+"/photos/a.txt", "alpha")
	res := do(t, "GET", srv.url+"/photos?list-type=2", nil, &rootCreds, nil)
	mustStatus(t, "list", res, 200)
	if !bytes.Contains(res.body, []byte("<Key>a.txt</Key>")) || !bytes.Contains(res.body, []byte("<Key>dir/b.txt</Key>")) {
		t.Fatalf("listing: %s", res.body)
	}

	// Multipart upload: a 5 MiB part and a small one.
	ctx := context.Background()
	c := gos3Client(t, srv.url)
	id, err := c.CreateMultipartUpload(ctx, "photos", "big.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	part1 := bytes.Repeat([]byte("x"), 5<<20)
	part2 := []byte("tail")
	e1, err := c.UploadPart(ctx, "photos", "big.bin", id, 1, bytes.NewReader(part1), int64(len(part1)))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := c.UploadPart(ctx, "photos", "big.bin", id, 2, bytes.NewReader(part2), int64(len(part2)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, "photos", "big.bin", id,
		[]s3client.CompletedPart{{Number: 1, ETag: e1}, {Number: 2, ETag: e2}}, s3client.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	big := string(part1) + string(part2)
	getBody(t, srv.url+"/photos/big.bin", big)

	// Versioning.
	mustStatus(t, "enable versioning", do(t, "PUT", srv.url+"/photos?versioning", []byte(versioningEnabled), &rootCreds, nil), 200)
	mustStatus(t, "put v1", do(t, "PUT", srv.url+"/photos/v.txt", []byte("one"), &rootCreds, nil), 200)
	mustStatus(t, "put v2", do(t, "PUT", srv.url+"/photos/v.txt", []byte("two"), &rootCreds, nil), 200)
	res = do(t, "GET", srv.url+"/photos?versions&prefix=v.txt", nil, &rootCreds, nil)
	mustStatus(t, "list versions", res, 200)
	if n := bytes.Count(res.body, []byte("<Version>")); n != 2 {
		t.Fatalf("%d versions of v.txt, want 2: %s", n, res.body)
	}

	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	// Placements are kept in storage.path.
	data, err := os.ReadFile(filepath.Join(cfg.Storage.Path, config.SystemDirName, PlacementsFileName))
	if err != nil || !bytes.Contains(data, []byte(`"photos"`)) {
		t.Fatalf("placements: %v %s", err, data)
	}

	srv = start(t, cfg)
	getBody(t, srv.url+"/photos/a.txt", "alpha")
	getBody(t, srv.url+"/photos/big.bin", big)
	getBody(t, srv.url+"/photos/v.txt", "two")
	res = do(t, "GET", srv.url+"/photos?versions&prefix=v.txt", nil, &rootCreds, nil)
	if n := bytes.Count(res.body, []byte("<Version>")); res.status != 200 || n != 2 {
		t.Fatalf("versions after restart: %d %d %s", res.status, n, res.body)
	}
	var placement multi.PlacementStatus
	res = do(t, "GET", srv.url+"/_gos3/admin/v1/buckets/photos/placement", nil, &rootCreds, nil)
	mustStatus(t, "placement", res, 200)
	res.json(t, &placement)
	if placement.Primary != "default" {
		t.Fatalf("placement: %+v", placement)
	}
}

// (b) An fs default backing plus an s3 backing "cold" and a gateway
// backing "gw".
func TestS3AndGatewayBackings(t *testing.T) {
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

	st := backingStatuses(t, srv.url)
	if b := st["cold"]; b.Kind != "s3" || b.Mode != "" || b.Location != "s3://coldstore/cold/" || !b.Healthy {
		t.Fatalf("cold: %+v", b)
	}
	if b := st["gw"]; b.Kind != "s3" || b.Mode != "gateway" || !b.Healthy {
		t.Fatalf("gw: %+v", b)
	}
	res := do(t, "GET", srv.url+"/_gos3/admin/v1/whoami", nil, &rootCreds, nil)
	mustStatus(t, "whoami", res, 200)
	if !bytes.Contains(res.body, []byte(`{"name":"gw","kind":"s3","mode":"gateway"}`)) {
		t.Fatalf("whoami backings: %s", res.body)
	}

	for _, b := range []struct{ bucket, backing string }{{"local", ""}, {"archive", "cold"}, {"shared", "gw"}} {
		hdr := map[string]string{}
		if b.backing != "" {
			hdr["x-gos3-backing"] = b.backing
		}
		mustStatus(t, "create "+b.bucket, do(t, "PUT", srv.url+"/"+b.bucket, nil, &rootCreds, hdr), 200)
		want := b.backing
		if want == "" {
			want = "default"
		}
		res := do(t, "HEAD", srv.url+"/"+b.bucket, nil, &rootCreds, nil)
		if res.status != 200 || res.header.Get("x-gos3-backing") != want {
			t.Fatalf("HEAD %s: %d backing %q, want %q", b.bucket, res.status, res.header.Get("x-gos3-backing"), want)
		}
		mustStatus(t, "put", do(t, "PUT", srv.url+"/"+b.bucket+"/hello", []byte("hi "+b.bucket), &rootCreds, nil), 200)
		getBody(t, srv.url+"/"+b.bucket+"/hello", "hi "+b.bucket)
	}
	mustStatus(t, "unknown backing", do(t, "PUT", srv.url+"/nowhere", nil, &rootCreds, map[string]string{"x-gos3-backing": "nope"}), 400)

	// The gateway stores native objects in a remote bucket of the same name.
	info, err := remote.Backend.GetObjectInfo(context.Background(), "shared", "hello", "")
	if err != nil || info.Size != int64(len("hi shared")) {
		t.Fatalf("remote object: %+v %v", info, err)
	}
	// No versioning on gateway buckets; on s3 backings it works.
	mustStatus(t, "versioning on gw", do(t, "PUT", srv.url+"/shared?versioning", []byte(versioningEnabled), &rootCreds, nil), 501)
	mustStatus(t, "versioning on cold", do(t, "PUT", srv.url+"/archive?versioning", []byte(versioningEnabled), &rootCreds, nil), 200)

	// A gateway backing can be neither a replica nor a move target.
	place := func(bucket string, body any) response {
		raw, _ := json.Marshal(body)
		return do(t, "PUT", srv.url+"/_gos3/admin/v1/buckets/"+bucket+"/placement", raw, &rootCreds, nil)
	}
	mustStatus(t, "replica on gw", place("local", map[string]any{"mode": "active-passive", "replicas": []string{"gw"}}), 400)
	raw, _ := json.Marshal(map[string]string{"to": "gw"})
	mustStatus(t, "move to gw", do(t, "POST", srv.url+"/_gos3/admin/v1/buckets/local/placement/move", raw, &rootCreds, nil), 400)

	// An s3 backing can hold replicas.
	mustStatus(t, "replica on cold", place("local", map[string]any{"mode": "active-passive", "replicas": []string{"cold"}}), 200)
	waitFor(t, 30*time.Second, "replica in sync", func() bool {
		var ps multi.PlacementStatus
		res := do(t, "GET", srv.url+"/_gos3/admin/v1/buckets/local/placement", nil, &rootCreds, nil)
		if res.status != 200 {
			return false
		}
		res.json(t, &ps)
		for _, m := range ps.Members {
			if m.State != multi.StateInSync {
				return false
			}
		}
		return len(ps.Members) == 2
	})
}

// (c) and (d): an s3 backing added and edited through the admin API, and
// restored from backings.json after a restart.
func TestLiveS3Backing(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "live")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	srv := start(t, cfg)
	const endpoint = "/_gos3/admin/v1/backings/config"
	put := func(in multi.BackingConfiguration) response {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
	}
	get := func() (multi.BackingConfiguration, response) {
		var out multi.BackingConfiguration
		res := do(t, "GET", srv.url+endpoint, nil, &rootCreds, nil)
		mustStatus(t, "get backing configuration", res, 200)
		res.json(t, &out)
		return out, res
	}
	noSecret := func(what string, res response) {
		t.Helper()
		if bytes.Contains(res.body, []byte(s3clienttest.SecretKey)) {
			t.Fatalf("%s reveals the secret key: %s", what, res.body)
		}
	}
	def := func(name, prefix string) config.BackingDefinition {
		return config.BackingDefinition{Name: name, Backend: config.BackendS3, S3: &config.S3Definition{
			Endpoint: remote.URL, Bucket: "live", Prefix: prefix, Addressing: "path",
			AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
		}}
	}

	in, _ := get()
	// Settings the service refuses are reported, and nothing is saved.
	bad := def("cold", "cold/")
	bad.S3.SecretKey = "wrong-secret-key-000000000000000000000"
	mustStatus(t, "add with a wrong secret", put(multi.BackingConfiguration{
		BackingSettings: config.BackingSettings{Backings: []config.BackingDefinition{bad}, DefaultBacking: in.DefaultBacking},
		Revision:        in.Revision,
	}), 400)
	if _, err := os.Stat(filepath.Join(cfg.Storage.Path, config.SystemDirName, "backings.json")); err == nil {
		t.Fatal("backings.json written for a refused configuration")
	}
	in.Backings = append(in.Backings, def("cold", "cold/"))
	res := put(in)
	mustStatus(t, "add s3 backing", res, 200)
	noSecret("PUT response", res)
	in, res = get()
	noSecret("GET", res)
	if len(in.Backings) != 1 || in.Backings[0].S3 == nil || !in.Backings[0].S3.SecretKeyConfigured || in.Backings[0].S3.SecretKey != "" {
		t.Fatalf("configuration: %s", res.body)
	}
	backingsFile := filepath.Join(cfg.Storage.Path, config.SystemDirName, "backings.json")
	checkFile := func() []byte {
		t.Helper()
		fi, err := os.Stat(backingsFile)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Fatalf("backings.json mode %v", fi.Mode().Perm())
		}
		data, _ := os.ReadFile(backingsFile)
		return data
	}
	if data := checkFile(); !bytes.Contains(data, []byte(s3clienttest.SecretKey)) {
		t.Fatalf("backings.json lacks the secret key: %s", data)
	}
	if b := backingStatuses(t, srv.url)["cold"]; b.Kind != "s3" || !b.Healthy || b.Location != "s3://live/cold/" {
		t.Fatalf("cold: %+v", b)
	}

	// Put a bucket on it.
	mustStatus(t, "create", do(t, "PUT", srv.url+"/frozen", nil, &rootCreds, map[string]string{"x-gos3-backing": "cold"}), 200)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/frozen/k", []byte("ice"), &rootCreds, nil), 200)

	// A blank secret keeps the stored one; connection settings change in
	// place while the backing is in use (another name for the endpoint,
	// another part size).
	in.Backings[0].S3.Endpoint = strings.Replace(remote.URL, "127.0.0.1", "localhost", 1)
	in.Backings[0].S3.PartSize = 8 << 20
	mustStatus(t, "change connection settings", put(in), 200)
	if data := checkFile(); !bytes.Contains(data, []byte(s3clienttest.SecretKey)) || !bytes.Contains(data, []byte("localhost")) {
		t.Fatalf("backings.json after a blank secret: %s", data)
	}
	getBody(t, srv.url+"/frozen/k", "ice")
	mustStatus(t, "put after change", do(t, "PUT", srv.url+"/frozen/k2", []byte("snow"), &rootCreds, nil), 200)

	// Wrong credentials are refused and change nothing.
	in, _ = get()
	in.Backings[0].S3.SecretKey = "wrong-secret-key-000000000000000000000"
	mustStatus(t, "wrong secret", put(in), 400)
	getBody(t, srv.url+"/frozen/k", "ice")

	// Changing what data the backing holds while in use is a conflict.
	in, _ = get()
	in.Backings[0].S3.Prefix = "elsewhere/"
	mustStatus(t, "prefix change in use", put(in), 409)

	// An endpoint pointing at this server is refused.
	in, _ = get()
	self := def("loop", "loop/")
	self.S3.Endpoint = srv.url
	in.Backings = append(in.Backings, self)
	mustStatus(t, "endpoint on this server", put(in), 400)

	// A secret from the environment is used but not written to the file.
	t.Setenv(config.BackingEnv("envy", "secret_key"), s3clienttest.SecretKey)
	in, _ = get()
	envy := def("envy", "envy/")
	envy.S3.SecretKey = ""
	in.Backings = append(in.Backings, envy)
	mustStatus(t, "add backing with an env secret", put(in), 200)
	in, _ = get()
	if s := in.Backings[1].S3; s.SecretKeySource != config.SourceEnv || !s.SecretKeyConfigured {
		t.Fatalf("envy: %+v", s)
	}
	var saved config.BackingSettings
	if err := json.Unmarshal(checkFile(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Backings) != 2 || saved.Backings[1].S3.SecretKey != "" || saved.Backings[0].S3.SecretKey != s3clienttest.SecretKey {
		t.Fatalf("backings.json: %+v", saved)
	}
	mustStatus(t, "create on envy", do(t, "PUT", srv.url+"/breeze", nil, &rootCreds, map[string]string{"x-gos3-backing": "envy"}), 200)

	// A gateway backing added live: its connection settings change in
	// place while it holds no buckets, and not once it does.
	remote.CreateBucket(t, "gwlmeta")
	in, _ = get()
	gwl := def("gwl", "")
	gwl.S3.Bucket, gwl.S3.Gateway = "gwlmeta", true
	in.Backings = append(in.Backings, gwl)
	mustStatus(t, "add gateway backing", put(in), 200)
	in, _ = get()
	in.Backings[2].S3.Endpoint = strings.Replace(remote.URL, "127.0.0.1", "localhost", 1)
	mustStatus(t, "change an empty gateway backing", put(in), 200)
	mustStatus(t, "create on gwl", do(t, "PUT", srv.url+"/native", nil, &rootCreds, map[string]string{"x-gos3-backing": "gwl"}), 200)
	mustStatus(t, "put on gwl", do(t, "PUT", srv.url+"/native/obj", []byte("plain"), &rootCreds, nil), 200)
	getBody(t, srv.url+"/native/obj", "plain")
	if _, err := remote.Backend.GetObjectInfo(context.Background(), "native", "obj", ""); err != nil {
		t.Fatalf("gateway object on the remote: %v", err)
	}
	in, _ = get()
	in.Backings[2].S3.PartSize = 8 << 20
	mustStatus(t, "change a gateway backing in use", put(in), 409)

	// (d) After a restart backings.json is loaded again.
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	getBody(t, srv.url+"/frozen/k2", "snow")
	res = do(t, "HEAD", srv.url+"/breeze", nil, &rootCreds, nil)
	if res.status != 200 || res.header.Get("x-gos3-backing") != "envy" {
		t.Fatalf("HEAD breeze: %d %q", res.status, res.header.Get("x-gos3-backing"))
	}
	st := backingStatuses(t, srv.url)
	if !st["cold"].Healthy || !st["envy"].Healthy {
		t.Fatalf("backings after restart: %+v", st)
	}
	in, _ = get()
	getBody(t, srv.url+"/native/obj", "plain")
	if len(in.Backings) != 3 || in.Backings[1].S3.SecretKeySource != config.SourceEnv {
		t.Fatalf("configuration after restart: %+v", in)
	}
}

// (e) A server whose s3 backing's service is unreachable at startup
// starts; that backing's buckets answer 503 until the service is back,
// then they are served (and buckets without a placement adopted).
func TestS3BackingUnavailableAtStart(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Replication.HealthInterval = time.Second
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "coldstore", "")},
	}
	srv := start(t, cfg)
	mustStatus(t, "create local", do(t, "PUT", srv.url+"/local", nil, &rootCreds, nil), 200)
	for _, b := range []string{"placed", "unplaced"} {
		mustStatus(t, "create "+b, do(t, "PUT", srv.url+"/"+b, nil, &rootCreds, map[string]string{"x-gos3-backing": "cold"}), 200)
		mustStatus(t, "put", do(t, "PUT", srv.url+"/"+b+"/k", []byte("v-"+b), &rootCreds, nil), 200)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	// Forget the placement of "unplaced": it is adopted when cold is back.
	statePath := filepath.Join(cfg.Storage.Path, config.SystemDirName, PlacementsFileName)
	var state map[string]json.RawMessage
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	var buckets map[string]json.RawMessage
	if err := json.Unmarshal(state["buckets"], &buckets); err != nil {
		t.Fatal(err)
	}
	delete(buckets, "unplaced")
	state["buckets"], _ = json.Marshal(buckets)
	data, _ = json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	down := func(*http.Request) int { return http.StatusServiceUnavailable }
	remote.Fail.Store(&down)
	srv = start(t, cfg)
	if b := backingStatuses(t, srv.url)["cold"]; b.Healthy || b.LastError == "" {
		t.Fatalf("cold while down: %+v", b)
	}
	mustStatus(t, "get from cold while down", do(t, "GET", srv.url+"/placed/k", nil, &rootCreds, nil), 503)
	mustStatus(t, "ready (the default backing is fine)", do(t, "GET", srv.url+"/_gos3/ready", nil, nil, nil), 200)
	res := do(t, "GET", srv.url+"/", nil, &rootCreds, nil)
	mustStatus(t, "list buckets", res, 200)
	if !bytes.Contains(res.body, []byte("<Name>local</Name>")) {
		t.Fatalf("buckets while cold is down: %s", res.body)
	}
	mustStatus(t, "list a bucket on the default backing", do(t, "GET", srv.url+"/local?list-type=2", nil, &rootCreds, nil), 200)
	remote.Fail.Store(nil)
	waitFor(t, 60*time.Second, "cold healthy", func() bool { return backingStatuses(t, srv.url)["cold"].Healthy })
	getBody(t, srv.url+"/placed/k", "v-placed")
	waitFor(t, 10*time.Second, "unplaced adopted", func() bool {
		return do(t, "GET", srv.url+"/unplaced/k", nil, &rootCreds, nil).status == 200
	})
	getBody(t, srv.url+"/unplaced/k", "v-unplaced")
}

// (f) An s3 backing configured through the environment only:
// GOS3_STORAGE_BACKINGS=cold=s3 plus GOS3_BACKING_COLD_*.
func TestS3BackingFromEnvironment(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "envstore")
	dir := t.TempDir()
	for k, v := range map[string]string{
		"GOS3_ADDRESS":                 "127.0.0.1:0",
		"GOS3_DATA":                    dir,
		"GOS3_STORAGE_BACKEND":         "fs",
		"GOS3_STORAGE_BACKINGS":        "cold=s3",
		"GOS3_ROOT_ACCESS_KEY":         testAccessKey,
		"GOS3_ROOT_SECRET_KEY":         testSecretKey,
		"GOS3_BACKING_COLD_ENDPOINT":   remote.URL,
		"GOS3_BACKING_COLD_BUCKET":     "envstore",
		"GOS3_BACKING_COLD_PREFIX":     "gos3",
		"GOS3_BACKING_COLD_ADDRESSING": "path",
		"GOS3_BACKING_COLD_ACCESS_KEY": s3clienttest.AccessKey,
		"GOS3_BACKING_COLD_SECRET_KEY": s3clienttest.SecretKey,
		"GOS3_BACKING_COLD_PART_SIZE":  "8MiB",
		"GOS3_BACKING_COLD_GATEWAY":    "false",
		"GOS3_BACKING_COLD_REGION":     "us-east-1",
		"GOS3_DEFAULT_BACKING":         "cold",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Discovery.Enabled = false
	cfg.Logging.AccessLog = false
	srv := start(t, cfg)
	b := backingStatuses(t, srv.url)["cold"]
	if b.Kind != "s3" || !b.Healthy || b.Location != "s3://envstore/gos3/" {
		t.Fatalf("cold: %+v", b)
	}
	// The default for new buckets is cold.
	mustStatus(t, "create", do(t, "PUT", srv.url+"/fromenv", nil, &rootCreds, nil), 200)
	res := do(t, "HEAD", srv.url+"/fromenv", nil, &rootCreds, nil)
	if res.header.Get("x-gos3-backing") != "cold" {
		t.Fatalf("HEAD: %d %q", res.status, res.header.Get("x-gos3-backing"))
	}
	mustStatus(t, "put", do(t, "PUT", srv.url+"/fromenv/k", []byte("env"), &rootCreds, nil), 200)
	// The data is in the remote bucket under the prefix.
	var keys []string
	if err := remote.Backend.WalkObjects(context.Background(), "envstore", "gos3/", "", func(o storage.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	}); err != nil || len(keys) == 0 {
		t.Fatalf("remote keys under gos3/: %v %v", keys, err)
	}
	// The env-sourced secret is shown as such and never written anywhere.
	var conf multi.BackingConfiguration
	res = do(t, "GET", srv.url+"/_gos3/admin/v1/backings/config", nil, &rootCreds, nil)
	mustStatus(t, "get configuration", res, 200)
	res.json(t, &conf)
	if s := conf.Backings[0].S3; s == nil || s.SecretKeySource != config.SourceEnv || s.Endpoint != remote.URL {
		t.Fatalf("configuration: %s", res.body)
	}
}

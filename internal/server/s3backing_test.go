package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
// (s3clienttest): the default backing on s3, additional s3 backings with
// and without a home holding native and wrapped buckets, live
// configuration, restarts and an unreachable service.

// s3Settings returns the settings of an s3 backing on remote with its home
// at homeBucket/homePrefix ("" = no home: records in the system
// directory).
func s3Settings(remote *s3clienttest.Remote, homeBucket, homePrefix string) *config.S3Config {
	return &config.S3Config{
		Endpoint:   remote.URL,
		HomeBucket: homeBucket,
		HomePrefix: homePrefix,
		Addressing: "path",
		AccessKey:  s3clienttest.AccessKey,
		SecretKey:  s3clienttest.SecretKey,
	}
}

// s3Definition returns the admin API definition of an s3 backing on remote
// (see s3Settings).
func s3Definition(remote *s3clienttest.Remote, name, homeBucket, homePrefix string) config.BackingDefinition {
	return config.BackingDefinition{Name: name, Backend: config.BackendS3, S3: &config.S3Definition{
		Endpoint: remote.URL, HomeBucket: homeBucket, HomePrefix: homePrefix, Addressing: "path",
		AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
	}}
}

// testKEK returns a random key-encryption key in configuration form.
func testKEK(t *testing.T) string {
	return base64.StdEncoding.EncodeToString(incompressible(t, config.KEKSize))
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

// mustStatusIn fails unless res has one of the statuses want.
func mustStatusIn(t *testing.T, what string, res response, want ...int) {
	t.Helper()
	if !slices.Contains(want, res.status) {
		t.Fatalf("%s: got %d, want one of %v: %s", what, res.status, want, res.body)
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

// adminDo sends an admin API request with a JSON body (nil: none).
func adminDo(t *testing.T, url, method, path string, in any) response {
	t.Helper()
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			t.Fatal(err)
		}
	}
	return do(t, method, url+"/_gos3/admin/v1"+path, body, &rootCreds, nil)
}

// createOn creates bucket on backing with the extra CreateBucket headers
// hdr (may be nil) and fails unless it answers 200.
func createOn(t *testing.T, url, bucket, backing string, hdr map[string]string) {
	t.Helper()
	h := map[string]string{}
	for k, v := range hdr {
		h[k] = v
	}
	if backing != "" {
		h["x-gos3-backing"] = backing
	}
	mustStatus(t, "create "+bucket, do(t, "PUT", url+"/"+bucket, nil, &rootCreds, h), 200)
}

// headBucket checks a bucket's backing and kind headers (kind "" = none).
func headBucket(t *testing.T, url, bucket, backing, kind string) {
	t.Helper()
	res := do(t, "HEAD", url+"/"+bucket, nil, &rootCreds, nil)
	if res.status != 200 || res.header.Get("x-gos3-backing") != backing || res.header.Get("x-gos3-bucket-kind") != kind {
		t.Fatalf("HEAD %s: %d backing %q kind %q, want %q %q", bucket, res.status,
			res.header.Get("x-gos3-backing"), res.header.Get("x-gos3-bucket-kind"), backing, kind)
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

// remoteVersioning returns the versioning status of the remote's bucket.
func remoteVersioning(t *testing.T, remote *s3clienttest.Remote, bucket string) storage.VersioningStatus {
	t.Helper()
	info, err := remote.Backend.GetBucket(context.Background(), bucket)
	if err != nil {
		t.Fatalf("remote bucket %s: %v", bucket, err)
	}
	return info.Versioning
}

// remoteHas reports whether the remote has bucket.
func remoteHas(remote *s3clienttest.Remote, bucket string) bool {
	_, err := remote.Backend.GetBucket(context.Background(), bucket)
	return err == nil
}

// waitPlacement waits until the placement of bucket satisfies ok with no
// move in progress and every member in sync.
func waitPlacement(t *testing.T, url, bucket string, ok func(multi.PlacementStatus) bool) multi.PlacementStatus {
	t.Helper()
	var ps multi.PlacementStatus
	waitFor(t, 60*time.Second, "placement of "+bucket, func() bool {
		ps = multi.PlacementStatus{}
		res := do(t, "GET", url+"/_gos3/admin/v1/buckets/"+bucket+"/placement", nil, &rootCreds, nil)
		if res.status != 200 {
			return false
		}
		res.json(t, &ps)
		if ps.Move != nil || !ok(ps) {
			return false
		}
		for _, m := range ps.Members {
			if m.State != multi.StateInSync {
				return false
			}
		}
		return true
	})
	return ps
}

// (a) storage.backend s3 with a home and storage.path: objects, listings,
// multipart uploads and versioning in wrapped buckets of the home; data and
// placements survive a restart.
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
	if b := backingStatuses(t, srv.url)["default"]; b.Kind != "s3" || b.Location != "s3://main/gos3/" || !b.Healthy || b.Path != "" ||
		b.DefaultKind != "wrapped" || !slices.Equal(b.Kinds, []string{"wrapped", "native"}) {
		t.Fatalf("default backing: %+v", b)
	}

	mustStatus(t, "create bucket", do(t, "PUT", srv.url+"/photos", nil, &rootCreds, nil), 200)
	headBucket(t, srv.url, "photos", "default", "wrapped")
	mustStatus(t, "put", do(t, "PUT", srv.url+"/photos/a.txt", []byte("alpha"), &rootCreds, nil), 200)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/photos/dir/b.txt", []byte("beta"), &rootCreds, nil), 200)
	getBody(t, srv.url+"/photos/a.txt", "alpha")
	res := do(t, "GET", srv.url+"/photos?list-type=2", nil, &rootCreds, nil)
	mustStatus(t, "list", res, 200)
	if !bytes.Contains(res.body, []byte("<Key>a.txt</Key>")) || !bytes.Contains(res.body, []byte("<Key>dir/b.txt</Key>")) {
		t.Fatalf("listing: %s", res.body)
	}
	// Wrapped buckets live in the home, never in remote buckets of their own.
	if remoteHas(remote, "photos") {
		t.Fatal("a wrapped bucket created a remote bucket")
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
	headBucket(t, srv.url, "photos", "default", "wrapped")
}

// (b) An fs default backing plus the s3 backings "cloud" (a home, wrapped
// buckets by default, a key-encryption key) and "edge" (no home: records
// in the system directory, native buckets by default) on one service.
func TestS3AccountBackings(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "cloudhome")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cloud := s3Settings(remote, "cloudhome", "gos3/")
	cloud.EncryptionKey = testKEK(t)
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cloud", Backend: config.BackendS3, S3: cloud},
		{Name: "edge", Backend: config.BackendS3, S3: s3Settings(remote, "", "")},
	}
	srv := start(t, cfg)

	st := backingStatuses(t, srv.url)
	if b := st["cloud"]; b.Kind != "s3" || b.Location != "s3://cloudhome/gos3/" || !b.Healthy || b.DefaultKind != "wrapped" ||
		!slices.Equal(b.Kinds, []string{"wrapped", "native"}) {
		t.Fatalf("cloud: %+v", b)
	}
	if b := st["edge"]; b.Kind != "s3" || b.Location != "local:" || !b.Healthy || b.DefaultKind != "native" ||
		!slices.Equal(b.Kinds, []string{"native"}) {
		t.Fatalf("edge: %+v", b)
	}
	// edge keeps its records in <system dir>/s3/edge.
	records := S3RecordsDir(filepath.Join(cfg.Storage.Path, config.SystemDirName), "edge")
	if _, err := os.Stat(filepath.Join(records, "state.db")); err != nil {
		t.Fatalf("edge's records: %v", err)
	}
	if dir, err := os.Stat(records); err != nil || (runtime.GOOS != "windows" && dir.Mode().Perm() != 0o700) {
		t.Fatalf("edge's records directory: %v", err)
	}
	res := do(t, "GET", srv.url+"/_gos3/admin/v1/whoami", nil, &rootCreds, nil)
	mustStatus(t, "whoami", res, 200)
	for _, want := range []string{`"name":"cloud","kind":"s3","kinds":["wrapped","native"],"defaultKind":"wrapped"`,
		`"name":"edge","kind":"s3","kinds":["native"],"defaultKind":"native"`} {
		if !bytes.Contains(res.body, []byte(want)) {
			t.Fatalf("whoami backings lack %s: %s", want, res.body)
		}
	}

	// One bucket of each default kind, plus a native bucket on cloud.
	for _, b := range []struct{ bucket, backing, kind string }{
		{"local", "", ""}, {"archive", "cloud", "wrapped"}, {"photos", "edge", "native"},
	} {
		createOn(t, srv.url, b.bucket, b.backing, nil)
		want := b.backing
		if want == "" {
			want = "default"
		}
		headBucket(t, srv.url, b.bucket, want, b.kind)
		mustStatus(t, "put", do(t, "PUT", srv.url+"/"+b.bucket+"/hello", []byte("hi "+b.bucket), &rootCreds, nil), 200)
		getBody(t, srv.url+"/"+b.bucket+"/hello", "hi "+b.bucket)
	}
	createOn(t, srv.url, "nat2", "cloud", map[string]string{"x-gos3-bucket-kind": "native"})
	headBucket(t, srv.url, "nat2", "cloud", "native")
	createOn(t, srv.url, "aliased", "edge", map[string]string{"x-gos3-remote-bucket": "edge-remote"})
	headBucket(t, srv.url, "aliased", "edge", "native")
	mustStatus(t, "put aliased", do(t, "PUT", srv.url+"/aliased/k", []byte("via alias"), &rootCreds, nil), 200)
	// A wrapped bucket on edge needs a container; edge has none.
	mustStatus(t, "wrapped on edge", do(t, "PUT", srv.url+"/boxed", nil, &rootCreds,
		map[string]string{"x-gos3-backing": "edge", "x-gos3-bucket-kind": "wrapped"}), 400)
	mustStatus(t, "unknown backing", do(t, "PUT", srv.url+"/nowhere", nil, &rootCreds, map[string]string{"x-gos3-backing": "nope"}), 400)

	// Native buckets are remote buckets holding the objects as-is; wrapped
	// buckets are not.
	ctx := context.Background()
	for bucket, key := range map[string]string{"photos": "hello", "nat2": "", "edge-remote": "k"} {
		if !remoteHas(remote, bucket) {
			t.Fatalf("no remote bucket %s", bucket)
		}
		if key == "" {
			continue
		}
		if _, err := remote.Backend.GetObjectInfo(ctx, bucket, key, ""); err != nil {
			t.Fatalf("remote object %s/%s: %v", bucket, key, err)
		}
	}
	if remoteHas(remote, "archive") || remoteHas(remote, "aliased") {
		t.Fatal("a gos3 name became a remote bucket")
	}

	// Versioning of a native bucket is the remote bucket's.
	mustStatus(t, "versioning on aliased", do(t, "PUT", srv.url+"/aliased?versioning", []byte(versioningEnabled), &rootCreds, nil), 200)
	if v := remoteVersioning(t, remote, "edge-remote"); v != storage.VersioningEnabled {
		t.Fatalf("remote versioning of edge-remote: %q", v)
	}
	mustStatus(t, "put v2", do(t, "PUT", srv.url+"/aliased/k", []byte("again"), &rootCreds, nil), 200)
	res = do(t, "GET", srv.url+"/aliased?versions&prefix=k", nil, &rootCreds, nil)
	if n := bytes.Count(res.body, []byte("<Version>")); res.status != 200 || n != 2 {
		t.Fatalf("versions of aliased/k: %d %d %s", res.status, n, res.body)
	}
	if got := versionList(t, remote.Client(t), "edge-remote", "k"); len(got) != 2 {
		t.Fatalf("remote versions of edge-remote/k: %v", got)
	}
	mustStatus(t, "versioning on archive", do(t, "PUT", srv.url+"/archive?versioning", []byte(versioningEnabled), &rootCreds, nil), 200)

	// AES256 needs the KEK and a wrapped bucket.
	if doc := getProvider(t, srv.url, "archive"); !slices.Contains(doc.Available, storage.EncryptionAES256) {
		t.Fatalf("providers of archive: %+v", doc)
	}
	if doc := getProvider(t, srv.url, "photos"); len(doc.Available) != 0 {
		t.Fatalf("providers of a native bucket: %+v", doc)
	}
	mustStatus(t, "AES256 provider on archive", putProvider(t, srv.url, "archive", storage.EncryptionAES256), 200)
	mustStatusIn(t, "AES256 provider on photos", putProvider(t, srv.url, "photos", storage.EncryptionAES256), 400)
	sse := map[string]string{"x-amz-server-side-encryption": "AES256"}
	mustStatus(t, "SSE put on archive", do(t, "PUT", srv.url+"/archive/sse", []byte("sealed"), &rootCreds, sse), 200)
	getBody(t, srv.url+"/archive/sse", "sealed")
	mustStatusIn(t, "SSE put on photos", do(t, "PUT", srv.url+"/photos/sse", []byte("x"), &rootCreds, sse), 400, 501)

	// A native bucket can be neither replicated nor moved (its versions are
	// the remote's).
	place := func(bucket string, body any) response {
		return adminDo(t, srv.url, "PUT", "/buckets/"+bucket+"/placement", body)
	}
	move := func(bucket string, body any) response {
		return adminDo(t, srv.url, "POST", "/buckets/"+bucket+"/placement/move", body)
	}
	mustStatusIn(t, "replica of a native bucket", place("photos", map[string]any{"mode": "active-passive", "replicas": []string{"cloud"}}), 400, 409)
	mustStatusIn(t, "move of a native bucket", move("photos", map[string]string{"to": "cloud"}), 400, 409)
	// Copies on edge would be wrapped buckets, and edge has no container.
	mustStatus(t, "move to edge", move("local", map[string]string{"to": "edge"}), 400)

	// A wrapped copy: an fs bucket moves to cloud, into a dedicated
	// container (a remote bucket gos3 creates).
	var ci struct {
		ID       string `json:"id"`
		Location struct {
			Bucket string `json:"bucket"`
		} `json:"location"`
		State string `json:"state"`
	}
	res = adminDo(t, srv.url, "POST", "/backings/cloud/containers", map[string]any{"bucket": "cloudbox", "createBucket": true, "label": "box"})
	mustStatus(t, "add container", res, 201)
	res.json(t, &ci)
	if ci.ID == "" || ci.Location.Bucket != "cloudbox" || ci.State != "ready" {
		t.Fatalf("container: %s", res.body)
	}
	mustStatus(t, "move local to cloud", move("local", map[string]string{"to": "cloud", "container": ci.ID}), 202)
	waitPlacement(t, srv.url, "local", func(ps multi.PlacementStatus) bool { return ps.Primary == "cloud" })
	headBucket(t, srv.url, "local", "cloud", "wrapped")
	getBody(t, srv.url+"/local/hello", "hi local")
	if _, err := remote.Backend.GetObjectInfo(ctx, "cloudbox", "gos3.json", ""); err != nil {
		t.Fatalf("the container's marker: %v", err)
	}
	// An s3 backing (wrapped buckets) can hold replicas.
	createOn(t, srv.url, "mirrored", "", nil)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/mirrored/k", []byte("twice"), &rootCreds, nil), 200)
	mustStatus(t, "replica on cloud", place("mirrored", map[string]any{"mode": "active-passive", "replicas": []string{"cloud"}}), 200)
	waitPlacement(t, srv.url, "mirrored", func(ps multi.PlacementStatus) bool { return len(ps.Members) == 2 })

	// Everything survives a restart.
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	for bucket, want := range map[string]string{"archive/hello": "hi archive", "photos/hello": "hi photos", "aliased/k": "again",
		"local/hello": "hi local", "archive/sse": "sealed", "mirrored/k": "twice"} {
		getBody(t, srv.url+"/"+bucket, want)
	}
	headBucket(t, srv.url, "photos", "edge", "native")
	headBucket(t, srv.url, "local", "cloud", "wrapped")
}

// (d) s3 backings added and edited through the admin API, and restored from
// backings.json after a restart.
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
	const wrongSecret = "wrong-secret-key-000000000000000000000"
	backingsFile := filepath.Join(cfg.Storage.Path, config.SystemDirName, "backings.json")

	in, _ := get()
	// Settings the service refuses are reported, and nothing is saved.
	bad := s3Definition(remote, "cold", "live", "cold/")
	bad.S3.SecretKey = wrongSecret
	mustStatus(t, "add with a wrong secret", put(multi.BackingConfiguration{
		BackingSettings: config.BackingSettings{Backings: []config.BackingDefinition{bad}, DefaultBacking: in.DefaultBacking},
		Revision:        in.Revision,
	}), 400)
	if _, err := os.Stat(backingsFile); err == nil {
		t.Fatal("backings.json written for a refused configuration")
	}
	in.Backings = append(in.Backings, s3Definition(remote, "cold", "live", "cold/"))
	res := put(in)
	mustStatus(t, "add s3 backing", res, 200)
	noSecret("PUT response", res)
	in, res = get()
	noSecret("GET", res)
	if len(in.Backings) != 1 || in.Backings[0].S3 == nil || !in.Backings[0].S3.SecretKeyConfigured || in.Backings[0].S3.SecretKey != "" ||
		in.Backings[0].S3.HomeBucket != "live" || in.Backings[0].S3.DefaultKind != "wrapped" {
		t.Fatalf("configuration: %s", res.body)
	}
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
	if b := backingStatuses(t, srv.url)["cold"]; b.Kind != "s3" || !b.Healthy || b.Location != "s3://live/cold/" || b.DefaultKind != "wrapped" {
		t.Fatalf("cold: %+v", b)
	}

	// Put a bucket on it.
	createOn(t, srv.url, "frozen", "cold", nil)
	headBucket(t, srv.url, "frozen", "cold", "wrapped")
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
	in.Backings[0].S3.SecretKey = wrongSecret
	mustStatus(t, "wrong secret", put(in), 400)
	getBody(t, srv.url+"/frozen/k", "ice")

	// The default kind changes in place, while in use: new buckets are
	// native, existing ones keep their kind.
	in, _ = get()
	in.Backings[0].S3.DefaultKind = "native"
	mustStatus(t, "default kind native", put(in), 200)
	if b := backingStatuses(t, srv.url)["cold"]; b.DefaultKind != "native" || !b.Healthy {
		t.Fatalf("cold after the default kind changed: %+v", b)
	}
	createOn(t, srv.url, "thawed", "cold", nil)
	headBucket(t, srv.url, "thawed", "cold", "native")
	headBucket(t, srv.url, "frozen", "cold", "wrapped")
	if !remoteHas(remote, "thawed") {
		t.Fatal("the native bucket has no remote bucket")
	}

	// Changing what data the backing holds (its home) while in use is a
	// conflict.
	in, _ = get()
	in.Backings[0].S3.HomePrefix = "elsewhere/"
	mustStatus(t, "home change in use", put(in), 409)
	in, _ = get()
	in.Backings[0].S3.HomeBucket = ""
	in.Backings[0].S3.HomePrefix = ""
	mustStatus(t, "home removed in use", put(in), 409)

	// An endpoint pointing at this server is refused.
	in, _ = get()
	self := s3Definition(remote, "loop", "loop", "")
	self.S3.Endpoint = srv.url
	in.Backings = append(in.Backings, self)
	mustStatus(t, "endpoint on this server", put(in), 400)

	// Another backing on cold's home is refused (the location is claimed).
	in, _ = get()
	in.Backings = append(in.Backings, s3Definition(remote, "twin", "live", "cold/sub/"))
	mustStatusIn(t, "a home inside cold's", put(in), 400, 409)

	// A secret from the environment is used but not written to the file.
	t.Setenv(config.BackingEnv("envy", "secret_key"), s3clienttest.SecretKey)
	in, _ = get()
	envy := s3Definition(remote, "envy", "live", "envy/")
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
	createOn(t, srv.url, "breeze", "envy", nil)

	// A backing without a home: its credentials are checked when it is
	// added (it loads without the service), and it holds native buckets.
	in, _ = get()
	edge := s3Definition(remote, "edge", "", "")
	edge.S3.SecretKey = wrongSecret
	in.Backings = append(in.Backings, edge)
	mustStatus(t, "add a backing without a home with a wrong secret", put(in), 400)
	in.Backings[len(in.Backings)-1].S3.SecretKey = s3clienttest.SecretKey
	mustStatus(t, "add a backing without a home", put(in), 200)
	if b := backingStatuses(t, srv.url)["edge"]; b.Location != "local:" || b.DefaultKind != "native" || !b.Healthy {
		t.Fatalf("edge: %+v", b)
	}
	createOn(t, srv.url, "native", "edge", nil)
	mustStatus(t, "put on edge", do(t, "PUT", srv.url+"/native/obj", []byte("plain"), &rootCreds, nil), 200)
	if _, err := remote.Backend.GetObjectInfo(context.Background(), "native", "obj", ""); err != nil {
		t.Fatalf("native object on the remote: %v", err)
	}
	// Its connection changes in place while it holds buckets.
	in, _ = get()
	in.Backings[2].S3.Endpoint = strings.Replace(remote.URL, "127.0.0.1", "localhost", 1)
	in.Backings[2].S3.PartSize = 8 << 20
	mustStatus(t, "change edge's connection", put(in), 200)
	getBody(t, srv.url+"/native/obj", "plain")
	in, _ = get()
	in.Backings[2].S3.SecretKey = wrongSecret
	mustStatus(t, "wrong secret on edge", put(in), 400)

	// After a restart backings.json is loaded again.
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	getBody(t, srv.url+"/frozen/k2", "snow")
	headBucket(t, srv.url, "breeze", "envy", "wrapped")
	headBucket(t, srv.url, "thawed", "cold", "native")
	st := backingStatuses(t, srv.url)
	if !st["cold"].Healthy || !st["envy"].Healthy || !st["edge"].Healthy || st["cold"].DefaultKind != "native" {
		t.Fatalf("backings after restart: %+v", st)
	}
	in, _ = get()
	getBody(t, srv.url+"/native/obj", "plain")
	if len(in.Backings) != 3 || in.Backings[1].S3.SecretKeySource != config.SourceEnv || in.Backings[0].S3.DefaultKind != "native" {
		t.Fatalf("configuration after restart: %+v", in)
	}
}

// (g) A server whose s3 backing's service is unreachable at startup
// starts; that backing's buckets answer 503 until the service is back,
// then they are served (and buckets without a placement adopted). Other
// backings are not affected.
func TestS3BackingUnavailableAtStart(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Replication.HealthInterval = time.Second
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "coldstore", "")},
		{Name: "edge", Backend: config.BackendS3, S3: s3Settings(remote, "", "")},
	}
	srv := start(t, cfg)
	mustStatus(t, "create local", do(t, "PUT", srv.url+"/local", nil, &rootCreds, nil), 200)
	for _, b := range []string{"placed", "unplaced"} {
		createOn(t, srv.url, b, "cold", nil)
		mustStatus(t, "put", do(t, "PUT", srv.url+"/"+b+"/k", []byte("v-"+b), &rootCreds, nil), 200)
	}
	createOn(t, srv.url, "nearby", "edge", nil)
	mustStatus(t, "put", do(t, "PUT", srv.url+"/nearby/k", []byte("v-nearby"), &rootCreds, nil), 200)
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
	mustStatus(t, "get from edge while down", do(t, "GET", srv.url+"/nearby/k", nil, &rootCreds, nil), 503)
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
	waitFor(t, 60*time.Second, "edge healthy", func() bool { return backingStatuses(t, srv.url)["edge"].Healthy })
	getBody(t, srv.url+"/nearby/k", "v-nearby")
}

// An s3 backing configured through the environment only:
// GOS3_STORAGE_BACKINGS=cold=s3 plus GOS3_BACKING_COLD_*.
func TestS3BackingFromEnvironment(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "envstore")
	dir := t.TempDir()
	for k, v := range map[string]string{
		"GOS3_ADDRESS":                   "127.0.0.1:0",
		"GOS3_DATA":                      dir,
		"GOS3_STORAGE_BACKEND":           "fs",
		"GOS3_STORAGE_BACKINGS":          "cold=s3",
		"GOS3_ROOT_ACCESS_KEY":           testAccessKey,
		"GOS3_ROOT_SECRET_KEY":           testSecretKey,
		"GOS3_BACKING_COLD_ENDPOINT":     remote.URL,
		"GOS3_BACKING_COLD_HOME_BUCKET":  "envstore",
		"GOS3_BACKING_COLD_HOME_PREFIX":  "gos3",
		"GOS3_BACKING_COLD_ADDRESSING":   "path",
		"GOS3_BACKING_COLD_ACCESS_KEY":   s3clienttest.AccessKey,
		"GOS3_BACKING_COLD_SECRET_KEY":   s3clienttest.SecretKey,
		"GOS3_BACKING_COLD_PART_SIZE":    "8MiB",
		"GOS3_BACKING_COLD_DEFAULT_KIND": "wrapped",
		"GOS3_BACKING_COLD_REGION":       "us-east-1",
		"GOS3_DEFAULT_BACKING":           "cold",
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
	if b.Kind != "s3" || !b.Healthy || b.Location != "s3://envstore/gos3/" || b.DefaultKind != "wrapped" {
		t.Fatalf("cold: %+v", b)
	}
	// The default for new buckets is cold.
	mustStatus(t, "create", do(t, "PUT", srv.url+"/fromenv", nil, &rootCreds, nil), 200)
	headBucket(t, srv.url, "fromenv", "cold", "wrapped")
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
	res := do(t, "GET", srv.url+"/_gos3/admin/v1/backings/config", nil, &rootCreds, nil)
	mustStatus(t, "get configuration", res, 200)
	res.json(t, &conf)
	if s := conf.Backings[0].S3; s == nil || s.SecretKeySource != config.SourceEnv || s.Endpoint != remote.URL || s.HomeBucket != "envstore" {
		t.Fatalf("configuration: %s", res.body)
	}
}

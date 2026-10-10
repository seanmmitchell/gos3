package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
)

// startPrivate starts a server for one test and stops it at cleanup.
func startPrivate(t *testing.T, cfg config.Config) *env {
	t.Helper()
	e, err := startEnv(t.Name(), cfg)
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() {
		if err := e.stop(); err != nil {
			t.Errorf("stopping server: %v", err)
		}
	})
	return e
}

// rootOf returns the server's (possibly generated) root credentials.
func rootOf(e *env) auth.Credentials {
	ak, sk := e.srv.RootCredentials()
	return auth.Credentials{AccessKey: ak, SecretKey: sk}
}

// TestRestartPersistence stops an fs server and starts a new one on the
// same data directory: buckets, objects, versions, delete markers, pending
// multipart uploads, bucket configurations, IAM users/policies/keys,
// runtime settings, the server ID and generated root credentials survive.
func TestRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = dir
	cfg.Storage.FsyncData = true
	// Let the server generate and persist its root credentials.
	cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey = "", ""

	e1 := startPrivate(t, cfg)
	root := rootOf(e1)
	if root.AccessKey == "" || root.SecretKey == "" || e1.srv.CredentialsSource() == "" {
		t.Fatalf("no generated credentials (%q)", e1.srv.CredentialsSource())
	}
	if fi, err := os.Stat(filepath.Join(dir, ".gos3", config.RootCredentialsFileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("root credentials file: %v %v", fi, err)
	}
	serverID := e1.srv.ServerID()
	c1 := e1.s3Client(root)
	b := "persist-" + strings.ToLower(serverID[:8])
	_, err := c1.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(b)})
	must(t, err, "CreateBucket")
	_, err = c1.PutBucketVersioning(ctx(t), &s3.PutBucketVersioningInput{Bucket: aws.String(b),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled}})
	must(t, err, "PutBucketVersioning")
	putObj := func(key string, data []byte, opts ...func(*s3.PutObjectInput)) *s3.PutObjectOutput {
		in := &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), Body: bytes.NewReader(data)}
		for _, o := range opts {
			o(in)
		}
		out, err := c1.PutObject(ctx(t), in)
		must(t, err, "PutObject "+key)
		return out
	}
	big := randomData(3<<20, 50)
	v1 := putObj("doc", []byte("v1"), func(in *s3.PutObjectInput) {
		in.Metadata = map[string]string{"rev": "1"}
		in.Tagging = aws.String("t=1")
	})
	v2 := putObj("doc", []byte("v2"))
	putObj("big.bin", big, func(in *s3.PutObjectInput) { in.ChecksumAlgorithm = s3types.ChecksumAlgorithmCrc32c })
	putObj("gone", []byte("x"))
	del, err := c1.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("gone")})
	must(t, err, "DeleteObject")
	cr, err := c1.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("pending")})
	must(t, err, "CreateMultipartUpload")
	part1 := randomData(5<<20, 51)
	up1, err := c1.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("pending"), UploadId: cr.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(part1)})
	must(t, err, "UploadPart")
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::` + b + `/doc"}]}`
	_, err = c1.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(policy)})
	must(t, err, "PutBucketPolicy")
	_, err = c1.PutBucketTagging(ctx(t), &s3.PutBucketTaggingInput{Bucket: aws.String(b), Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("team"), Value: aws.String("e2e")}}}})
	must(t, err, "PutBucketTagging")

	// IAM: custom policy + user with key.
	pdoc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::` + b + `","arn:aws:s3:::` + b + `/*"]}]}`
	if r := e1.admin(t, root, http.MethodPut, "/policies/persisted-read", map[string]any{"document": json.RawMessage(pdoc)}, nil); r.status != 200 {
		t.Fatalf("PUT policy: %d %s", r.status, r.body)
	}
	var created struct {
		Key adminKey `json:"key"`
	}
	if r := e1.admin(t, root, http.MethodPost, "/users", map[string]any{"name": "persisted", "policies": []string{"persisted-read"}, "createKey": true}, &created); r.status != 201 {
		t.Fatalf("POST user: %d %s", r.status, r.body)
	}
	user := auth.Credentials{AccessKey: created.Key.AccessKey, SecretKey: created.Key.SecretKey}

	// Runtime settings through the admin API.
	var st config.Settings
	if r := e1.admin(t, root, http.MethodGet, "/config", nil, &st); r.status != 200 {
		t.Fatalf("GET /config: %d %s", r.status, r.body)
	}
	st.Name = "renamed-e2e"
	st.Domains = []string{"persist.test"}
	st.CORS.AllowedOrigins = []string{"https://ui.example.com"}
	st.Lifecycle.IntervalSeconds = 7200
	var updated config.Settings
	if r := e1.admin(t, root, http.MethodPut, "/config", st, &updated); r.status != 200 || updated.Name != "renamed-e2e" {
		t.Fatalf("PUT /config: %d %s", r.status, r.body)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".gos3", config.SettingsFileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", fi, err)
	}

	// A second server on the same directory is refused while the first runs.
	if _, err := startEnv("second", cfg); err == nil {
		t.Fatal("second server on a locked data directory started")
	}

	if err := e1.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// --- restart ---
	e2 := startPrivate(t, cfg)
	if got := rootOf(e2); got != root {
		t.Fatalf("root credentials changed across restart")
	}
	if e2.srv.ServerID() != serverID {
		t.Errorf("server ID changed: %s -> %s", serverID, e2.srv.ServerID())
	}
	c2 := e2.s3Client(root)
	lb, err := c2.ListBuckets(ctx(t), &s3.ListBucketsInput{})
	must(t, err, "ListBuckets")
	if len(lb.Buckets) != 1 || aws.ToString(lb.Buckets[0].Name) != b {
		t.Fatalf("buckets after restart: %+v", lb.Buckets)
	}
	gv, err := c2.GetBucketVersioning(ctx(t), &s3.GetBucketVersioningInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketVersioning")
	if gv.Status != s3types.BucketVersioningStatusEnabled {
		t.Errorf("versioning %q", gv.Status)
	}
	get := func(key string, vid *string) (*s3.GetObjectOutput, []byte) {
		out, err := c2.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), VersionId: vid, ChecksumMode: s3types.ChecksumModeEnabled})
		must(t, err, "GetObject "+key)
		return out, readAll(t, out.Body)
	}
	if out, data := get("doc", nil); string(data) != "v2" || aws.ToString(out.VersionId) != aws.ToString(v2.VersionId) {
		t.Errorf("doc latest %q version %q", data, aws.ToString(out.VersionId))
	}
	if out, data := get("doc", v1.VersionId); string(data) != "v1" || out.Metadata["rev"] != "1" || aws.ToInt32(out.TagCount) != 1 {
		t.Errorf("doc v1 %q meta %v tags %d", data, out.Metadata, aws.ToInt32(out.TagCount))
	}
	if out, data := get("big.bin", nil); !bytes.Equal(data, big) || aws.ToString(out.ChecksumCRC32C) != checksumB64(s3types.ChecksumAlgorithmCrc32c, big) {
		t.Errorf("big.bin differs (checksum %q)", aws.ToString(out.ChecksumCRC32C))
	}
	vs, err := c2.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b), Prefix: aws.String("gone")})
	must(t, err, "ListObjectVersions")
	if len(vs.DeleteMarkers) != 1 || aws.ToString(vs.DeleteMarkers[0].VersionId) != aws.ToString(del.VersionId) || len(vs.Versions) != 1 {
		t.Errorf("gone: %d markers %d versions", len(vs.DeleteMarkers), len(vs.Versions))
	}
	// The pending upload can be finished after the restart.
	lp, err := c2.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String("pending"), UploadId: cr.UploadId})
	must(t, err, "ListParts after restart")
	if len(lp.Parts) != 1 || aws.ToString(lp.Parts[0].ETag) != aws.ToString(up1.ETag) {
		t.Fatalf("parts after restart: %+v", lp.Parts)
	}
	up2, err := c2.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("pending"), UploadId: cr.UploadId,
		PartNumber: aws.Int32(2), Body: bytes.NewReader([]byte("tail"))})
	must(t, err, "UploadPart after restart")
	_, err = c2.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("pending"), UploadId: cr.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: up1.ETag}, {PartNumber: aws.Int32(2), ETag: up2.ETag}}}})
	must(t, err, "CompleteMultipartUpload after restart")
	if _, data := get("pending", nil); !bytes.Equal(data, append(part1, "tail"...)) {
		t.Errorf("completed object differs (%d bytes)", len(data))
	}

	// Bucket configs.
	gp, err := c2.GetBucketPolicy(ctx(t), &s3.GetBucketPolicyInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketPolicy")
	if aws.ToString(gp.Policy) != policy {
		t.Errorf("policy after restart: %s", aws.ToString(gp.Policy))
	}
	if r := e2.anon(t, http.MethodGet, "/"+b+"/doc", nil, nil); r.status != 200 || string(r.body) != "v2" {
		t.Errorf("anonymous GET through persisted policy: %d %s", r.status, r.body)
	}
	bt, err := c2.GetBucketTagging(ctx(t), &s3.GetBucketTaggingInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketTagging")
	if !reflect.DeepEqual(tagMap(bt.TagSet), map[string]string{"team": "e2e"}) {
		t.Errorf("bucket tags %v", tagMap(bt.TagSet))
	}

	// IAM survived.
	uc := e2.s3Client(user)
	out, err := uc.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("big.bin")})
	must(t, err, "user GetObject after restart")
	readAll(t, out.Body)
	_, err = uc.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("nope"), Body: stringsReader("x")})
	denied(t, err, "user PutObject after restart")
	var pol struct {
		Name string `json:"name"`
	}
	if r := e2.admin(t, root, http.MethodGet, "/policies/persisted-read", nil, &pol); r.status != 200 || pol.Name != "persisted-read" {
		t.Errorf("policy after restart: %d %s", r.status, r.body)
	}

	// Settings survived and are applied.
	var st2 config.Settings
	if r := e2.admin(t, root, http.MethodGet, "/config", nil, &st2); r.status != 200 {
		t.Fatalf("GET /config: %d", r.status)
	}
	if st2.Name != "renamed-e2e" || !reflect.DeepEqual(st2.Domains, []string{"persist.test"}) ||
		!reflect.DeepEqual(st2.CORS.AllowedOrigins, []string{"https://ui.example.com"}) || st2.Lifecycle.IntervalSeconds != 7200 {
		t.Errorf("settings after restart: %+v", st2)
	}
	if info := e2.srv.Info(); info.Name != "renamed-e2e" {
		t.Errorf("Info name %q", info.Name)
	}
	// The persisted virtual-host domain is live.
	if r := e2.signed(t, root, http.MethodGet, "http://"+b+".persist.test/doc", nil, nil); r.status != 200 || string(r.body) != "v2" {
		t.Errorf("virtual-host GET with persisted domain: %d %s", r.status, r.body)
	}
	// The persisted global CORS applies to preflights.
	r := e2.anon(t, http.MethodOptions, "/"+b+"/doc", nil, map[string]string{"Origin": "https://ui.example.com", "Access-Control-Request-Method": "GET"})
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "https://ui.example.com" {
		t.Errorf("preflight with persisted CORS: %d %v", r.status, r.header)
	}
	r = e2.anon(t, http.MethodOptions, "/"+b+"/doc", nil, map[string]string{"Origin": "https://other.example.com", "Access-Control-Request-Method": "GET"})
	if r.status != 403 {
		t.Errorf("preflight from another origin: %d", r.status)
	}
}

// TestMemoryBackendIsEphemeral: a memory server without a data path starts
// empty every time and keeps nothing on disk.
func TestMemoryBackendIsEphemeral(t *testing.T) {
	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendMemory
	cfg.Storage.Path = ""
	e1 := startPrivate(t, cfg)
	e1.newBucket(t)
	must(t, e1.stop(), "stop")
	e2 := startPrivate(t, cfg)
	lb, err := e2.root().ListBuckets(ctx(t), &s3.ListBucketsInput{})
	must(t, err, "ListBuckets")
	if len(lb.Buckets) != 0 {
		t.Errorf("memory server kept %d buckets", len(lb.Buckets))
	}
	if e2.srv.DataPath() != "" {
		t.Errorf("memory server data path %q", e2.srv.DataPath())
	}
}

// TestSystemEndpoints covers /_gos3/health, /ready, /info and /metrics.
func TestSystemEndpoints(t *testing.T) {
	e := fsEnv
	for _, p := range []string{"/_gos3/health", "/_gos3/ready"} {
		r := e.anon(t, http.MethodGet, p, nil, nil)
		if r.status != 200 || !strings.Contains(string(r.body), `"ok"`) {
			t.Errorf("%s: %d %s", p, r.status, r.body)
		}
	}
	r := e.anon(t, http.MethodGet, "/_gos3/info", nil, nil)
	var info struct {
		Service string `json:"service"`
		ID      string `json:"id"`
		Region  string `json:"region"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &info) != nil || info.Service != "gos3" || info.ID != e.srv.ServerID() || info.Region != region ||
		r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("/_gos3/info: %d %s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/_gos3/metrics", nil, nil); r.status != 401 {
		t.Errorf("anonymous metrics: %d", r.status)
	}
	// Make sure some S3 traffic has been recorded.
	_, err := e.root().ListBuckets(ctx(t), &s3.ListBucketsInput{})
	must(t, err, "ListBuckets")
	r = e.signed(t, rootCreds, http.MethodGet, "/_gos3/metrics", nil, nil)
	if r.status != 200 || !strings.Contains(string(r.body), "gos3_") || !strings.Contains(string(r.body), "ListBuckets") {
		t.Errorf("metrics as root: %d %.300s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/_gos3/nope", nil, nil); r.status != 404 || !strings.Contains(string(r.body), "NotFound") {
		t.Errorf("unknown system path: %d %s", r.status, r.body)
	}
	// Anonymous ListBuckets is denied; a browser GET / with the UI disabled
	// is not redirected.
	if r := e.anon(t, http.MethodGet, "/", nil, nil); r.status != 403 || r.code() != "AccessDenied" {
		t.Errorf("anonymous ListBuckets: %d %s", r.status, r.code())
	}
	// SigV2 is rejected with an explanation.
	if r := e.anon(t, http.MethodGet, "/", nil, map[string]string{"Authorization": "AWS " + rootAccess + ":c2lnbmF0dXJl"}); r.status != 403 || r.code() != "AccessDenied" {
		t.Errorf("SigV2: %d %s", r.status, r.code())
	}
	// Request time skew.
	req := rawRequest{method: http.MethodGet, path: "/", creds: &rootCreds}
	resp := e.doAt(t, req, time.Now().Add(-time.Hour))
	if resp.status != 403 || resp.code() != "RequestTimeTooSkewed" {
		t.Errorf("skewed request: %d %s", resp.status, resp.code())
	}
}

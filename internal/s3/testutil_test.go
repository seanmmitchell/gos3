package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// Root credentials of every test environment.
var testRoot = auth.Credentials{AccessKey: "GOS3TESTROOTKEY", SecretKey: "gos3-test-root-secret-key"}

// testEnv is a running S3 server over the memory backend for handler tests.
type testEnv struct {
	t        testing.TB
	srv      *httptest.Server
	server   *Server
	backend  *memory.Backend
	iam      *iam.Manager
	settings *config.Store
	metrics  *metrics.Registry
	logs     *syncBuffer // JSON log lines (debug level)
	// client is an aws-sdk-go-v2 S3 client using the root credentials
	// (path-style, region us-east-1, retries disabled).
	client *awss3.Client
	httpc  *http.Client

	mu        sync.RWMutex
	overrides map[string]handlerFunc
}

type testEnvConfig struct {
	settings config.Settings
	tls      bool
	opts     []func(*Options)
}

// testEnvOption customizes newTestEnv.
type testEnvOption func(*testEnvConfig)

// withTLS serves over HTTPS (the AWS SDK then uses aws-chunked trailers).
func withTLS() testEnvOption { return func(c *testEnvConfig) { c.tls = true } }

// withSettings edits the initial runtime settings.
func withSettings(f func(*config.Settings)) testEnvOption {
	return func(c *testEnvConfig) { f(&c.settings) }
}

// withOptions edits the handler Options before NewHandler.
func withOptions(f func(*Options)) testEnvOption {
	return func(c *testEnvConfig) { c.opts = append(c.opts, f) }
}

// newTestEnv starts a test server; it is shut down by t.Cleanup.
func newTestEnv(t testing.TB, opts ...testEnvOption) *testEnv {
	t.Helper()
	// Complete, valid settings (config.Store.Update validates the whole
	// document), without global CORS and virtual-host domains.
	cfg := testEnvConfig{settings: config.Settings{
		Region:    "us-east-1",
		Discovery: discovery.DefaultConfig(),
		Logging:   config.LoggingConfig{Level: "info", Format: "text", AccessLog: true},
		Lifecycle: config.LifecycleSettings{Enabled: true, IntervalSeconds: 3600},
	}}
	for _, o := range opts {
		o(&cfg)
	}
	mgr, err := iam.Open(iam.Options{RootAccessKey: testRoot.AccessKey, RootSecretKey: testRoot.SecretKey})
	if err != nil {
		t.Fatalf("iam.Open: %v", err)
	}
	e := &testEnv{
		t:         t,
		backend:   memory.New(),
		iam:       mgr,
		settings:  config.NewStore(cfg.settings, ""),
		metrics:   metrics.NewRegistry(),
		logs:      &syncBuffer{},
		overrides: map[string]handlerFunc{},
	}
	o := Options{
		Backend:  e.backend,
		Verifier: auth.NewVerifier(mgr, auth.Options{Region: "us-east-1"}),
		IAM:      mgr,
		Metrics:  e.metrics,
		Settings: e.settings,
		Logger:   slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version:  "test",
	}
	for _, f := range cfg.opts {
		f(&o)
	}
	e.server = New(o)
	e.server.handlerOverride = func(op string) handlerFunc {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.overrides[op]
	}
	if cfg.tls {
		e.srv = httptest.NewTLSServer(e.server)
	} else {
		e.srv = httptest.NewServer(e.server)
	}
	e.httpc = e.srv.Client()
	e.client = e.clientFor(testRoot)
	t.Cleanup(func() {
		e.srv.Close()
		_ = e.backend.Close()
		_ = mgr.Close()
	})
	return e
}

// clientFor returns an SDK client for creds against the test server.
func (e *testEnv) clientFor(c auth.Credentials) *awss3.Client {
	return awss3.New(awss3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		BaseEndpoint: aws.String(e.srv.URL),
		UsePathStyle: true,
		HTTPClient:   e.httpc,
		Retryer:      aws.NopRetryer{},
	})
}

// override replaces the handler of op in this environment.
func (e *testEnv) override(op string, h handlerFunc) {
	if lookupOp(op) == nil {
		e.t.Fatalf("override: unknown op %q", op)
	}
	e.mu.Lock()
	e.overrides[op] = h
	e.mu.Unlock()
}

// testResponse is a finished response with its body read.
type testResponse struct {
	*http.Response
	Data []byte
}

// code returns the S3 error code of the response body ("" if none).
func (r *testResponse) code() string {
	var doc struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(r.Data, &doc) != nil {
		return ""
	}
	return doc.Code
}

// url returns the absolute URL of a path (which may include a query).
func (e *testEnv) url(path string) string { return e.srv.URL + path }

// newRequest builds a request for path (with optional query) and headers.
func (e *testEnv) newRequest(method, path string, body []byte, hdr http.Header) *http.Request {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.url(path), rd)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if hdr.Get("Host") != "" {
		req.Host = hdr.Get("Host")
		req.Header.Del("Host")
	}
	return req
}

// do sends req and reads the response.
func (e *testEnv) do(req *http.Request) *testResponse {
	e.t.Helper()
	resp, err := e.httpc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return &testResponse{Response: resp, Data: data}
}

// signedAs sends a SigV4 (header) request signed with creds; body may be
// nil. The payload hash is the body's SHA-256.
func (e *testEnv) signedAs(c auth.Credentials, method, path string, body []byte, hdr http.Header) *testResponse {
	e.t.Helper()
	req := e.newRequest(method, path, body, hdr)
	sum := sha256.Sum256(body)
	if err := auth.SignRequest(req, c, "us-east-1", "s3", hex.EncodeToString(sum[:]), time.Now()); err != nil {
		e.t.Fatalf("SignRequest: %v", err)
	}
	return e.do(req)
}

// signed sends a request signed with the root credentials.
func (e *testEnv) signed(method, path string, body []byte, hdr http.Header) *testResponse {
	e.t.Helper()
	return e.signedAs(testRoot, method, path, body, hdr)
}

// anon sends an unauthenticated request.
func (e *testEnv) anon(method, path string, body []byte, hdr http.Header) *testResponse {
	e.t.Helper()
	return e.do(e.newRequest(method, path, body, hdr))
}

// createBucket creates a bucket directly in the backend.
func (e *testEnv) createBucket(name string) {
	e.t.Helper()
	if err := e.backend.CreateBucket(context.Background(), name, storage.CreateBucketOptions{Region: "us-east-1"}); err != nil {
		e.t.Fatalf("CreateBucket %s: %v", name, err)
	}
}

// putObject stores an object directly in the backend (ETag = MD5).
func (e *testEnv) putObject(bucket, key string, data []byte, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	ctx := context.Background()
	w, err := e.backend.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		e.t.Fatalf("NewObjectWriter: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		e.t.Fatalf("write: %v", err)
	}
	if meta.ETag == "" {
		meta.ETag = md5Hex(data)
	}
	info, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	return info
}

// putBucketConfig stores a bucket configuration document directly.
func (e *testEnv) putBucketConfig(bucket, key, data string) {
	e.t.Helper()
	if err := e.backend.PutBucketConfig(context.Background(), bucket, key, []byte(data)); err != nil {
		e.t.Fatalf("PutBucketConfig: %v", err)
	}
}

// createUser creates an IAM user with the given policies and returns a
// fresh access key for it.
func (e *testEnv) createUser(name string, policies ...string) auth.Credentials {
	e.t.Helper()
	ctx := context.Background()
	if policies == nil {
		policies = []string{}
	}
	if _, err := e.iam.CreateUser(ctx, iam.UserSpec{Name: name, Policies: policies}); err != nil {
		e.t.Fatalf("CreateUser: %v", err)
	}
	k, err := e.iam.CreateAccessKey(ctx, name, "test", "", "")
	if err != nil {
		e.t.Fatalf("CreateAccessKey: %v", err)
	}
	return auth.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey}
}

// putPolicy creates a custom identity policy from JSON.
func (e *testEnv) putPolicy(name, doc string) {
	e.t.Helper()
	p, err := iam.ParsePolicy([]byte(doc))
	if err != nil {
		e.t.Fatalf("ParsePolicy: %v", err)
	}
	if _, err := e.iam.PutPolicy(context.Background(), name, "", *p); err != nil {
		e.t.Fatalf("PutPolicy: %v", err)
	}
}

// authCreds builds client credentials.
func authCreds(ak, sk string) auth.Credentials { return auth.Credentials{AccessKey: ak, SecretKey: sk} }

// signRoot signs a bodiless request with the root credentials.
func signRoot(req *http.Request) error {
	return auth.SignRequest(req, testRoot, "us-east-1", "s3", "", time.Now())
}

// okHandler is an override that answers 200 with "<op> <bucket> <key>".
func okHandler(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	writeBody(w, http.StatusOK, "text/plain", []byte(rc.opName+" "+rc.bucket+" "+rc.key))
	return nil
}

// requireAuthz skips the test when the IAM engine is still a stub.
func requireAuthz(t testing.TB, e *testEnv) {
	t.Helper()
	d := e.iam.Authorize(context.Background(), iam.AuthzRequest{
		Identity: auth.Anonymous, Action: "s3:GetObject", Bucket: "b", Key: "k",
		BucketPolicy: &iam.PolicyDocument{Statement: []iam.Statement{{Effect: "Allow", Principal: &iam.Principal{Any: true}, Action: iam.StringOrSlice{"s3:GetObject"}, Resource: iam.StringOrSlice{"arn:aws:s3:::b/*"}}}},
	})
	if !d.Allowed && strings.Contains(strings.ToLower(d.Reason), "not implemented") {
		t.Skip("iam.Manager.Authorize is not implemented yet")
	}
}

func md5Hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

// syncBuffer is a goroutine-safe bytes.Buffer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

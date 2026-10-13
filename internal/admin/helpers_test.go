package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

var rootCreds = auth.Credentials{AccessKey: "rootkey", SecretKey: "rootsecret-123456"}

// fakeClock is a settable clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type testEnv struct {
	t        *testing.T
	api      *api
	srv      *httptest.Server
	backend  *memory.Backend
	iam      *iam.Manager
	settings *config.Store
	clock    *fakeClock
	info     discovery.Info
}

func defaultSettings() config.Settings {
	return config.Settings{
		Name:       "gos3-test",
		Region:     "us-east-1",
		PublicURL:  "http://127.0.0.1:9000",
		Domains:    []string{"s3.local"},
		PublicInfo: true,
		UIRedirect: true,
		CORS: config.CORSConfig{
			AllowedOrigins: []string{"https://ui.example.com", "https://*.apps.example.com"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag", "x-amz-request-id"},
			MaxAgeSeconds:  600,
		},
		Discovery:     discovery.DefaultConfig(),
		Logging:       config.LoggingConfig{Level: "info", Format: "text", AccessLog: true},
		Lifecycle:     config.LifecycleSettings{Enabled: true, IntervalSeconds: 3600},
		MetricsPublic: false,
		Compression: config.CompressionSettings{
			Mode: config.CompressionOptional, Level: 1, MinSize: 4096, SkipContentTypes: []string{"video/*"},
		},
	}
}

type envOption func(*Deps)

func withoutDiscovery() envOption { return func(d *Deps) { d.Discovery = nil } }
func withFSBackend(path string) envOption {
	return func(d *Deps) { d.BackendName, d.DataPath = "fs", path }
}

func newEnv(t *testing.T, opts ...envOption) *testEnv {
	t.Helper()
	mgr, err := iam.Open(iam.Options{RootAccessKey: rootCreds.AccessKey, RootSecretKey: rootCreds.SecretKey})
	if err != nil {
		t.Fatalf("iam.Open: %v", err)
	}
	be := memory.New()
	t.Cleanup(func() { be.Close() })
	store := config.NewStore(defaultSettings(), "")
	info := discovery.Info{
		Service: discovery.ServiceName, ID: "0123456789abcdef", Name: "gos3-test", Version: "test",
		APIVersion: "v1", Region: "us-east-1",
		Endpoints: discovery.Endpoints{S3: "http://127.0.0.1:9000", Admin: "http://127.0.0.1:9000" + Prefix},
		Features:  []string{"admin"},
		StartedAt: time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := Deps{
		Backend:     be,
		Verifier:    auth.NewVerifier(mgr, auth.Options{Region: "us-east-1"}),
		IAM:         mgr,
		Metrics:     metrics.NewRegistry(),
		Settings:    store,
		Discovery:   discovery.New(store.Get().Discovery, func() discovery.Info { return info }, logger),
		Info:        func() discovery.Info { return info },
		BackendName: "memory",
		Logger:      logger,
	}
	for _, o := range opts {
		o(&d)
	}
	a := newAPI(d)
	clock := &fakeClock{t: time.Now()}
	a.now = clock.Now
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &testEnv{t: t, api: a, srv: srv, backend: be, iam: mgr, settings: store, clock: clock, info: info}
}

// response is a decoded HTTP response.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
}

func (r response) errCode(t *testing.T) (string, string) {
	t.Helper()
	var e errorBody
	r.json(t, &e)
	return e.Error.Code, e.Error.Message
}

// expectError asserts a JSON error response.
func (r response) expectError(t *testing.T, status int, code string) string {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d (body %s)", r.status, status, r.body)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	got, msg := r.errCode(t)
	if got != code {
		t.Fatalf("code = %q, want %q (message %q)", got, code, msg)
	}
	return msg
}

func (r response) expectStatus(t *testing.T, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d (body %s)", r.status, status, r.body)
	}
}

// reqOpt customizes a request before signing.
type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// do sends a request signed with creds (nil = anonymous). body may be nil,
// a string/[]byte (sent raw) or a value encoded as JSON.
func (e *testEnv) do(creds *auth.Credentials, method, path string, body any, opts ...reqOpt) response {
	e.t.Helper()
	var data []byte
	switch b := body.(type) {
	case nil:
	case string:
		data = []byte(b)
	case []byte:
		data = b
	default:
		var err error
		if data, err = json.Marshal(b); err != nil {
			e.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(data))
	if err != nil {
		e.t.Fatal(err)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	if creds != nil {
		sum := sha256.Sum256(data)
		if err := auth.SignRequest(req, *creds, "us-east-1", "s3", hex.EncodeToString(sum[:]), time.Now()); err != nil {
			e.t.Fatalf("SignRequest: %v", err)
		}
	}
	return e.send(req)
}

func (e *testEnv) send(req *http.Request) response {
	e.t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{status: res.StatusCode, header: res.Header, body: b}
}

func (e *testEnv) root(method, path string, body any, opts ...reqOpt) response {
	e.t.Helper()
	return e.do(&rootCreds, method, Prefix+path, body, opts...)
}

// addUser creates a user with policies and one access key.
func (e *testEnv) addUser(name string, policies ...string) auth.Credentials {
	e.t.Helper()
	ctx := context.Background()
	if policies == nil {
		policies = []string{}
	}
	if _, err := e.iam.CreateUser(ctx, iam.UserSpec{Name: name, Policies: policies}); err != nil {
		e.t.Fatalf("CreateUser: %v", err)
	}
	k, err := e.iam.CreateAccessKey(ctx, name, "", "", "")
	if err != nil {
		e.t.Fatalf("CreateAccessKey: %v", err)
	}
	return auth.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey}
}

func (e *testEnv) createBucket(name string, opts storage.CreateBucketOptions) {
	e.t.Helper()
	if err := e.backend.CreateBucket(context.Background(), name, opts); err != nil {
		e.t.Fatalf("CreateBucket: %v", err)
	}
}

func (e *testEnv) putObject(bucket, key, data string) storage.ObjectInfo {
	e.t.Helper()
	ctx := context.Background()
	w, err := e.backend.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		e.t.Fatalf("NewObjectWriter: %v", err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		e.t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "etag"}, storage.CommitOptions{})
	if err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	return info
}

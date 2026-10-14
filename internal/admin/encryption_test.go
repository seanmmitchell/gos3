package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/storage"
	storagefs "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

type encryptionTestBackend struct {
	storage.Backend
	validationError error
	prepareError    error
	prepared        []string
}

func (b *encryptionTestBackend) ValidateEncryptionSettings(context.Context, string, storage.EncryptionSettings) error {
	return b.validationError
}
func (b *encryptionTestBackend) PrepareEncryption(_ context.Context, _, mode string) error {
	b.prepared = append(b.prepared, mode)
	return b.prepareError
}

// providersTestBackend also reports the usable providers.
type providersTestBackend struct {
	*encryptionTestBackend
	available []string
}

func (b *providersTestBackend) EncryptionProviders(context.Context, string) ([]string, error) {
	return b.available, nil
}

func storedProvider(t *testing.T, e *testEnv, bucket string) string {
	t.Helper()
	raw, err := e.backend.GetBucketConfig(context.Background(), bucket, storage.ConfigEncryptionProvider)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBucketEncryptionProvider(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	b := &encryptionTestBackend{Backend: e.backend}
	e.api.d.Backend = b
	path := "/buckets/photos/encryption-provider"

	e.root("PUT", path, map[string]any{"provider": "restic"}).expectStatus(t, 200)
	stored := storedProvider(t, e, "photos")
	if stored != `{"provider":"restic"}` {
		t.Fatalf("bucket contains storage settings: %s", stored)
	}
	var doc encryptionDocument
	e.root("GET", path, nil).json(t, &doc)
	if doc.Provider != "restic" {
		t.Fatalf("GET: %+v", doc)
	}
	// Storage and secrets cannot be supplied at the bucket level; the
	// strict decoder names the offending field.
	for _, field := range []string{"repository", "endpoint", "accessKey", "secretKey", "password", "sessionToken", "region"} {
		msg := e.root("PUT", path, map[string]any{"provider": "restic", field: "unexpected"}).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, field) {
			t.Fatalf("%s: message %q does not name the field", field, msg)
		}
	}
	if msg := e.root("PUT", path, "").expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "required") {
		t.Fatalf("empty body: %q", msg)
	}
	e.root("PUT", path, map[string]any{"provider": "aws:kms"}).expectError(t, 400, codeInvalidRequest)

	// A failed validation or preparation keeps the previous provider.
	b.validationError = errors.New("backing unavailable")
	e.root("PUT", path, map[string]any{"provider": "AES256"}).expectError(t, 400, codeInvalidRequest)
	if got := storedProvider(t, e, "photos"); got != stored {
		t.Fatalf("failed validation replaced provider: %s", got)
	}
	b.validationError = nil
	b.prepareError = errors.New("initialization timed out")
	msg := e.root("PUT", path, map[string]any{"provider": "AES256"}).expectError(t, 400, codeInvalidRequest)
	if !strings.Contains(msg, "not changed") {
		t.Fatalf("preparation failure message %q", msg)
	}
	if got := storedProvider(t, e, "photos"); got != stored {
		t.Fatalf("failed preparation replaced provider: %s", got)
	}
	// An unsupported provider is reported as such, not as a repository error.
	b.prepareError = fmt.Errorf("memory backing: %w", errors.ErrUnsupported)
	if msg := e.root("PUT", path, map[string]any{"provider": "restic"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "does not support") {
		t.Fatalf("unsupported: %q", msg)
	}
	b.prepareError = nil
	e.root("GET", "/buckets/missing/encryption-provider", nil).expectError(t, 404, codeNotFound)
}

// The providers a bucket can use are reported, and others are refused
// before anything is prepared.
func TestBucketEncryptionProviderAvailability(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	b := &providersTestBackend{encryptionTestBackend: &encryptionTestBackend{Backend: e.backend}, available: []string{storage.EncryptionAES256}}
	e.api.d.Backend = b
	path := "/buckets/photos/encryption-provider"
	var doc encryptionDocument
	e.root("GET", path, nil).json(t, &doc)
	if doc.Provider != storage.EncryptionAES256 || !slices.Equal(doc.Available, []string{storage.EncryptionAES256}) {
		t.Fatalf("GET: %+v", doc)
	}
	if msg := e.root("PUT", path, map[string]any{"provider": "restic"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "restic is not available") {
		t.Fatalf("message %q", msg)
	}
	if len(b.prepared) != 0 || storedProvider(t, e, "photos") != "" {
		t.Fatalf("unavailable provider was prepared (%v) or stored", b.prepared)
	}
	b.available = []string{storage.EncryptionAES256, storage.EncryptionRestic}
	e.root("PUT", path, map[string]any{"provider": "restic"}).json(t, &doc)
	if doc.Provider != "restic" || len(doc.Available) != 2 {
		t.Fatalf("PUT: %+v", doc)
	}
}

// The memory backend reports AES256 only, so restic is refused for buckets
// stored in memory with a precise message; a missing bucket is 404.
func TestBucketEncryptionProviderMemoryBacking(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	if msg := e.root("PUT", "/buckets/photos/encryption-provider", map[string]any{"provider": "restic"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "filesystem backing") {
		t.Fatalf("message %q", msg)
	}
	e.root("PUT", "/buckets/photos/encryption-provider", map[string]any{"provider": "AES256"}).expectStatus(t, 200)
	e.root("PUT", "/buckets/missing/encryption-provider", map[string]any{"provider": "AES256"}).expectError(t, 404, codeNotFound)
}

// GET validates the stored document with the parser writes use.
func TestBucketEncryptionProviderInvalidStored(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	if err := e.backend.PutBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider, []byte(`{"provider":"rot13"}`)); err != nil {
		t.Fatal(err)
	}
	if msg := e.root("GET", "/buckets/photos/encryption-provider", nil).expectError(t, 500, codeInternalError); !strings.Contains(msg, "rot13") {
		t.Fatalf("message %q", msg)
	}
	e.root("PUT", "/buckets/photos/encryption-provider", map[string]any{"provider": "AES256"}).expectStatus(t, 200)
	e.root("GET", "/buckets/photos/encryption-provider", nil).expectStatus(t, 200)
}

func TestBucketEncryptionProviderScoped(t *testing.T) {
	e := newEnv(t)
	e.createBucket("team-a", storage.CreateBucketOptions{})
	e.createBucket("other", storage.CreateBucketOptions{})
	e.putPolicy("encryption", map[string]any{"Effect": "Allow", "Action": []string{"admin:GetBucketEncryption", "admin:PutBucketEncryption"}, "Resource": "arn:gos3:admin:::bucket/team-*"})
	alice := e.addUser("alice", "encryption")
	e.do(&alice, "PUT", Prefix+"/buckets/team-a/encryption-provider", map[string]any{"provider": "AES256"}).expectStatus(t, 200)
	e.do(&alice, "GET", Prefix+"/buckets/team-a/encryption-provider", nil).expectStatus(t, 200)
	e.do(&alice, "GET", Prefix+"/buckets/other/encryption-provider", nil).expectError(t, 403, codeAccessDenied)
	e.do(&alice, "PUT", Prefix+"/buckets/other/encryption-provider", map[string]any{"provider": "AES256"}).expectError(t, 403, codeAccessDenied)
}

// Preparing one bucket's provider (possibly a slow remote repository) must
// not block another bucket's, while one key stays serialized.
func TestKeyedMutexIsPerKey(t *testing.T) {
	var k keyedMutex
	unlockA := k.lock("bucket/a")
	done := make(chan struct{})
	go func() {
		k.lock("bucket/b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock of another key blocked")
	}
	second := make(chan struct{})
	go func() {
		k.lock("bucket/a")()
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("same key was not serialized")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-second
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.m) != 0 {
		t.Fatalf("entries leaked: %v", k.m)
	}
}

// A force delete removes the encryption documents only together with the
// bucket, so a bucket surviving a failed DeleteBucket keeps them.
func TestEmptyBucketKeepsEncryptionConfig(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	ctx := context.Background()
	for key, doc := range map[string]string{
		storage.ConfigEncryptionProvider: `{"provider":"restic"}`,
		storage.ConfigEncryption:         `<ServerSideEncryptionConfiguration/>`,
		storage.ConfigTagging:            `<Tagging/>`,
	} {
		if err := e.backend.PutBucketConfig(ctx, "photos", key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.api.emptyBucket(ctx, "photos", &lockGuard{bucket: "photos", now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{storage.ConfigEncryptionProvider, storage.ConfigEncryption} {
		if _, err := e.backend.GetBucketConfig(ctx, "photos", key); err != nil {
			t.Fatalf("%s removed before the bucket: %v", key, err)
		}
	}
	if _, err := e.backend.GetBucketConfig(ctx, "photos", storage.ConfigTagging); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("tagging not removed: %v", err)
	}
	e.root("DELETE", "/buckets/photos?force=true", nil).expectStatus(t, 204)
}

// Restic S3 storage on this server itself is refused, whether the endpoint
// is an advertised URL or another name answering with this server's ID.
func TestResticBackingRefusesThisServer(t *testing.T) {
	b, err := storagefs.Open(t.TempDir(), storagefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := multi.Open(context.Background(), multi.Options{Backings: []multi.Backing{{Name: "default", Kind: "fs", Backend: b}}, HealthInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	self := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != discovery.InfoPath {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"service":"gos3","id":"0123456789abcdef","name":"gos3-test","apiVersion":"v1"}`)
	}))
	defer self.Close()
	e := newEnv(t, func(d *Deps) { d.Backend = r })
	body := func(endpoint string) map[string]any {
		return map[string]any{"mode": "s3", "endpoint": endpoint, "bucket": "archives", "accessKey": "a", "secretKey": "s"}
	}
	for _, endpoint := range []string{"http://127.0.0.1:9000", "HTTP://127.0.0.1:9000/", self.URL} {
		msg := e.root("PUT", "/backings/default/restic", body(endpoint)).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "this gos3 server") {
			t.Fatalf("%s: message %q", endpoint, msg)
		}
	}
	var doc resticBackingResponse
	e.root("GET", "/backings/default/restic", nil).json(t, &doc)
	if doc.Mode != "local" {
		t.Fatalf("refused settings were stored: %+v", doc)
	}
}

func TestOriginOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://S3.Example.com":       "https://s3.example.com:443",
		"http://s3.example.com:80/":    "http://s3.example.com:80",
		"http://[::1]:9000":            "http://[::1]:9000",
		"ftp://example.com":            "",
		"not a url":                    "",
		"https://s3.example.com:8443/": "https://s3.example.com:8443",
	} {
		if got := originOf(in); got != want {
			t.Errorf("originOf(%q) = %q, want %q", in, got, want)
		}
	}
}

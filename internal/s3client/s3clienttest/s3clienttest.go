// Package s3clienttest runs an in-process S3 service (the gos3 S3 handler
// over the memory backend) for tests of code that talks to a remote S3
// service through internal/s3client.
//
// It deliberately does not import internal/server or the fs backend, so
// tests inside those packages can use it without import cycles.
package s3clienttest

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// Credentials of the remote's root user.
const (
	AccessKey = "REMOTEACCESSKEY0001"
	SecretKey = "remote-secret-key-0000000000000000000001"
	Region    = "us-east-1"
)

// Remote is a running in-process S3 service.
type Remote struct {
	URL     string          // endpoint, http://127.0.0.1:port
	Backend storage.Backend // the remote's storage, for direct inspection
	// Requests counts the requests served.
	Requests atomic.Int64
	// Fail, when set, is consulted before every request; returning a
	// status >= 400 answers with that status instead (fault injection).
	Fail atomic.Pointer[func(r *http.Request) int]

	srv *httptest.Server
}

// Start starts a remote; it is stopped by t.Cleanup.
func Start(t testing.TB) *Remote {
	t.Helper()
	mgr, err := iam.Open(iam.Options{RootAccessKey: AccessKey, RootSecretKey: SecretKey})
	if err != nil {
		t.Fatalf("s3clienttest: iam.Open: %v", err)
	}
	be := memory.New()
	h := s3.New(s3.Options{
		Backend:  be,
		Verifier: auth.NewVerifier(mgr, auth.Options{Region: Region}),
		IAM:      mgr,
		Logger:   slog.New(slog.DiscardHandler),
		Version:  "remote",
	})
	r := &Remote{Backend: be}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.Requests.Add(1)
		if f := r.Fail.Load(); f != nil {
			if st := (*f)(req); st >= 400 {
				w.WriteHeader(st)
				return
			}
		}
		h.ServeHTTP(w, req)
	}))
	r.URL = r.srv.URL
	t.Cleanup(func() {
		r.srv.Close()
		_ = be.Close()
	})
	return r
}

// Config returns an s3client configuration for the remote (path style).
func (r *Remote) Config() s3client.Config {
	return s3client.Config{
		Endpoint:    r.URL,
		Region:      Region,
		AccessKey:   AccessKey,
		SecretKey:   SecretKey,
		PathStyle:   true,
		MaxAttempts: 2,
	}
}

// Client returns a client for the remote.
func (r *Remote) Client(t testing.TB) *s3client.Client {
	t.Helper()
	c, err := s3client.New(r.Config())
	if err != nil {
		t.Fatalf("s3clienttest: %v", err)
	}
	return c
}

// CreateBucket creates a bucket on the remote.
func (r *Remote) CreateBucket(t testing.TB, name string) {
	t.Helper()
	if err := r.Backend.CreateBucket(t.Context(), name, storage.CreateBucketOptions{Region: Region}); err != nil {
		t.Fatalf("s3clienttest: create bucket %q: %v", name, err)
	}
}

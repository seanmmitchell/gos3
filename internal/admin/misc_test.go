package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestNewHandlerRequiresDeps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewHandler with empty Deps did not panic")
		}
	}()
	NewHandler(Deps{})
}

func TestNewHandlerServes(t *testing.T) {
	e := newEnv(t)
	h := NewHandler(e.api.d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", Prefix+"/info", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rec.Code)
	}
}

// panicBackend panics on ListBuckets to exercise panic recovery.
type panicBackend struct{ storage.Backend }

func (panicBackend) ListBuckets(context.Context) ([]storage.BucketInfo, error) { panic("kaboom") }

func TestPanicRecovery(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Backend = panicBackend{d.Backend} })
	msg := e.root("GET", "/buckets", nil).expectError(t, 500, codeInternalError)
	if strings.Contains(msg, "kaboom") {
		t.Errorf("panic value leaked: %q", msg)
	}
	// The server keeps working.
	e.root("GET", "/info", nil).expectStatus(t, 200)
}

// failingBackend fails ListBuckets with an opaque error.
type failingBackend struct{ storage.Backend }

func (failingBackend) ListBuckets(context.Context) ([]storage.BucketInfo, error) {
	return nil, errors.New("disk on fire")
}

func TestBackendErrors(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Backend = failingBackend{d.Backend} })
	e.root("GET", "/buckets", nil).expectError(t, 500, codeInternalError)
	e.root("GET", "/stats", nil).expectError(t, 500, codeInternalError)
}

func TestToAPIError(t *testing.T) {
	a := &api{}
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{iam.ErrUserNotFound, 404, codeNotFound},
		{iam.ErrKeyNotFound, 404, codeNotFound},
		{fmt.Errorf("x: %w", iam.ErrPolicyNotFound), 404, codeNotFound},
		{iam.ErrUserExists, 409, codeConflict},
		{iam.ErrKeyExists, 409, codeConflict},
		{iam.ErrPolicyInUse, 409, codeConflict},
		{iam.ErrInvalidName, 400, codeInvalidName},
		{iam.ErrInvalidKey, 400, codeInvalidName},
		{iam.ErrInvalidPolicy, 400, codeInvalidPolicy},
		{iam.ErrTooManyKeys, 400, codeInvalidRequest},
		{iam.ErrPolicyBuiltin, 400, codeInvalidRequest},
		{iam.ErrRootImmutable, 400, codeInvalidRequest},
		{storage.ErrBucketNotFound, 404, codeNotFound},
		{storage.ErrBucketNotEmpty, 409, codeConflict},
		{storage.ErrInvalidName, 400, codeInvalidName},
		{s3err.ErrMalformedPolicy, 400, codeInvalidRequest},
		{context.Canceled, 503, codeInternalError},
		{errors.New("boom"), 500, codeInternalError},
		{badRequest("x"), 400, codeInvalidRequest},
	}
	for _, tt := range tests {
		got := a.toAPIError(tt.err)
		if got.Status != tt.status || got.Code != tt.code {
			t.Errorf("toAPIError(%v) = %d %s, want %d %s", tt.err, got.Status, got.Code, tt.status, tt.code)
		}
		if strings.HasPrefix(got.Message, "iam: ") || strings.HasPrefix(got.Message, "storage: ") {
			t.Errorf("message %q keeps the package prefix", got.Message)
		}
	}
}

func TestInfoWithoutProvider(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Info = nil })
	r := e.root("GET", "/info", nil)
	r.expectStatus(t, 200)
	var si serverInfo
	r.json(t, &si)
	if si.Service != "gos3" || si.Features == nil || si.UptimeSeconds < 0 {
		t.Errorf("info = %+v", si)
	}
}

func TestCachePrune(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	c := newTTLCache[int](time.Minute, time.Minute, clock.Now)
	ctx := context.Background()
	for i := range maxCacheEntries {
		c.get(ctx, fmt.Sprint(i), func(context.Context) (int, error) { return i, nil })
	}
	clock.Advance(30 * time.Second)
	c.get(ctx, "extra", func(context.Context) (int, error) { return -1, nil })
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > maxCacheEntries {
		t.Errorf("cache grew to %d entries", n)
	}
	clock.Advance(time.Minute)
	c.get(ctx, "extra2", func(context.Context) (int, error) { return -1, nil })
	c.mu.Lock()
	n = len(c.entries)
	c.mu.Unlock()
	if n > maxCacheEntries {
		t.Errorf("cache grew to %d entries", n)
	}
	c.invalidateAll()
	c.mu.Lock()
	n = len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("invalidateAll left %d entries", n)
	}
}

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

func TestValidateCORSOrigin(t *testing.T) {
	valid := []string{"*", "https://a.com", "http://localhost:3000", "https://*.example.com", "https://*.example.com:8443",
		"http://10.0.0.1", "http://[::1]:8080", "http://[fe80::1]", "app://localhost", "chrome-extension://abcdef"}
	invalid := []string{"", "a.com", "https://", "https://a.com/", "https://a.com/x", "https://a.com?x", "https://u@a.com",
		"https://a.com:", "https://a.com:0", "https://a.com:+80", "https://a.com:x", "https://*.com.*", "https://a.*.com",
		"https://*", "http://[::1", "http://[::1]x", "http://[1.2.3.4]", "1ttp://a.com", "https://a b.com"}
	for _, o := range valid {
		if err := validateCORSOrigin(o); err != nil {
			t.Errorf("validateCORSOrigin(%q) = %v", o, err)
		}
	}
	for _, o := range invalid {
		if err := validateCORSOrigin(o); err == nil {
			t.Errorf("validateCORSOrigin(%q) accepted", o)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		in        string
		allowPath bool
		ok        bool
	}{
		{"http://h:9000", false, true},
		{"https://h", false, true},
		{"https://h/", false, true},
		{"http://10.0.0.5:9000", false, true},
		{"http://[::1]:9000", false, true},
		{"http://h/base", true, true},
		{"http://h/base", false, false},
		{"h:9000", false, false},
		{"http://", false, false},
		{"http://h:0", false, false},
		{"http://h:", false, false},
		{"http://h#f", false, false},
		{"http://h?", false, false},
		{"http://bad_host!", false, false},
		{"http://" + strings.Repeat("a", maxURLLen), false, false},
		{"%zz", false, false},
	}
	for _, tt := range tests {
		if err := validateBaseURL(tt.in, tt.allowPath); (err == nil) != tt.ok {
			t.Errorf("validateBaseURL(%q, %v) = %v", tt.in, tt.allowPath, err)
		}
	}
}

func TestValidHostname(t *testing.T) {
	for _, h := range []string{"a", "s3.local", "my-host.example.com", "x_y.local", strings.Repeat("a", 63) + ".com"} {
		if !validHostname(h) {
			t.Errorf("validHostname(%q) = false", h)
		}
	}
	for _, h := range []string{"", ".", "a..b", "-a.com", "a-.com", "a b", strings.Repeat("a", 64) + ".com",
		strings.Repeat("a.", 127) + "aa", "a:80", "a/b"} {
		if validHostname(h) {
			t.Errorf("validHostname(%q) = true", h)
		}
	}
}

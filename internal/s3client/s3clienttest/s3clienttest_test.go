package s3clienttest_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

// faultOnce returns an After hook applying fault to the first request
// matching method, and counting the requests it saw.
func faultOnce(method string, fault s3clienttest.Fault, seen *atomic.Int64) *func(*http.Request) s3clienttest.Fault {
	var done atomic.Bool
	f := func(r *http.Request) s3clienttest.Fault {
		seen.Add(1)
		if r.Method == method && done.CompareAndSwap(false, true) {
			return fault
		}
		return s3clienttest.Pass
	}
	return &f
}

func setup(t *testing.T) (*s3clienttest.Remote, *s3client.Client) {
	t.Helper()
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "bkt")
	return r, r.Client(t)
}

func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func readObject(t *testing.T, c *s3client.Client, key string) []byte {
	t.Helper()
	rc, _, err := c.GetObject(t.Context(), "bkt", key, s3client.GetOptions{})
	if err != nil {
		t.Fatalf("GetObject %q: %v", key, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAfterStatus500(t *testing.T) {
	r, c := setup(t)
	var seen atomic.Int64
	r.After.Store(faultOnce(http.MethodPut, s3clienttest.Status500, &seen))
	data := body(1 << 20)
	_, err := c.PutObject(t.Context(), "bkt", "k", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{NoRetry: true})
	var e *s3client.Error
	if !errors.As(err, &e) || e.StatusCode != http.StatusInternalServerError || e.Code != "InternalError" {
		t.Fatalf("PutObject: %v, want an injected 500", err)
	}
	// The write took effect, with its whole body.
	if got := readObject(t, c, "k"); !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(data))
	}
	if seen.Load() != 2 {
		t.Fatalf("After saw %d requests, want 2", seen.Load())
	}
}

func TestAfterReset(t *testing.T) {
	r, c := setup(t)
	var seen atomic.Int64
	r.After.Store(faultOnce(http.MethodPut, s3clienttest.Reset, &seen))
	data := body(256 << 10)
	// Conditional PUTs are not retried after transport errors.
	_, err := c.PutObject(t.Context(), "bkt", "k", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{IfNoneMatch: true})
	var e *s3client.Error
	if !errors.As(err, &e) || e.Err == nil || !s3client.IsUnavailable(err) {
		t.Fatalf("PutObject: %v, want a transport error", err)
	}
	if got := readObject(t, c, "k"); !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(data))
	}

	// Idempotent requests are retried over a new connection.
	r.After.Store(faultOnce(http.MethodGet, s3clienttest.Reset, &seen))
	if got := readObject(t, c, "k"); !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes after a reset, want %d", len(got), len(data))
	}
}

func TestIgnoreConditionals(t *testing.T) {
	r, c := setup(t)
	ctx := t.Context()
	put := func(v string, opts s3client.PutOptions) (string, error) {
		return c.PutObject(ctx, "bkt", "k", bytes.NewReader([]byte(v)), int64(len(v)), opts)
	}
	etag, err := put("v1", s3client.PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put("v2", s3client.PutOptions{IfNoneMatch: true}); !s3client.IsPreconditionFailed(err) {
		t.Fatalf("conditional PUT over an existing key: %v, want 412", err)
	}

	r.IgnoreConditionals.Store(true)
	if _, err := put("v2", s3client.PutOptions{IfNoneMatch: true}); err != nil {
		t.Fatalf("If-None-Match PUT with conditionals ignored: %v", err)
	}
	if _, err := put("v3", s3client.PutOptions{IfMatch: etag}); err != nil {
		t.Fatalf("stale If-Match PUT with conditionals ignored: %v", err)
	}
	if got := readObject(t, c, "k"); string(got) != "v3" {
		t.Fatalf("object = %q, want v3", got)
	}
	// Reads keep their conditions.
	if _, _, err := c.GetObject(ctx, "bkt", "k", s3client.GetOptions{IfMatch: etag}); !s3client.IsPreconditionFailed(err) {
		t.Fatalf("GET with a stale If-Match: %v, want 412", err)
	}
}

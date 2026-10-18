package s3

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// TestCreateBucketBacking covers the x-gos3-backing extension header of
// CreateBucket and HeadBucket.
func TestCreateBucketBacking(t *testing.T) {
	e := newTestEnv(t, withOptions(func(o *Options) {
		r, err := multi.Open(t.Context(), multi.Options{Backings: []multi.Backing{
			{Name: "default", Kind: "memory", Backend: o.Backend},
			{Name: "archive", Kind: "memory", Backend: memory.New()},
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		o.Backend = r
	}))
	tests := []struct {
		name, bucket, header string
		status               int
		code, backing        string
	}{
		{name: "default", bucket: "bk-default", status: 200, backing: "default"},
		{name: "explicit default", bucket: "bk-local", header: "default", status: 200, backing: "default"},
		{name: "configured", bucket: "bk-archive", header: "archive", status: 200, backing: "archive"},
		{name: "case and space", bucket: "bk-upper", header: " Archive ", status: 200, backing: "archive"},
		{name: "unknown", bucket: "bk-unknown", header: "nope", status: 400, code: "InvalidArgument"},
		{name: "malformed", bucket: "bk-bad", header: "a b", status: 400, code: "InvalidArgument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr := http.Header{}
			if tc.header != "" {
				hdr.Set("X-Gos3-Backing", tc.header)
			}
			resp := e.signed(http.MethodPut, "/"+tc.bucket, nil, hdr)
			bktExpect(t, resp, tc.status, tc.code)
			head := e.signed(http.MethodHead, "/"+tc.bucket, nil, nil)
			if tc.status != 200 {
				if head.StatusCode != 404 {
					t.Errorf("bucket created despite %d: HEAD %d", tc.status, head.StatusCode)
				}
				return
			}
			if got := head.Header.Get("X-Gos3-Backing"); got != tc.backing {
				t.Errorf("x-gos3-backing = %q, want %q", got, tc.backing)
			}
		})
	}
}

// TestBackingErrorMapping checks how the backing sentinels reach clients.
func TestBackingErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{storage.ErrInvalidBacking, "InvalidArgument", 400},
		{storage.ErrNotSupported, "NotImplemented", 501},
		{fmt.Errorf("wrapped: %w", storage.ErrBackingUnavailable), "ServiceUnavailable", 503},
		{storage.ErrResourceLimit, "ServiceUnavailable", 503},
	} {
		var se *s3err.Error
		if !errors.As(mapStorageError(tc.err), &se) || se.Code != tc.code || se.HTTPStatus != tc.status {
			t.Errorf("%v → %v", tc.err, mapStorageError(tc.err))
		}
	}
}

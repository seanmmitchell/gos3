package s3

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestResticCompletionRefusedAfterScopeChange: completion decides an
// upload's encryption again, so it applies the restic recursion guard with
// the completing request's User-Agent. restic's own multipart upload,
// started while its bucket archived only SSE writes, is refused when it
// completes after the bucket was switched to archive every write (scope
// "all"), instead of being archived through restic.
func TestResticCompletionRefusedAfterScopeChange(t *testing.T) {
	e, ab := newArchiveEnv(t)
	e.createBucket("repo")
	e.putBucketConfig("repo", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"sse"}`)
	restic := http.Header{"User-Agent": {storage.ResticUserAgent + "/0.17.3"}}

	// start creates an upload of key with one part, as restic, and returns
	// the upload ID and the part's ETag.
	start := func(key string) (string, string) {
		t.Helper()
		r := e.signed(http.MethodPost, "/repo/"+key+"?uploads", nil, restic)
		objExpect(t, r, 200, "")
		id := between(string(r.Data), "<UploadId>", "</UploadId>")
		r = e.signed(http.MethodPut, mpPath("repo", key, mpQuery("partNumber", "1", "uploadId", id)), []byte("pack"), restic)
		objExpect(t, r, 200, "")
		return id, r.Header.Get("ETag")
	}
	complete := func(key, id, etag string, hdr http.Header) *testResponse {
		t.Helper()
		return e.signed(http.MethodPost, mpPath("repo", key, mpQuery("uploadId", id)), mpCompleteBody("1", etag), hdr)
	}
	errorCode := func(r *testResponse) string {
		return between(string(r.Data), "<Code>", "</Code>")
	}

	// Under scope "sse", restic's plain upload completes unencrypted.
	okID, okETag := start("data/ok")
	pendingID, pendingETag := start("data/pending")
	ab.last("repo")
	if r := complete("data/ok", okID, okETag, restic); r.StatusCode != 200 || errorCode(r) != "" {
		t.Fatalf("completion under scope sse: %d %s", r.StatusCode, r.Data)
	}
	if enc, prepared := ab.last("repo"); enc != "" || prepared {
		t.Fatalf("completion under scope sse: write encryption %q, restic prepared %v", enc, prepared)
	}

	// After the switch to scope "all", restic's completion is refused and
	// nothing is written.
	e.putBucketConfig("repo", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"all"}`)
	r := complete("data/pending", pendingID, pendingETag, restic)
	if errorCode(r) != "InvalidRequest" || !strings.Contains(string(r.Data), "restic repository") {
		t.Fatalf("restic completion under scope all: %d %s", r.StatusCode, r.Data)
	}
	if enc, prepared := ab.last("repo"); enc != "<none>" || prepared {
		t.Fatalf("refused completion: write encryption %q, restic prepared %v", enc, prepared)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "repo", "data/pending", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("refused completion stored the object: %v", err)
	}
}

package native_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestInvalidPrefixDoesNotScanRemote(t *testing.T) {
	var lists atomic.Int64
	h := newHarness(t, s3clienttest.Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("list-type") || r.URL.Query().Has("versions") {
				lists.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	}}, nil)
	for _, versioned := range []bool{false, true} {
		bucket := "plain"
		if versioned {
			bucket = "versioned"
			h.mkVersioned(bucket)
		} else {
			h.mk(bucket, storage.CreateBucketOptions{})
		}
		lists.Store(0)
		if got := h.walkKeys(bucket, "invalid\xff", ""); len(got) != 0 {
			t.Fatalf("invalid prefix matched: %v", got)
		}
		if got := h.versions(bucket, "invalid\xff", "", ""); len(got) != 0 {
			t.Fatalf("invalid prefix versions: %v", got)
		}
		if n := lists.Load(); n != 0 {
			t.Fatalf("invalid prefix made %d remote listings", n)
		}
	}
}

func TestForeignExtensionCannotSetObjectLock(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	ext := base64.RawURLEncoding.EncodeToString([]byte(`{"lh":true,"rm":"COMPLIANCE","ru":"2099-01-01T00:00:00Z"}`))
	h.remotePut("nb-b", "foreign", []byte("data"), http.Header{"X-Amz-Meta-Gos3-Meta": {ext}})
	got := h.info("b", "foreign", "")
	if got.LegalHold || got.RetentionMode != "" || !got.RetainUntil.IsZero() {
		t.Fatalf("foreign writer forged lock: %+v", got.ObjectMeta)
	}
	h.mustPut("b", "local", []byte("data"), storage.ObjectMeta{LegalHold: true})
	h.open()
	if !h.info("b", "local", "").LegalHold {
		t.Fatal("trusted local legal hold lost after reopen")
	}
}

// Emulate a service omitting the null-version response header. Later
// version-scoped mutations and seeks must still send versionId=null.
func TestMissingVersionHeaderKeepsNullTarget(t *testing.T) {
	var bad atomic.Bool
	h := newHarness(t, s3clienttest.Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/nb-b/k" && (r.Method == http.MethodDelete || r.URL.Query().Has("tagging")) && !r.URL.Query().Has("versionId") {
				bad.Store(true)
			}
			if r.URL.Path == "/nb-b/k" && r.URL.Query().Get("versionId") == "null" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !r.URL.Query().Has("tagging") {
				rr := httptest.NewRecorder()
				next.ServeHTTP(rr, r)
				for k, vs := range rr.Header() {
					if http.CanonicalHeaderKey(k) != "X-Amz-Version-Id" {
						w.Header()[k] = vs
					}
				}
				w.WriteHeader(rr.Code)
				w.Write(rr.Body.Bytes())
				return
			}
			next.ServeHTTP(w, r)
		})
	}}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("old null"), storage.ObjectMeta{})
	if err := h.SetBucketVersioning(t.Context(), "b", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	latest := h.mustPut("b", "k", []byte("latest"), storage.ObjectMeta{})
	if _, err := h.UpdateObjectMeta(t.Context(), "b", "k", storage.NullVersionID, func(m *storage.ObjectMeta) error { m.Tags = map[string]string{"scope": "null"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DeleteObject(t.Context(), "b", "k", storage.NullVersionID); err != nil {
		t.Fatal(err)
	}
	if bad.Load() {
		t.Fatal("null-version operation targeted latest")
	}
	if got := h.info("b", "k", ""); got.VersionID != latest.VersionID {
		t.Fatalf("latest changed: %+v", got)
	}
}

package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestDeleteObjectsSharesAuthorizationBudget(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putPolicy("delete", `{"Statement":[{"Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"},`+
		`{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringLike":{"aws:UserAgent":"*`+strings.Repeat("a", 512)+`b"}}}]}`)
	u := e.createUser("deleter", "delete")
	body := []byte("<Delete>" + strings.Repeat("<Object><Key>k</Key></Object>", 1000) + "</Delete>")
	r := e.signedAs(u, http.MethodPost, "/b?delete", body, objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16(body)), "User-Agent", strings.Repeat("a", 1024)))
	if r.StatusCode != 200 || !strings.Contains(string(r.Data), "<Code>AccessDenied</Code>") {
		t.Fatalf("bulk budget not enforced: %d %s", r.StatusCode, r.Data)
	}
	if n := strings.Count(string(r.Data), "<Deleted>"); n == 0 || n >= 100 {
		t.Fatalf("authorized %d expensive deletes before exhaustion", n)
	}
}

func TestDummyVersionCannotHideExistingTags(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "k", []byte("protected"), storage.ObjectMeta{Tags: map[string]string{"protected": "yes"}})
	e.putPolicy("write", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/protected":"yes"}}}]}`)
	u := e.createUser("writer", "write")
	for _, path := range []string{"/b/k", "/b/k?versionId=dummy", "/b/k?versionId=null"} {
		objExpect(t, e.signedAs(u, http.MethodPut, path, []byte("overwrite"), nil), 403, "AccessDenied")
	}
}

func TestMultipartCopyRefreshesSourcePermissions(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(map[bool]string{false: "null", true: "versioned"}[versioned], func(t *testing.T) {
			wrapper := &mpRevokeSourceBackend{}
			e := newTestEnv(t, withOptions(func(o *Options) { wrapper.Backend = o.Backend; o.Backend = wrapper }))
			e.createBucket("b")
			if versioned {
				if err := e.backend.SetBucketVersioning(context.Background(), "b", storage.VersioningEnabled); err != nil {
					t.Fatal(err)
				}
			}
			first := e.putObject("b", "source", []byte("private"), storage.ObjectMeta{Tags: map[string]string{"allow": "yes"}})
			e.putPolicy("copy", `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/source","Condition":{"StringEquals":{"s3:ExistingObjectTag/allow":"yes"}}},
				{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/dest"}]}`)
			u := e.createUser("copier", "copy")
			id := mpCreateFor(t, e, u, "dest", nil)
			wrapper.revoke = func() {
				if _, err := e.backend.UpdateObjectMeta(context.Background(), "b", "source", first.VersionID, func(m *storage.ObjectMeta) error { m.Tags["allow"] = "no"; return nil }); err != nil {
					t.Error(err)
				}
				if versioned {
					e.putObject("b", "source", []byte("new"), storage.ObjectMeta{Tags: map[string]string{"allow": "yes"}})
				}
			}
			r := e.signedAs(u, http.MethodPut, mpPath("b", "dest", mpQuery("uploadId", id, "partNumber", "1")), nil, objHdr("X-Amz-Copy-Source", "/b/source"))
			if r.code() != "AccessDenied" {
				t.Fatalf("revoked source copied: %d %s", r.StatusCode, r.Data)
			}
			parts, err := e.backend.ListParts(context.Background(), "b", "dest", id)
			if err != nil || len(parts) != 0 {
				t.Fatalf("part published: %+v, %v", parts, err)
			}
		})
	}
}

func TestUnknownHostPostDoesNotRevealBucket(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("exists")
	for _, bucket := range []string{"exists", "missing"} {
		r := httptest.NewRequest(http.MethodPost, "/"+bucket, nil)
		r.Header.Set("Content-Type", "multipart/form-data; boundary=test")
		r = r.WithContext(WithUnknownHost(r.Context()))
		w := httptest.NewRecorder()
		e.server.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d: %s", bucket, w.Code, w.Body.String())
		}
	}
}

type failingAuthzLookup struct{ storage.Backend }

func (b *failingAuthzLookup) GetObjectInfo(context.Context, string, string, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, errors.New("injected metadata failure")
}

func TestAuthorizationLookupFailureCannotHideDenyTags(t *testing.T) {
	e := newTestEnv(t, withOptions(func(o *Options) { o.Backend = &failingAuthzLookup{Backend: o.Backend} }))
	e.createBucket("b")
	e.putPolicy("write", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/protected":"yes"}}}]}`)
	u := e.createUser("writer", "write")
	r := e.signedAs(u, http.MethodPut, "/b/k", []byte("data"), nil)
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("metadata failure bypassed Deny: %d %s", r.StatusCode, r.Data)
	}
}

type enableLockAfterBucketRead struct {
	storage.Backend
	after func()
}

func (b *enableLockAfterBucketRead) GetBucket(ctx context.Context, bucket string) (storage.BucketInfo, error) {
	info, err := b.Backend.GetBucket(ctx, bucket)
	if b.after != nil {
		f := b.after
		b.after = nil
		f()
	}
	return info, err
}

func TestBulkDeleteDoesNotUseStaleBucketLockFlag(t *testing.T) {
	backend := &enableLockAfterBucketRead{}
	e := newTestEnv(t, withOptions(func(o *Options) { backend.Backend = o.Backend; o.Backend = backend }))
	e.createBucket("b")
	ctx := context.Background()
	if err := e.backend.SetBucketVersioning(ctx, "b", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	v := e.putObject("b", "k", []byte("protected"), storage.ObjectMeta{})
	backend.after = func() {
		if err := e.backend.SetBucketObjectLock(ctx, "b", true); err != nil {
			t.Error(err)
		}
		if _, err := e.backend.UpdateObjectMeta(ctx, "b", "k", v.VersionID, func(m *storage.ObjectMeta) error { m.LegalHold = true; return nil }); err != nil {
			t.Error(err)
		}
	}
	body := []byte("<Delete><Object><Key>k</Key><VersionId>" + v.VersionID + "</VersionId></Object></Delete>")
	r := e.signed(http.MethodPost, "/b?delete", body, objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16(body))))
	if r.StatusCode != 200 || !strings.Contains(string(r.Data), "<Code>AccessDenied</Code>") {
		t.Fatalf("stale flag allowed permanent deletion: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(ctx, "b", "k", v.VersionID); err != nil {
		t.Fatalf("locked version lost: %v", err)
	}
}

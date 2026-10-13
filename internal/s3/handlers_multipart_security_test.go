package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func mpCreateFor(t *testing.T, e *testEnv, who auth.Credentials, key string, hdr http.Header) string {
	t.Helper()
	r := e.signedAs(who, http.MethodPost, "/b/"+key+"?uploads", nil, hdr)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("create upload: %d %s", r.StatusCode, r.Data)
	}
	var doc struct {
		ID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(r.Data, &doc); err != nil || doc.ID == "" {
		t.Fatalf("create upload XML: %s: %v", r.Data, err)
	}
	return doc.ID
}

func mpPartFor(t *testing.T, e *testEnv, who auth.Credentials, key, id string) string {
	t.Helper()
	r := e.signedAs(who, http.MethodPut, mpPath("b", key, mpQuery("uploadId", id, "partNumber", "1")), []byte("part"), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("upload part: %d %s", r.StatusCode, r.Data)
	}
	return r.Header.Get("ETag")
}

func mpCompleteFor(e *testEnv, who auth.Credentials, key, id, etag string) *testResponse {
	return e.signedAs(who, http.MethodPost, mpPath("b", key, mpQuery("uploadId", id)), mpCompleteBody("1", etag), nil)
}

// mpBeforeCompleteBackend changes security state after the handler's checks
// and before the backend begins its potentially long completion work.
type mpBeforeCompleteBackend struct {
	storage.Backend
	before func() error
}

func (b *mpBeforeCompleteBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, id string, parts []int,
	meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if b.before != nil {
		if err := b.before(); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	return b.Backend.CompleteMultipartUpload(ctx, bucket, key, id, parts, meta, opts)
}

func TestMultipartDefaultRetentionUsesVersionCreation(t *testing.T) {
	e := newTestEnv(t)
	if err := e.backend.CreateBucket(context.Background(), "b", storage.CreateBucketOptions{
		Region: "us-east-1", ObjectLockEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	lockConfig := func(days int) string {
		return `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>` +
			strconv.Itoa(days) + `</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	}
	e.putBucketConfig("b", storage.ConfigObjectLock, lockConfig(2))
	id := mpCreateFor(t, e, testRoot, "default", nil)
	etag := mpPartFor(t, e, testRoot, "default", id)
	e.putBucketConfig("b", storage.ConfigObjectLock, lockConfig(3))
	if r := mpCompleteFor(e, testRoot, "default", id, etag); r.StatusCode != http.StatusOK {
		t.Fatalf("complete: %d %s", r.StatusCode, r.Data)
	}
	info, err := e.backend.GetObjectInfo(context.Background(), "b", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.RetentionMode != "COMPLIANCE" || !info.RetainUntil.Equal(info.LastModified.AddDate(0, 0, 3)) || info.MultipartRetentionSource != "" {
		t.Fatalf("stored retention: %+v", info.ObjectMeta)
	}

	// An upload started without a default must receive a default enabled
	// before completion, when its object version actually comes into being.
	e.putBucketConfig("b", storage.ConfigObjectLock, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
	id = mpCreateFor(t, e, testRoot, "new-default", nil)
	etag = mpPartFor(t, e, testRoot, "new-default", id)
	e.putBucketConfig("b", storage.ConfigObjectLock, lockConfig(4))
	if r := mpCompleteFor(e, testRoot, "new-default", id, etag); r.StatusCode != http.StatusOK {
		t.Fatalf("complete after default enabled: %d %s", r.StatusCode, r.Data)
	}
	info, err = e.backend.GetObjectInfo(context.Background(), "b", "new-default", "")
	if err != nil || info.RetentionMode != "COMPLIANCE" || !info.RetainUntil.Equal(info.LastModified.AddDate(0, 0, 4)) {
		t.Fatalf("new default retention: %+v, %v", info.ObjectMeta, err)
	}

	// An explicit deadline must not be replaced by the bucket default.
	deadline := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second)
	h := http.Header{}
	h.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
	h.Set("X-Amz-Object-Lock-Retain-Until-Date", deadline.Format(time.RFC3339))
	id = mpCreateFor(t, e, testRoot, "explicit", h)
	etag = mpPartFor(t, e, testRoot, "explicit", id)
	if r := mpCompleteFor(e, testRoot, "explicit", id, etag); r.StatusCode != http.StatusOK {
		t.Fatalf("complete explicit: %d %s", r.StatusCode, r.Data)
	}
	info, err = e.backend.GetObjectInfo(context.Background(), "b", "explicit", "")
	if err != nil || info.RetentionMode != "GOVERNANCE" || !info.RetainUntil.Equal(deadline) {
		t.Fatalf("explicit retention: %+v, %v", info.ObjectMeta, err)
	}
}

func TestMultipartPublicACLCheckedAtPublication(t *testing.T) {
	wrapper := &mpBeforeCompleteBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	h := http.Header{}
	h.Set("X-Amz-Acl", "public-read")
	id := mpCreateFor(t, e, testRoot, "k", h)
	etag := mpPartFor(t, e, testRoot, "k", id)
	wrapper.before = func() error {
		return e.backend.PutBucketConfig(context.Background(), "b", storage.ConfigPublicAccessBlock,
			[]byte(`<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`))
	}
	if r := mpCompleteFor(e, testRoot, "k", id, etag); r.code() != "AccessDenied" {
		t.Fatalf("public ACL committed after PAB update: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object published: %v", err)
	}
}

func TestMultipartCompletionChecksCurrentAttributePermissions(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putPolicy("initiator", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::b/*"}]}`)
	e.putPolicy("writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	alice := e.createUser("alice", "initiator")
	bob := e.createUser("bob", "writer")
	h := http.Header{}
	h.Set("X-Amz-Acl", "public-read")
	h.Set("X-Amz-Tagging", "role=privileged")
	id := mpCreateFor(t, e, alice, "k", h)
	etag := mpPartFor(t, e, bob, "k", id)
	e.putBucketConfig("b", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":["s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::b/*"}]}`)
	if r := mpCompleteFor(e, bob, "k", id, etag); r.code() != "AccessDenied" {
		t.Fatalf("completion applied denied attributes: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object published: %v", err)
	}
}

func TestMultipartCompletionChecksObjectLockPermissions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "legal-hold", header: http.Header{"X-Amz-Object-Lock-Legal-Hold": []string{"ON"}}},
		{name: "retention", header: http.Header{
			"X-Amz-Object-Lock-Mode":              []string{"GOVERNANCE"},
			"X-Amz-Object-Lock-Retain-Until-Date": []string{time.Now().UTC().AddDate(0, 0, 7).Format(time.RFC3339)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if err := e.backend.CreateBucket(context.Background(), "b", storage.CreateBucketOptions{
				Region: "us-east-1", ObjectLockEnabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			e.putPolicy("writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
			bob := e.createUser("bob", "writer")
			id := mpCreateFor(t, e, testRoot, "k", tc.header)
			etag := mpPartFor(t, e, bob, "k", id)
			if r := mpCompleteFor(e, bob, "k", id, etag); r.code() != "AccessDenied" {
				t.Fatalf("completion applied Object Lock attribute without permission: %d %s", r.StatusCode, r.Data)
			}
			if _, err := e.backend.GetObjectInfo(context.Background(), "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
				t.Fatalf("object published: %v", err)
			}
		})
	}
}

func TestMultipartCompletionChecksCredentialsAtPublication(t *testing.T) {
	wrapper := &mpBeforeCompleteBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	e.putPolicy("writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	bob := e.createUser("bob", "writer")
	id := mpCreateFor(t, e, bob, "k", nil)
	etag := mpPartFor(t, e, bob, "k", id)
	wrapper.before = func() error { return e.iam.UpdateAccessKey(context.Background(), bob.AccessKey, iam.KeyInactive) }
	if r := mpCompleteFor(e, bob, "k", id, etag); r.code() != "AccessDenied" {
		t.Fatalf("completion committed after key revocation: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object published: %v", err)
	}
}

func TestMultipartCompletionChecksPolicyAtPublication(t *testing.T) {
	wrapper := &mpBeforeCompleteBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	e.putPolicy("writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	bob := e.createUser("bob", "writer")
	id := mpCreateFor(t, e, bob, "k", nil)
	etag := mpPartFor(t, e, bob, "k", id)
	wrapper.before = func() error {
		return e.backend.PutBucketConfig(context.Background(), "b", storage.ConfigPolicy,
			[]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`))
	}
	if r := mpCompleteFor(e, bob, "k", id, etag); r.code() != "AccessDenied" {
		t.Fatalf("completion committed after policy changed: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object published: %v", err)
	}
}

type mpRevokeOnRead struct {
	io.ReadSeekCloser
	once   sync.Once
	revoke func()
}

func (r *mpRevokeOnRead) Read(p []byte) (int, error) {
	r.once.Do(r.revoke)
	return r.ReadSeekCloser.Read(p)
}

type mpRevokeSourceBackend struct {
	storage.Backend
	revoke func()
}

func (b *mpRevokeSourceBackend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	info, reader, err := b.Backend.OpenObject(ctx, bucket, key, versionID)
	if err == nil && reader != nil && key == "source" {
		reader = &mpRevokeOnRead{ReadSeekCloser: reader, revoke: b.revoke}
	}
	return info, reader, err
}

func TestUploadPartCopyChecksCredentialsAfterSourceRead(t *testing.T) {
	wrapper := &mpRevokeSourceBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	e.putObject("b", "source", []byte("source bytes"), storage.ObjectMeta{})
	e.putPolicy("copy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::b/*"}]}`)
	bob := e.createUser("bob", "copy")
	id := mpCreateFor(t, e, bob, "dest", nil)
	wrapper.revoke = func() {
		if err := e.iam.UpdateAccessKey(context.Background(), bob.AccessKey, iam.KeyInactive); err != nil {
			t.Errorf("deactivate key: %v", err)
		}
	}
	h := http.Header{}
	h.Set("X-Amz-Copy-Source", "/b/source")
	r := e.signedAs(bob, http.MethodPut, mpPath("b", "dest", mpQuery("uploadId", id, "partNumber", "1")), nil, h)
	if r.code() != "AccessDenied" {
		t.Fatalf("part copy committed after key revocation: %d %s", r.StatusCode, r.Data)
	}
	parts, err := e.backend.ListParts(context.Background(), "b", "dest", id)
	if err != nil || len(parts) != 0 {
		t.Fatalf("persisted parts after denial: %+v, %v", parts, err)
	}
}

func TestUploadPartCopyChecksPolicyAfterSourceRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "source", key: "source"},
		{name: "destination", key: "dest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapper := &mpRevokeSourceBackend{}
			e := newTestEnv(t, withOptions(func(o *Options) {
				wrapper.Backend = o.Backend
				o.Backend = wrapper
			}))
			e.createBucket("b")
			e.putObject("b", "source", []byte("source bytes"), storage.ObjectMeta{})
			e.putPolicy("copy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::b/*"}]}`)
			bob := e.createUser("bob", "copy")
			id := mpCreateFor(t, e, bob, "dest", nil)
			wrapper.revoke = func() {
				e.putBucketConfig("b", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/`+tc.key+`"}]}`)
			}
			h := http.Header{}
			h.Set("X-Amz-Copy-Source", "/b/source")
			r := e.signedAs(bob, http.MethodPut, mpPath("b", "dest", mpQuery("uploadId", id, "partNumber", "1")), nil, h)
			if r.code() != "AccessDenied" {
				t.Fatalf("part copy committed after policy change: %d %s", r.StatusCode, r.Data)
			}
			parts, err := e.backend.ListParts(context.Background(), "b", "dest", id)
			if err != nil || len(parts) != 0 {
				t.Fatalf("persisted parts after denial: %+v, %v", parts, err)
			}
		})
	}
}

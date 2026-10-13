package s3

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// publicationBackend changes a bucket setting after the handler has read
// and streamed the object, immediately before the backend's atomic commit.
type publicationBackend struct {
	storage.Backend
	before  func(bucket, key string)
	written atomic.Int64
}

type publicationWriter struct {
	storage.ObjectWriter
	backend *publicationBackend
	bucket  string
	key     string
}

func (b *publicationBackend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	w, err := b.Backend.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return &publicationWriter{ObjectWriter: w, backend: b, bucket: bucket, key: key}, nil
}

func (w *publicationWriter) Write(p []byte) (int, error) {
	n, err := w.ObjectWriter.Write(p)
	w.backend.written.Add(int64(n))
	return n, err
}

func (w *publicationWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if w.backend.before != nil {
		w.backend.before(w.bucket, w.key)
	}
	return w.ObjectWriter.Commit(ctx, meta, opts)
}

func writeLockConfig(days int) string {
	base := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>`
	if days > 0 {
		base += `<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>` + strconv.Itoa(days) +
			`</Days></DefaultRetention></Rule>`
	}
	return base + `</ObjectLockConfiguration>`
}

func securityWriteObject(t *testing.T, e *testEnv, method string, explicit time.Time) *testResponse {
	t.Helper()
	h := http.Header{}
	if !explicit.IsZero() {
		h.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
		h.Set("X-Amz-Object-Lock-Retain-Until-Date", explicit.Format(time.RFC3339))
	}
	switch method {
	case "put":
		return e.signed(http.MethodPut, "/b/dest", []byte("data"), h)
	case "copy":
		h.Set("X-Amz-Copy-Source", "/b/source")
		return e.signed(http.MethodPut, "/b/dest", nil, h)
	case "post":
		conditions := []any{map[string]string{"bucket": "b"}, map[string]string{"key": "dest"}}
		extra := []objPostField{{"key", "dest"}}
		if !explicit.IsZero() {
			conditions = append(conditions, map[string]string{"x-amz-object-lock-mode": "GOVERNANCE"},
				map[string]string{"x-amz-object-lock-retain-until-date": explicit.Format(time.RFC3339)})
			extra = append(extra, objPostField{"x-amz-object-lock-mode", "GOVERNANCE"},
				objPostField{"x-amz-object-lock-retain-until-date", explicit.Format(time.RFC3339)})
		}
		fields := objSignedPostFields(t, testRoot, time.Now().Add(time.Hour), conditions, extra...)
		body, ct := objPostBody(t, fields, "file", []byte("data"))
		return objPost(e, "b", body, ct)
	default:
		t.Fatalf("unknown method %q", method)
		return nil
	}
}

func TestObjectDefaultRetentionAtPublication(t *testing.T) {
	for _, method := range []string{"put", "post", "copy"} {
		for _, initial := range []int{0, 2} {
			t.Run(method+"/initial-"+strconv.Itoa(initial), func(t *testing.T) {
				wrapper := &publicationBackend{}
				e := newTestEnv(t, withOptions(func(o *Options) {
					wrapper.Backend = o.Backend
					o.Backend = wrapper
				}))
				if err := e.backend.CreateBucket(context.Background(), "b", storage.CreateBucketOptions{
					Region: "us-east-1", ObjectLockEnabled: true,
				}); err != nil {
					t.Fatal(err)
				}
				e.putBucketConfig("b", storage.ConfigObjectLock, writeLockConfig(initial))
				if method == "copy" {
					e.putObject("b", "source", []byte("data"), storage.ObjectMeta{})
				}
				wrapper.before = func(bucket, key string) {
					if bucket == "b" && key == "dest" {
						e.putBucketConfig("b", storage.ConfigObjectLock, writeLockConfig(3))
					}
				}
				r := securityWriteObject(t, e, method, time.Time{})
				wantStatus := http.StatusOK
				if method == "post" {
					wantStatus = http.StatusNoContent
				}
				if r.StatusCode != wantStatus || r.code() != "" {
					t.Fatalf("write: %d %s", r.StatusCode, r.Data)
				}
				info, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", "")
				if err != nil || info.RetentionMode != "COMPLIANCE" || !info.RetainUntil.Equal(info.LastModified.AddDate(0, 0, 3)) {
					t.Fatalf("retention at publication: %+v, %v", info, err)
				}
			})
		}
	}
}

func TestObjectExplicitRetentionSurvivesDefaultChange(t *testing.T) {
	for _, method := range []string{"put", "post", "copy"} {
		t.Run(method, func(t *testing.T) {
			wrapper := &publicationBackend{}
			e := newTestEnv(t, withOptions(func(o *Options) {
				wrapper.Backend = o.Backend
				o.Backend = wrapper
			}))
			if err := e.backend.CreateBucket(context.Background(), "b", storage.CreateBucketOptions{
				Region: "us-east-1", ObjectLockEnabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			e.putBucketConfig("b", storage.ConfigObjectLock, writeLockConfig(2))
			if method == "copy" {
				e.putObject("b", "source", []byte("data"), storage.ObjectMeta{})
			}
			wrapper.before = func(bucket, key string) {
				if bucket == "b" && key == "dest" {
					e.putBucketConfig("b", storage.ConfigObjectLock, writeLockConfig(3))
				}
			}
			deadline := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second)
			r := securityWriteObject(t, e, method, deadline)
			wantStatus := http.StatusOK
			if method == "post" {
				wantStatus = http.StatusNoContent
			}
			if r.StatusCode != wantStatus || r.code() != "" {
				t.Fatalf("write: %d %s", r.StatusCode, r.Data)
			}
			info, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", "")
			if err != nil || info.RetentionMode != "GOVERNANCE" || !info.RetainUntil.Equal(deadline) {
				t.Fatalf("explicit retention changed: %+v, %v", info, err)
			}
		})
	}
}

func TestCopyObjectRechecksAccessAfterSourceRead(t *testing.T) {
	for _, tc := range []struct {
		name, action, key string
	}{
		{name: "access key"},
		{name: "source read", action: "s3:GetObject", key: "source"},
		{name: "destination write", action: "s3:PutObject", key: "dest"},
		{name: "destination ACL", action: "s3:PutObjectAcl", key: "dest"},
		{name: "source tags", action: "s3:GetObjectTagging", key: "source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapper := &mpRevokeSourceBackend{}
			e := newTestEnv(t, withOptions(func(o *Options) {
				wrapper.Backend = o.Backend
				o.Backend = wrapper
			}))
			e.createBucket("b")
			e.putObject("b", "source", []byte("source data"), storage.ObjectMeta{Tags: map[string]string{"color": "red"}})
			e.putPolicy("copy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectTagging","s3:PutObject","s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::b/*"}]}`)
			caller := e.createUser("caller", "copy")
			wrapper.revoke = func() {
				if tc.action == "" {
					if err := e.iam.UpdateAccessKey(context.Background(), caller.AccessKey, iam.KeyInactive); err != nil {
						t.Errorf("deactivate key: %v", err)
					}
					return
				}
				e.putBucketConfig("b", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"`+tc.action+`","Resource":"arn:aws:s3:::b/`+tc.key+`"}]}`)
			}
			r := e.signedAs(caller, http.MethodPut, "/b/dest", nil,
				objHdr("X-Amz-Copy-Source", "/b/source", "X-Amz-Acl", "public-read"))
			if r.code() != "AccessDenied" {
				t.Fatalf("copy after %s revocation: %d %s", tc.name, r.StatusCode, r.Data)
			}
			if _, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", ""); !errors.Is(err, storage.ErrObjectNotFound) {
				t.Fatalf("destination published after revocation: %v", err)
			}
		})
	}
}

func TestCopyObjectRechecksAccessAtPublication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(*testEnv, string)
	}{
		{name: "access key", revoke: func(e *testEnv, key string) {
			if err := e.iam.UpdateAccessKey(context.Background(), key, iam.KeyInactive); err != nil {
				e.t.Errorf("deactivate key: %v", err)
			}
		}},
		{name: "destination policy", revoke: func(e *testEnv, _ string) {
			e.putBucketConfig("b", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/dest"}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapper := &publicationBackend{}
			e := newTestEnv(t, withOptions(func(o *Options) {
				wrapper.Backend = o.Backend
				o.Backend = wrapper
			}))
			e.createBucket("b")
			e.putObject("b", "source", []byte("source data"), storage.ObjectMeta{})
			e.putPolicy("copy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::b/*"}]}`)
			caller := e.createUser("caller", "copy")
			wrapper.before = func(bucket, key string) {
				if bucket == "b" && key == "dest" {
					tc.revoke(e, caller.AccessKey)
				}
			}
			r := e.signedAs(caller, http.MethodPut, "/b/dest", nil, objHdr("X-Amz-Copy-Source", "/b/source"))
			if r.code() != "AccessDenied" {
				t.Fatalf("copy after %s revocation: %d %s", tc.name, r.StatusCode, r.Data)
			}
			if _, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", ""); !errors.Is(err, storage.ErrObjectNotFound) {
				t.Fatalf("destination published after revocation: %v", err)
			}
		})
	}
}

func TestCopyObjectRechecksCopiedVersionTags(t *testing.T) {
	wrapper := &mpRevokeSourceBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	if err := e.backend.SetBucketVersioning(context.Background(), "b", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	first := e.putObject("b", "source", []byte("private data"), storage.ObjectMeta{Tags: map[string]string{"allow": "yes"}})
	e.putPolicy("copy", `{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/source","Condition":{"StringEquals":{"s3:ExistingObjectTag/allow":"yes"}}},`+
		`{"Effect":"Allow","Action":["s3:GetObjectTagging","s3:PutObject","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::b/*"}]}`)
	caller := e.createUser("caller", "copy")
	wrapper.revoke = func() {
		if _, err := e.backend.UpdateObjectMeta(context.Background(), "b", "source", first.VersionID, func(m *storage.ObjectMeta) error {
			m.Tags["allow"] = "no"
			return nil
		}); err != nil {
			t.Errorf("change copied version tags: %v", err)
		}
		e.putObject("b", "source", []byte("new public data"), storage.ObjectMeta{Tags: map[string]string{"allow": "yes"}})
	}
	r := e.signedAs(caller, http.MethodPut, "/b/dest", nil, objHdr("X-Amz-Copy-Source", "/b/source"))
	if r.code() != "AccessDenied" {
		t.Fatalf("copy used newer version's tags: %d %s", r.StatusCode, r.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("destination published after tag change: %v", err)
	}
}

func TestZeroBytePostPolicyStopsBeforeStorageWrite(t *testing.T) {
	wrapper := &publicationBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wrapper.Backend = o.Backend
		o.Backend = wrapper
	}))
	e.createBucket("b")
	conditions := []any{map[string]string{"bucket": "b"}, map[string]string{"key": "dest"},
		[]any{"content-length-range", 0, 0}}
	fields := objSignedPostFields(t, testRoot, time.Now().Add(time.Hour), conditions, objPostField{"key", "dest"})
	body, ct := objPostBody(t, fields, "file", []byte(strings.Repeat("x", 64*1024)))
	r := objPost(e, "b", body, ct)
	if r.StatusCode != http.StatusBadRequest || r.code() != "EntityTooLarge" {
		t.Fatalf("nonempty zero-byte policy: %d %s", r.StatusCode, r.Data)
	}
	if n := wrapper.written.Load(); n != 0 {
		t.Fatalf("streamed %d bytes to storage before rejecting zero-byte policy", n)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("destination published: %v", err)
	}
	body, ct = objPostBody(t, fields, "file", nil)
	r = objPost(e, "b", body, ct)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("empty file denied: %d %s", r.StatusCode, r.Data)
	}
	info, err := e.backend.GetObjectInfo(context.Background(), "b", "dest", "")
	if err != nil || info.Size != 0 {
		t.Fatalf("empty file not stored: %+v, %v", info, err)
	}
}

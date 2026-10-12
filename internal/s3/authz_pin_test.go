package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// swapBackend runs a one-shot hook right after the next GetObjectInfo of
// a key: the authorization lookup of a read. The hook replaces the object,
// so the handler's open sees another version than authorization did.
type swapBackend struct {
	storage.Backend
	mu    sync.Mutex
	hooks map[string]func()
}

func (b *swapBackend) onNextInfo(bucket, key string, f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hooks == nil {
		b.hooks = map[string]func(){}
	}
	b.hooks[bucket+"/"+key] = f
}

func (b *swapBackend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	info, err := b.Backend.GetObjectInfo(ctx, bucket, key, versionID)
	b.mu.Lock()
	f := b.hooks[bucket+"/"+key]
	delete(b.hooks, bucket+"/"+key)
	b.mu.Unlock()
	if f != nil {
		f()
	}
	return info, err
}

// newSwapEnv is an authorization test environment whose backend is a
// swapBackend over e.backend.
func newSwapEnv(t *testing.T) (*testEnv, *swapBackend) {
	var sb *swapBackend
	e := newTestEnv(t, withOptions(func(o *Options) {
		sb = &swapBackend{Backend: o.Backend}
		o.Backend = sb
	}))
	requireAuthz(t, e)
	return e, sb
}

var (
	publicData = []byte("public data")
	secretData = []byte("SECRET DATA")
)

// TestReadServesAuthorizedVersion: GetObject and HeadObject never serve a
// version that authorization did not evaluate when the object is replaced
// between the authorization lookup (object ACL, s3:ExistingObjectTag) and
// the open.
func TestReadServesAuthorizedVersion(t *testing.T) {
	e, sb := newSwapEnv(t)
	ctx := context.Background()
	e.createBucket("unv")
	e.createBucket("ver")
	if err := e.backend.SetBucketVersioning(ctx, "ver", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	e.createBucket("tag")
	e.putBucketConfig("tag", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::tag/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/public":"true"}}}]}`)

	swap := func(bucket string) {
		sb.onNextInfo(bucket, "k", func() { e.putObject(bucket, "k", secretData, storage.ObjectMeta{}) })
	}
	for _, tc := range []struct {
		name, bucket string
		meta         storage.ObjectMeta
		// versioned: the authorized (old) version is served; otherwise
		// the read fails closed.
		versioned bool
	}{
		{"object ACL, unversioned", "unv", storage.ObjectMeta{ACL: "public-read"}, false},
		{"object ACL, versioned", "ver", storage.ObjectMeta{ACL: "public-read"}, true},
		{"existing object tag", "tag", storage.ObjectMeta{Tags: map[string]string{"public": "true"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.putObject(tc.bucket, "k", publicData, tc.meta)
			// Ordinary reads are unchanged.
			if resp := e.anon("GET", "/"+tc.bucket+"/k", nil, nil); resp.StatusCode != 200 || !bytes.Equal(resp.Data, publicData) {
				t.Fatalf("plain GET: %d %q", resp.StatusCode, resp.Data)
			}
			for _, method := range []string{"GET", "HEAD"} {
				t.Run(method, func(t *testing.T) {
					old := e.putObject(tc.bucket, "k", publicData, tc.meta)
					swap(tc.bucket)
					resp := e.anon(method, "/"+tc.bucket+"/k", nil, nil)
					if bytes.Contains(resp.Data, secretData) || resp.Header.Get("Etag") == `"`+md5Hex(secretData)+`"` {
						t.Fatalf("served the unauthorized version: %d %q", resp.StatusCode, resp.Data)
					}
					switch {
					case tc.versioned:
						if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Version-Id") != old.VersionID {
							t.Fatalf("%d version %q, want the authorized %q", resp.StatusCode, resp.Header.Get("X-Amz-Version-Id"), old.VersionID)
						}
					case resp.StatusCode != 403:
						t.Fatalf("%d, want 403", resp.StatusCode)
					}
				})
			}
		})
	}
}

// TestCopySourceServesAuthorizedVersion: the same for the source of
// CopyObject and UploadPartCopy, authorized by a bucket policy with an
// s3:ExistingObjectTag condition.
func TestCopySourceServesAuthorizedVersion(t *testing.T) {
	e, sb := newSwapEnv(t)
	e.createBucket("src")
	e.createBucket("dst")
	e.putBucketConfig("src", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::src/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/public":"true"}}}]}`)
	e.putPolicy("dstwrite", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::dst/*"}]}`)
	user := e.createUser("copier", "dstwrite")
	tagged := storage.ObjectMeta{Tags: map[string]string{"public": "true"}}
	copyHdr := objHdr("X-Amz-Copy-Source", "/src/k", "X-Amz-Tagging-Directive", "REPLACE")

	t.Run("CopyObject", func(t *testing.T) {
		// Ordinary copies work.
		e.putObject("src", "k", publicData, tagged)
		objExpect(t, e.signedAs(user, "PUT", "/dst/plain", nil, copyHdr), 200, "")

		e.putObject("src", "k", publicData, tagged)
		sb.onNextInfo("src", "k", func() { e.putObject("src", "k", secretData, storage.ObjectMeta{}) })
		objExpect(t, e.signedAs(user, "PUT", "/dst/copy", nil, copyHdr), 403, "AccessDenied")
		if _, err := e.backend.GetObjectInfo(context.Background(), "dst", "copy", ""); err == nil {
			t.Fatal("CopyObject copied the unauthorized version")
		}
	})

	t.Run("UploadPartCopy", func(t *testing.T) {
		resp := e.signedAs(user, "POST", "/dst/mp?uploads", nil, nil)
		objExpect(t, resp, 200, "")
		var up struct {
			UploadID string `xml:"UploadId"`
		}
		if err := xml.Unmarshal(resp.Data, &up); err != nil || up.UploadID == "" {
			t.Fatalf("CreateMultipartUpload: %s %v", resp.Data, err)
		}
		partPath := mpPath("dst", "mp", mpQuery("uploadId", up.UploadID, "partNumber", "1"))
		e.putObject("src", "k", publicData, tagged)
		objExpect(t, e.signedAs(user, "PUT", partPath, nil, objHdr("X-Amz-Copy-Source", "/src/k")), 200, "")
		sb.onNextInfo("src", "k", func() { e.putObject("src", "k", secretData, storage.ObjectMeta{}) })
		objExpect(t, e.signedAs(user, "PUT", partPath, nil, objHdr("X-Amz-Copy-Source", "/src/k")), 403, "AccessDenied")
		parts, err := e.backend.ListParts(context.Background(), "dst", "mp", up.UploadID)
		if err != nil || len(parts) != 1 || parts[0].ETag != md5Hex(publicData) {
			t.Fatalf("parts %+v %v", parts, err)
		}
	})
}

package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// bypassBackend is a memory backend recording, for each object delete and
// metadata update, whether its context carried an approved GOVERNANCE
// bypass (storage.GovernanceBypass), as a native bucket's store reads it.
type bypassBackend struct {
	*memory.Backend

	mu    sync.Mutex
	calls []bypassCall
}

// bypassCall is one recorded backend call.
type bypassCall struct {
	op, key string
	bypass  bool
}

func (b *bypassBackend) record(ctx context.Context, op, key string) {
	b.mu.Lock()
	b.calls = append(b.calls, bypassCall{op: op, key: key, bypass: storage.GovernanceBypass(ctx)})
	b.mu.Unlock()
}

// take returns and forgets the calls recorded so far.
func (b *bypassBackend) take() []bypassCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.calls
	b.calls = nil
	return c
}

func (b *bypassBackend) DeleteObject(ctx context.Context, bucket, key, vid string) (storage.DeleteResult, error) {
	b.record(ctx, "delete", key)
	return b.Backend.DeleteObject(ctx, bucket, key, vid)
}

func (b *bypassBackend) DeleteObjectIf(ctx context.Context, bucket, key, vid string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	b.record(ctx, "delete", key)
	return b.Backend.DeleteObjectIf(ctx, bucket, key, vid, check)
}

func (b *bypassBackend) DeleteObjectChecked(ctx context.Context, bucket, key, vid string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	b.record(ctx, "delete", key)
	return b.Backend.DeleteObjectChecked(ctx, bucket, key, vid, check)
}

func (b *bypassBackend) UpdateObjectMeta(ctx context.Context, bucket, key, vid string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	b.record(ctx, "update", key)
	return b.Backend.UpdateObjectMeta(ctx, bucket, key, vid, fn)
}

func (b *bypassBackend) UpdateObjectMetaIf(ctx context.Context, bucket, key, vid string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	b.record(ctx, "update", key)
	return b.Backend.UpdateObjectMetaIf(ctx, bucket, key, vid, check, fn)
}

// orRoot returns c, or the root credentials when c is the zero value.
func orRoot(c auth.Credentials) auth.Credentials {
	if c.AccessKey == "" {
		return testRoot
	}
	return c
}

// TestGovernanceBypassContext: the S3 layer passes a GOVERNANCE bypass to
// the backend (storage.WithGovernanceBypass) for version deletes
// (DeleteObject, DeleteObjects) and PutObjectRetention exactly when the
// request asked for it and the caller holds s3:BypassGovernanceRetention.
func TestGovernanceBypassContext(t *testing.T) {
	bb := &bypassBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		bb.Backend = o.Backend.(*memory.Backend)
		o.Backend = bb
	}))
	requireAuthz(t, e)
	objLockBucket(t, e, "lock")
	e.createBucket("plain")
	objVersioned(t, e, "plain", storage.VersioningEnabled)

	const actions = `"s3:GetObject","s3:GetObjectVersion","s3:DeleteObject","s3:DeleteObjectVersion","s3:PutObjectRetention"`
	e.putPolicy("writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[`+actions+`],"Resource":["arn:aws:s3:::*/*"]}]}`)
	e.putPolicy("bypasser", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[`+actions+`,"s3:BypassGovernanceRetention"],"Resource":["arn:aws:s3:::*/*"]}]}`)
	writer := e.createUser("writer", "writer")
	bypasser := e.createUser("bypasser", "bypasser")

	future := time.Now().Add(time.Hour)
	put := func(bucket, key, mode string) string {
		m := storage.ObjectMeta{}
		if mode != "" {
			m.RetentionMode, m.RetainUntil = mode, future
		}
		return e.putObject(bucket, key, []byte("x"), m).VersionID
	}
	bypassHdr := func(kv ...string) http.Header {
		return objHdr(append([]string{"X-Amz-Bypass-Governance-Retention", "true"}, kv...)...)
	}
	// expect checks that the request made backend calls of op and that
	// each carried the bypass exactly when want.
	expect := func(t *testing.T, op string, want bool) {
		t.Helper()
		calls := bb.take()
		n := 0
		for _, c := range calls {
			if c.op != op {
				continue
			}
			n++
			if c.bypass != want {
				t.Errorf("%s %s: bypass in context = %v, want %v", c.op, c.key, c.bypass, want)
			}
		}
		if n == 0 {
			t.Errorf("no %s call reached the backend (calls: %+v)", op, calls)
		}
	}
	bb.take()

	t.Run("DeleteObject", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			bucket     string
			mode       string
			noVersion  bool
			as         auth.Credentials // zero: root
			header     bool
			status     int
			wantBypass bool
		}{
			{name: "root with header", bucket: "lock", mode: "GOVERNANCE", header: true, status: 204, wantBypass: true},
			{name: "root without header", bucket: "lock", mode: "GOVERNANCE", status: 403},
			{name: "header without permission", bucket: "lock", mode: "GOVERNANCE", as: writer, header: true, status: 403},
			{name: "permission with header", bucket: "lock", mode: "GOVERNANCE", as: bypasser, header: true, status: 204, wantBypass: true},
			{name: "permission without header", bucket: "lock", mode: "GOVERNANCE", as: bypasser, status: 403},
			{name: "unlocked version with header", bucket: "lock", header: true, status: 204, wantBypass: true},
			{name: "unlocked version without header", bucket: "lock", status: 204},
			{name: "delete marker with header", bucket: "lock", mode: "GOVERNANCE", noVersion: true, header: true, status: 204},
			// gos3 may not know the lock of a native bucket's remote bucket:
			// an approved bypass is passed on regardless.
			{name: "bucket without lock, header", bucket: "plain", header: true, status: 204, wantBypass: true},
			{name: "bucket without lock, header without permission", bucket: "plain", as: writer, header: true, status: 204},
			{name: "bucket without lock, no header", bucket: "plain", status: 204},
		} {
			t.Run(tc.name, func(t *testing.T) {
				vid := put(tc.bucket, "del", tc.mode)
				path := "/" + tc.bucket + "/del?versionId=" + vid
				if tc.noVersion {
					path = "/" + tc.bucket + "/del"
				}
				var hdr http.Header
				if tc.header {
					hdr = bypassHdr()
				}
				resp := e.signedAs(orRoot(tc.as), http.MethodDelete, path, nil, hdr)
				if resp.StatusCode != tc.status {
					t.Fatalf("DELETE: %d %s, want %d", resp.StatusCode, resp.Data, tc.status)
				}
				expect(t, "delete", tc.wantBypass)
			})
		}
	})

	t.Run("DeleteObjects", func(t *testing.T) {
		v1, v2 := put("lock", "m1", "GOVERNANCE"), put("lock", "m2", "GOVERNANCE")
		body := `<Delete><Object><Key>m1</Key><VersionId>` + v1 + `</VersionId></Object>` +
			`<Object><Key>m2</Key><VersionId>` + v2 + `</VersionId></Object></Delete>`
		sum := md5.Sum([]byte(body))
		md5Hdr := base64.StdEncoding.EncodeToString(sum[:])

		resp := e.signedAs(writer, http.MethodPost, "/lock?delete", []byte(body), bypassHdr("Content-MD5", md5Hdr))
		if resp.StatusCode != 200 || strings.Count(string(resp.Data), "<Code>AccessDenied</Code>") != 2 {
			t.Fatalf("without permission: %d %s", resp.StatusCode, resp.Data)
		}
		expect(t, "delete", false)

		resp = e.signed(http.MethodPost, "/lock?delete", []byte(body), objHdr("Content-MD5", md5Hdr))
		if resp.StatusCode != 200 || strings.Count(string(resp.Data), "<Code>AccessDenied</Code>") != 2 {
			t.Fatalf("without header: %d %s", resp.StatusCode, resp.Data)
		}
		expect(t, "delete", false)

		resp = e.signed(http.MethodPost, "/lock?delete", []byte(body), bypassHdr("Content-MD5", md5Hdr))
		if resp.StatusCode != 200 || strings.Count(string(resp.Data), "<Deleted>") != 2 {
			t.Fatalf("bypass: %d %s", resp.StatusCode, resp.Data)
		}
		calls := bb.take()
		if len(calls) != 2 {
			t.Fatalf("calls = %+v, want one delete per key", calls)
		}
		for _, c := range calls {
			if c.op != "delete" || !c.bypass {
				t.Errorf("%+v: want a delete with the bypass", c)
			}
		}
	})

	t.Run("PutObjectRetention", func(t *testing.T) {
		remove := []byte(`<Retention/>`)
		extend := []byte(`<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>` +
			future.Add(time.Hour).UTC().Format(time.RFC3339) + `</RetainUntilDate></Retention>`)
		for _, tc := range []struct {
			name       string
			body       []byte
			as         auth.Credentials // zero: root
			header     bool
			status     int
			wantBypass bool
		}{
			{name: "root shortens with header", body: remove, header: true, status: 200, wantBypass: true},
			{name: "root shortens without header", body: remove, status: 403},
			{name: "header without permission", body: remove, as: writer, header: true, status: 403},
			{name: "permission with header", body: remove, as: bypasser, header: true, status: 200, wantBypass: true},
			{name: "extend without header", body: extend, status: 200},
			{name: "extend with header", body: extend, header: true, status: 200, wantBypass: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				vid := put("lock", "ret", "GOVERNANCE")
				path := "/lock/ret?retention&versionId=" + vid
				var hdr http.Header
				if tc.header {
					hdr = bypassHdr()
				}
				resp := e.signedAs(orRoot(tc.as), http.MethodPut, path, tc.body, hdr)
				if resp.StatusCode != tc.status {
					t.Fatalf("PUT retention: %d %s, want %d", resp.StatusCode, resp.Data, tc.status)
				}
				expect(t, "update", tc.wantBypass)
			})
		}
	})
}

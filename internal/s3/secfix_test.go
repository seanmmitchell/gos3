package s3

// Regression tests for the TOCTOU, revocation, metadata, DeleteObjects,
// PostObject, CORS and lifecycle-cache hardening of the object handlers.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// hookBackend is the memory backend with one-shot hooks run right after
// some calls for a bucket/key. It embeds *memory.Backend, so the optional
// storage interfaces (conditional delete and update, config view) stay
// available.
type hookBackend struct {
	*memory.Backend
	mu    sync.Mutex
	hooks map[string]func() // "<call> <bucket>/<key>"
}

// after registers f to run once after the next call (GetObjectInfo,
// Delete, NewObjectWriter) for bucket/key.
func (b *hookBackend) after(call, bucket, key string, f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hooks == nil {
		b.hooks = map[string]func(){}
	}
	b.hooks[call+" "+bucket+"/"+key] = f
}

func (b *hookBackend) fire(call, bucket, key string) {
	b.mu.Lock()
	f := b.hooks[call+" "+bucket+"/"+key]
	delete(b.hooks, call+" "+bucket+"/"+key)
	b.mu.Unlock()
	if f != nil {
		f()
	}
}

func (b *hookBackend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	info, err := b.Backend.GetObjectInfo(ctx, bucket, key, versionID)
	b.fire("GetObjectInfo", bucket, key)
	return info, err
}

func (b *hookBackend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	res, err := b.Backend.DeleteObject(ctx, bucket, key, versionID)
	b.fire("Delete", bucket, key)
	return res, err
}

func (b *hookBackend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	res, err := b.Backend.DeleteObjectIf(ctx, bucket, key, versionID, check)
	b.fire("Delete", bucket, key)
	return res, err
}

func (b *hookBackend) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	res, err := b.Backend.DeleteObjectChecked(ctx, bucket, key, versionID, check)
	b.fire("Delete", bucket, key)
	return res, err
}

func (b *hookBackend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	data, err := b.Backend.GetBucketConfig(ctx, bucket, key)
	b.fire("GetBucketConfig", bucket, key)
	return data, err
}

func (b *hookBackend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	w, err := b.Backend.NewObjectWriter(ctx, bucket, key)
	b.fire("NewObjectWriter", bucket, key)
	return w, err
}

// newHookEnv is a test environment whose server uses a hookBackend over
// e.backend (direct e.backend calls do not fire hooks).
func newHookEnv(t *testing.T) (*testEnv, *hookBackend) {
	var hb *hookBackend
	e := newTestEnv(t, withOptions(func(o *Options) {
		hb = &hookBackend{Backend: o.Backend.(*memory.Backend)}
		o.Backend = hb
	}))
	requireAuthz(t, e)
	return e, hb
}

// md5Header is a Content-MD5 header for body.
func md5Header(body string) http.Header {
	return objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16([]byte(body))))
}

// stalledRequest starts a signed request whose body (declared length n,
// signed payload hash of payload) is written by the caller through the
// returned pipe; the response arrives on the channel.
func stalledRequest(t *testing.T, e *testEnv, c auth.Credentials, method, path string, hdr http.Header, payload []byte) (*io.PipeWriter, <-chan *testResponse) {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(method, e.url(path), pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	sum := sha256.Sum256(payload)
	if err := auth.SignRequest(req, c, "us-east-1", "s3", hex.EncodeToString(sum[:]), time.Now()); err != nil {
		t.Fatal(err)
	}
	out := make(chan *testResponse, 1)
	go func() {
		resp, err := e.httpc.Do(req)
		if err != nil {
			out <- &testResponse{Response: &http.Response{StatusCode: -1}, Data: []byte(err.Error())}
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out <- &testResponse{Response: resp, Data: data}
	}()
	t.Cleanup(func() { _ = pw.CloseWithError(errors.New("test over")) })
	return pw, out
}

// waitFor waits for ch to be closed.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// receive waits for the response of a stalled request.
func receive(t *testing.T, ch <-chan *testResponse) *testResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the response")
		return nil
	}
}

const protectedDenyPolicy = `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*",` +
	`"Condition":{"StringEquals":{"s3:ExistingObjectTag/protected":"true"}}}]}`

var protectedMeta = storage.ObjectMeta{Tags: map[string]string{"protected": "true"}}

// TestDeleteTiedToAuthorizedVersion: a delete decided on an object's tags
// (s3:ExistingObjectTag) never removes another object that took its place
// after the decision (gap-authz-toctou-mutations-and-iam-races-1).
func TestDeleteTiedToAuthorizedVersion(t *testing.T) {
	e, hb := newHookEnv(t)
	e.createBucket("b")
	e.putBucketConfig("b", storage.ConfigPolicy, protectedDenyPolicy)
	e.putPolicy("del", `{"Statement":[{"Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}]}`)
	alice := e.createUser("alice", "del")
	e.putObject("b", "p", []byte("x"), protectedMeta)
	objExpect(t, e.signedAs(alice, "DELETE", "/b/p", nil, nil), 403, "AccessDenied")
	e.putObject("b", "plain", []byte("x"), storage.ObjectMeta{})
	objExpect(t, e.signedAs(alice, "DELETE", "/b/plain", nil, nil), 204, "")

	// DeleteObject: the protected object is written right after the
	// authorization looked at the unprotected one.
	e.putObject("b", "k", []byte("old"), storage.ObjectMeta{})
	hb.after("GetObjectInfo", "b", "k", func() { e.putObject("b", "k", []byte("victim"), protectedMeta) })
	objExpect(t, e.signedAs(alice, "DELETE", "/b/k", nil, nil), 403, "AccessDenied")
	if got := e.objectData("b", "k"); got != "victim" {
		t.Fatalf("protected object after the raced DeleteObject: %q", got)
	}

	// DeleteObjects naming the key twice: the second entry is not decided
	// on the lookup made for the first one.
	e.putObject("b", "d", []byte("old"), storage.ObjectMeta{})
	hb.after("Delete", "b", "d", func() { e.putObject("b", "d", []byte("victim"), protectedMeta) })
	body := `<Delete><Object><Key>d</Key></Object><Object><Key>d</Key></Object></Delete>`
	resp := e.signedAs(alice, "POST", "/b?delete", []byte(body), md5Header(body))
	objExpect(t, resp, 200, "")
	var res struct {
		Deleted []struct{ Key string }
		Error   []struct{ Key, Code string }
	}
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || len(res.Error) != 1 || res.Error[0].Code != "AccessDenied" {
		t.Fatalf("DeleteObjects result: %s", resp.Data)
	}
	if got := e.objectData("b", "d"); got != "victim" {
		t.Fatalf("protected object after DeleteObjects: %q", got)
	}
}

// TestPutObjectAclAppliesToAuthorizedVersion: a PutObjectAcl whose body is
// delayed cannot publish an object that replaced the one it was authorized
// for (gap-authz-toctou-mutations-and-iam-races-2).
func TestPutObjectAclAppliesToAuthorizedVersion(t *testing.T) {
	e, hb := newHookEnv(t)
	e.createBucket("b")
	e.putPolicy("acl", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObjectAcl","Resource":"arn:aws:s3:::b/*",`+
		`"Condition":{"StringEquals":{"s3:ExistingObjectTag/owner":"alice"}}}]}`)
	alice := e.createUser("alice", "acl")
	e.putObject("b", "bobs", []byte("x"), storage.ObjectMeta{Tags: map[string]string{"owner": "bob"}})
	objExpect(t, e.signedAs(alice, "PUT", "/b/bobs?acl", nil, objHdr("X-Amz-Acl", "public-read")), 403, "AccessDenied")

	e.putObject("b", "k", []byte("alice"), storage.ObjectMeta{Tags: map[string]string{"owner": "alice"}})
	authorized := make(chan struct{})
	hb.after("GetObjectInfo", "b", "k", func() { close(authorized) })
	pw, out := stalledRequest(t, e, alice, "PUT", "/b/k?acl", objHdr("X-Amz-Acl", "public-read"), []byte(" "))
	waitFor(t, authorized, "the authorization lookup")
	e.putObject("b", "k", []byte("BOB SECRET"), storage.ObjectMeta{Tags: map[string]string{"owner": "bob"}})
	if _, err := pw.Write([]byte(" ")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	objExpect(t, receive(t, out), 403, "AccessDenied")
	if info := objGetInfo(t, e, "b", "k", ""); info.ACL != "" {
		t.Fatalf("victim's object got ACL %q", info.ACL)
	}
	objExpect(t, e.anon("GET", "/b/k", nil, nil), 403, "AccessDenied")

	// Without a race the ACL is applied.
	e.putObject("b", "k", []byte("alice"), storage.ObjectMeta{Tags: map[string]string{"owner": "alice"}})
	objExpect(t, e.signedAs(alice, "PUT", "/b/k?acl", nil, objHdr("X-Amz-Acl", "public-read")), 200, "")
	if info := objGetInfo(t, e, "b", "k", ""); info.ACL != "public-read" {
		t.Fatalf("ACL %q", info.ACL)
	}
}

// TestInflightWriteRevalidated: a write whose body arrives after the
// caller's access was revoked does not commit
// (gap-authz-toctou-mutations-and-iam-races-3).
func TestInflightWriteRevalidated(t *testing.T) {
	ctx := context.Background()
	disabled := true
	for _, tc := range []struct {
		name   string
		revoke func(e *testEnv, user string, c auth.Credentials)
	}{
		{"user disabled", func(e *testEnv, user string, _ auth.Credentials) {
			if _, err := e.iam.UpdateUser(ctx, user, iam.UserUpdate{Disabled: &disabled}); err != nil {
				t.Fatal(err)
			}
		}},
		{"key deleted", func(e *testEnv, _ string, c auth.Credentials) {
			if err := e.iam.DeleteAccessKey(ctx, c.AccessKey); err != nil {
				t.Fatal(err)
			}
		}},
		{"bucket policy deny", func(e *testEnv, user string, _ auth.Credentials) {
			// Decisions are repeated for writes in flight for a while.
			old := writeReauthorizeAfter.Swap(0)
			t.Cleanup(func() { writeReauthorizeAfter.Store(old) })
			e.putBucketConfig("b", storage.ConfigPolicy, `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, hb := newHookEnv(t)
			e.createBucket("b")
			mallory := e.createUser("mallory", iam.PolicyReadWrite)
			started := make(chan struct{})
			hb.after("NewObjectWriter", "b", "r.db", func() { close(started) })
			pw, out := stalledRequest(t, e, mallory, "PUT", "/b/r.db", nil, []byte("PWND"))
			if _, err := pw.Write([]byte("PW")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, started, "the upload to start")
			tc.revoke(e, "mallory", mallory)
			e.putObject("b", "r.db", []byte("GOOD"), storage.ObjectMeta{})
			if _, err := pw.Write([]byte("ND")); err != nil {
				t.Fatal(err)
			}
			pw.Close()
			objExpect(t, receive(t, out), 403, "AccessDenied")
			if got := e.objectData("b", "r.db"); got != "GOOD" {
				t.Fatalf("object after the revoked upload: %q", got)
			}
		})
	}
}

// TestCompleteMultipartUploadBlockPublicAcls: an upload created with a
// public ACL cannot be completed once BlockPublicAcls is on (iam-3,
// s3-objects-2); with ACLs disabled the ACL is dropped instead.
func TestCompleteMultipartUploadBlockPublicAcls(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("b")
	start := func(key string) (string, string) {
		up, err := e.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("b"), Key: aws.String(key), ACL: types.ObjectCannedACLPublicRead})
		if err != nil {
			t.Fatal(err)
		}
		part, err := e.client.UploadPart(ctx, &awss3.UploadPartInput{Bucket: aws.String("b"), Key: aws.String(key), UploadId: up.UploadId,
			PartNumber: aws.Int32(1), Body: strings.NewReader("data")})
		if err != nil {
			t.Fatal(err)
		}
		return aws.ToString(up.UploadId), aws.ToString(part.ETag)
	}
	complete := func(key, id, etag string) error {
		_, err := e.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{Bucket: aws.String("b"), Key: aws.String(key), UploadId: aws.String(id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String(etag)}}}})
		return err
	}
	id, etag := start("k")
	id2, etag2 := start("k2")
	e.putBucketConfig("b", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`)
	var ae smithy.APIError
	if err := complete("k", id, etag); !errors.As(err, &ae) || ae.ErrorCode() != "AccessDenied" {
		t.Fatalf("complete after BlockPublicAcls: %v", err)
	}
	if _, err := e.backend.GetObjectInfo(ctx, "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object created: %v", err)
	}
	e.putBucketConfig("b", storage.ConfigOwnership, `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)
	if err := complete("k2", id2, etag2); err != nil {
		t.Fatalf("complete with ACLs disabled: %v", err)
	}
	if info := objGetInfo(t, e, "b", "k2", ""); info.ACL != "" {
		t.Fatalf("ACL %q kept with ACLs disabled", info.ACL)
	}
}

// TestObjectMetadataLimits: stored metadata is bounded in size and holds
// only valid header names and values (s3-objects-1, s3-objects-3).
func TestObjectMetadataLimits(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("b")
	big := strings.Repeat("a", maxObjectMetaSize+1)
	objExpect(t, e.signed("PUT", "/b/k", []byte("x"), objHdr("Content-Type", big)), 400, "MetadataTooLarge")
	objExpect(t, e.signed("PUT", "/b/k", []byte("x"), objHdr("Cache-Control", big[:5000], "Content-Disposition", big[:4000])), 400, "MetadataTooLarge")
	objExpect(t, e.signed("POST", "/b/k?uploads", nil, objHdr("Content-Language", big)), 400, "MetadataTooLarge")
	objExpect(t, e.signed("PUT", "/b/k", []byte("x"), objHdr("Cache-Control", big[:4000], "Content-Disposition", big[:3000])), 200, "")

	// CopyObject re-checks metadata copied from an older, oversized object.
	e.putObject("b", "legacy", []byte("x"), storage.ObjectMeta{ContentType: big})
	objExpect(t, e.signed("PUT", "/b/copy", nil, objHdr("X-Amz-Copy-Source", "/b/legacy")), 400, "MetadataTooLarge")
	objExpect(t, e.signed("PUT", "/b/copy", nil, objHdr("X-Amz-Copy-Source", "/b/legacy", "X-Amz-Metadata-Directive", "REPLACE")), 200, "")

	// PostObject fields are not HTTP headers: they are checked as such.
	e.createBucket("drop")
	e.putBucketConfig("drop", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::drop/*"}]}`)
	for _, f := range []objPostField{
		{"content-disposition", "inline\r\n\r\n<script>alert(1)</script>"},
		{"content-type", "text/plain\x00"},
		{"x-amz-meta-bad name:<b>", "v"},
		{"x-amz-meta-ok", "line\nbreak"},
	} {
		body, ct := objPostBody(t, []objPostField{{"key", "k"}, f}, "f.txt", []byte("hi"))
		objExpect(t, objPost(e, "drop", body, ct), 400, "InvalidArgument")
	}
	body, ct := objPostBody(t, []objPostField{{"key", "k"}, {"x-amz-meta-ok", "fine\tvalue"}}, "f.txt", []byte("hi"))
	objExpect(t, objPost(e, "drop", body, ct), 204, "")
}

// TestDeleteObjectsBounds: oversized keys are refused before anything is
// echoed, an integrity header is required, and a large result is streamed
// (s3-router-1, gap-csrf-1).
func TestDeleteObjectsBounds(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	for _, key := range []string{strings.Repeat("k", maxKeyLength+1), strings.Repeat(`&quot;`, maxKeyLength+1), strings.Repeat(`&quot;`, 6400)} {
		body := `<Delete><Object><Key>` + key + `</Key></Object></Delete>`
		resp := e.anon("POST", "/bkt?delete", []byte(body), md5Header(body))
		if resp.StatusCode != 400 || len(resp.Data) > 2048 {
			t.Errorf("key of %d bytes: %d, response of %d bytes (%s)", len(key), resp.StatusCode, len(resp.Data), resp.code())
		}
	}
	body0 := `<Delete><Object><Key>` + strings.Repeat("k", maxKeyLength+1) + `</Key></Object></Delete>`
	objExpect(t, e.anon("POST", "/bkt?delete", []byte(body0), md5Header(body0)), 400, "KeyTooLongError")
	body := `<Delete><Object><Key>k</Key><VersionId>` + strings.Repeat("v", maxVersionIDLength+1) + `</VersionId></Object></Delete>`
	objExpect(t, e.anon("POST", "/bkt?delete", []byte(body), md5Header(body)), 400, "InvalidArgument")

	// No Content-MD5 or checksum: refused (a cross-site form cannot set one).
	body = `<Delete><Object><Key>k</Key></Object></Delete>`
	objExpect(t, e.signed("POST", "/bkt?delete", []byte(body), nil), 400, "InvalidRequest")
	objExpect(t, e.anon("POST", "/bkt?delete", []byte(body), objHdr("Content-Type", "text/plain")), 400, "InvalidRequest")

	// A result larger than the buffer is streamed and complete.
	var b strings.Builder
	b.WriteString("<Delete>")
	for i := range maxDeleteObjects {
		fmt.Fprintf(&b, "<Object><Key>%04d-%s</Key></Object>", i, strings.Repeat("x", 1000))
	}
	b.WriteString("</Delete>")
	resp := e.signed("POST", "/bkt?delete", []byte(b.String()), md5Header(b.String()))
	objExpect(t, resp, 200, "")
	if resp.ContentLength != -1 {
		t.Errorf("large result sent with Content-Length %d, want streamed", resp.ContentLength)
	}
	var res deleteResult
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != maxDeleteObjects || len(res.Errors) != 0 || !strings.HasPrefix(res.Deleted[999].Key, "0999-") {
		t.Fatalf("%d deleted, %d errors", len(res.Deleted), len(res.Errors))
	}
}

// TestPostObjectRedirectAnonymous: an anonymous, unsigned form is only
// redirected to the server's own origin (gap-csrf-simple-requests-anon-buckets-4).
func TestPostObjectRedirectAnonymous(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("drop")
	e.putBucketConfig("drop", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::drop/*"}]}`)
	noFollow := &http.Client{Transport: e.httpc.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(redirect string) *http.Response {
		body, ct := objPostBody(t, []objPostField{{"key", "k"}, {"success_action_redirect", redirect}}, "f.txt", []byte("hi"))
		resp, err := noFollow.Do(e.newRequest("POST", "/drop", body, objHdr("Content-Type", ct)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post("https://evil.example/landing"); resp.StatusCode != http.StatusNoContent || strings.Contains(resp.Header.Get("Location"), "evil") {
		t.Errorf("cross-origin redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	own := e.srv.URL + "/done"
	resp := post(own)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), own+"?") {
		t.Errorf("same-origin redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if u, err := url.Parse(resp.Header.Get("Location")); err != nil || u.Query().Get("key") != "k" {
		t.Errorf("redirect location %q", resp.Header.Get("Location"))
	}
}

// TestCORSConfigurationCharacters: header names and origins of a CORS
// configuration must be valid in the headers they are sent in
// (s3-router-4).
func TestCORSConfigurationCharacters(t *testing.T) {
	rule := func(inner string) []byte {
		return []byte(`<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod>` + inner + `</CORSRule></CORSConfiguration>`)
	}
	for _, bad := range []string{
		`<AllowedOrigin>*</AllowedOrigin><ExposeHeader>X-A&#13;&#10;Set-Cookie: pwn=1</ExposeHeader>`,
		`<AllowedOrigin>*</AllowedOrigin><AllowedHeader>x-a&#10;b</AllowedHeader>`,
		`<AllowedOrigin>*</AllowedOrigin><AllowedHeader>x a</AllowedHeader>`,
		`<AllowedOrigin>https://a.example&#13;&#10;X: y</AllowedOrigin>`,
		`<AllowedOrigin>https://a b</AllowedOrigin>`,
		`<ID>a&#10;b</ID><AllowedOrigin>*</AllowedOrigin>`,
	} {
		if _, err := parseCORSConfiguration(rule(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, good := range []string{
		`<ID>my rule</ID><AllowedOrigin>https://*.example.com</AllowedOrigin><AllowedHeader>*</AllowedHeader><AllowedHeader>x-amz-*</AllowedHeader><ExposeHeader>ETag</ExposeHeader>`,
	} {
		if _, err := parseCORSConfiguration(rule(good)); err != nil {
			t.Errorf("refused %s: %v", good, err)
		}
	}
}

// bigLifecycle returns a valid lifecycle configuration of about n bytes.
func bigLifecycle(n int) []byte {
	var b bytes.Buffer
	b.WriteString("<LifecycleConfiguration>")
	for i := 0; b.Len() < n && i < 1000; i++ {
		fmt.Fprintf(&b, "<Rule><ID>r%d</ID><Filter><Prefix>%04d/%s</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>",
			i, i, strings.Repeat("p", 1000))
	}
	b.WriteString("</LifecycleConfiguration>")
	return b.Bytes()
}

// TestBucketLifecycleCache: the parsed lifecycle configuration is reused
// without copying the stored document per request, follows changes and is
// bounded in memory (dos-lifecycle-1).
func TestBucketLifecycleCache(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("b")
	doc := bigLifecycle(1 << 20)
	e.putBucketConfig("b", storage.ConfigLifecycle, string(doc))
	first := e.server.bucketLifecycle(ctx, "b")
	if first == nil || len(first.Rules) < 900 {
		t.Fatalf("configuration not parsed")
	}
	if again := e.server.bucketLifecycle(ctx, "b"); again != first {
		t.Fatalf("configuration parsed again")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 20 {
		e.server.bucketLifecycle(ctx, "b")
	}
	runtime.ReadMemStats(&after)
	if d := after.TotalAlloc - before.TotalAlloc; d > 256<<10 {
		t.Errorf("20 lookups allocated %d KiB", d>>10)
	}
	e.putBucketConfig("b", storage.ConfigLifecycle, `<LifecycleConfiguration><Rule><ID>x</ID><Filter/><Status>Enabled</Status><Expiration><Days>3</Days></Expiration></Rule></LifecycleConfiguration>`)
	if cfg := e.server.bucketLifecycle(ctx, "b"); cfg == nil || len(cfg.Rules) != 1 {
		t.Fatalf("changed configuration not seen")
	}
	e.putBucketConfig("b", storage.ConfigLifecycle, "not xml")
	if cfg := e.server.bucketLifecycle(ctx, "b"); cfg != nil {
		t.Fatalf("invalid configuration parsed")
	}

	// The cache is bounded by the estimated memory of its entries.
	c := newLifecycleCache(3*lifecycleCost(1000), 100)
	for i := range 10 {
		c.put(lifecycleKey{nil, fmt.Sprint(i)}, uint64(i), 1000, nil)
	}
	if n, size := c.size(); n != 3 || size > 3*lifecycleCost(1000) {
		t.Errorf("cache holds %d entries, %d bytes", n, size)
	}
	if _, ok := c.get(lifecycleKey{nil, "9"}, 9, 1000); !ok {
		t.Error("most recent entry evicted")
	}
	if _, ok := c.get(lifecycleKey{nil, "9"}, 8, 1000); ok {
		t.Error("entry returned for another document")
	}
	c.put(lifecycleKey{nil, "huge"}, 1, 1<<20, nil)
	if _, ok := c.get(lifecycleKey{nil, "huge"}, 1, 1<<20); ok {
		t.Error("entry over the whole budget cached")
	}
}

// TestPDFServedAsAttachment: PDFs, exempt from the sandbox CSP, are never
// rendered inline (webui-embed-3).
func TestPDFServedAsAttachment(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "doc.pdf", []byte("%PDF-1.4"), storage.ObjectMeta{ContentType: "application/pdf", ContentDisposition: `inline; filename="a.pdf"`})
	e.putObject("b", "plain.pdf", []byte("%PDF-1.4"), storage.ObjectMeta{ContentType: "application/pdf"})
	for _, tc := range []struct{ path, want string }{
		{"/b/doc.pdf", `attachment; filename=a.pdf`},
		{"/b/doc.pdf?response-content-disposition=inline", "attachment"},
		{"/b/plain.pdf", "attachment"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			resp := e.signed(method, tc.path, nil, nil)
			objExpect(t, resp, 200, "")
			if got := resp.Header.Get("Content-Disposition"); got != tc.want {
				t.Errorf("%s %s: Content-Disposition %q, want %q", method, tc.path, got, tc.want)
			}
		}
	}
	e.putObject("b", "page.html", []byte("<p>"), storage.ObjectMeta{ContentType: "text/html", ContentDisposition: "inline"})
	if resp := e.signed("GET", "/b/page.html", nil, nil); resp.Header.Get("Content-Disposition") != "inline" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("html: %v", resp.Header)
	}
}

// TestLongUploadJudgedAtRequestStart: the commit-time re-authorization of a
// long upload evaluates the clock-derived condition keys (s3:signatureAge,
// aws:CurrentTime) as of the request's start, so an upload that was
// allowed when it started is not refused because it took long, while a
// revocation during the upload still applies.
func TestLongUploadJudgedAtRequestStart(t *testing.T) {
	var offset atomic.Int64 // added to the server's clock
	var hb *hookBackend
	e := newTestEnv(t, withOptions(func(o *Options) {
		hb = &hookBackend{Backend: o.Backend.(*memory.Backend)}
		o.Backend = hb
		o.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	}))
	requireAuthz(t, e)
	e.createBucket("b")
	deadline := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	e.putPolicy("timed", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*","Condition":{`+
		`"NumericLessThan":{"s3:signatureAge":"600000"},"DateLessThan":{"aws:CurrentTime":"`+deadline+`"}}}]}`)
	alice := e.createUser("alice", "timed")

	upload := func(key string, during func()) *testResponse {
		started := make(chan struct{})
		hb.after("NewObjectWriter", "b", key, func() { close(started) })
		pw, out := stalledRequest(t, e, alice, "PUT", "/b/"+key, nil, []byte("DATA"))
		if _, err := pw.Write([]byte("DA")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, started, "the upload to start")
		offset.Store(int64(20 * time.Minute)) // the body takes 20 minutes
		if during != nil {
			during()
		}
		if _, err := pw.Write([]byte("TA")); err != nil {
			t.Fatal(err)
		}
		pw.Close()
		resp := receive(t, out)
		offset.Store(0)
		return resp
	}
	objExpect(t, upload("long", nil), 200, "")
	if got := e.objectData("b", "long"); got != "DATA" {
		t.Fatalf("object %q", got)
	}
	// A new Deny is still enforced at commit.
	resp := upload("denied", func() {
		e.putBucketConfig("b", storage.ConfigPolicy, `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	})
	objExpect(t, resp, 403, "AccessDenied")
	if _, err := e.backend.GetObjectInfo(context.Background(), "b", "denied", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("denied upload stored: %v", err)
	}
}

// TestStoredInvalidCORSDisablesCORS: a stored bucket CORS configuration
// that no longer validates disables CORS for the bucket (no CORS headers,
// preflights refused) without failing its requests or falling back to the
// global rules.
func TestStoredInvalidCORSDisablesCORS(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "k", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("b", storage.ConfigCORS, `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin>`+
		`<ExposeHeader>X-A&#13;&#10;Set-Cookie: pwn=1</ExposeHeader></CORSRule></CORSConfiguration>`)
	origin := objHdr("Origin", "https://app.example")
	for range 2 {
		resp := e.signed("GET", "/b/k", nil, origin)
		objExpect(t, resp, 200, "")
		if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "" {
			t.Fatalf("CORS header %q from an invalid configuration", v)
		}
	}
	pre := e.anon("OPTIONS", "/b/k", nil, objHdr("Origin", "https://app.example", "Access-Control-Request-Method", "GET"))
	if pre.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight: %d %s", pre.StatusCode, pre.Data)
	}
	objExpect(t, e.signed("GET", "/b?cors", nil, nil), 200, "")
	if !strings.Contains(e.logs.String(), "stored bucket CORS configuration is invalid") {
		t.Error("invalid configuration not logged")
	}
	// Replacing it with a valid configuration restores CORS.
	e.putBucketConfig("b", storage.ConfigCORS, `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>`)
	if v := e.signed("GET", "/b/k", nil, origin).Header.Get("Access-Control-Allow-Origin"); v == "" {
		t.Fatal("valid configuration not applied")
	}
}

// TestBucketConfigWriteRevalidated: a bucket configuration write whose body
// arrives after the caller was disabled does not change the bucket (here:
// a policy making it public).
func TestBucketConfigWriteRevalidated(t *testing.T) {
	e, hb := newHookEnv(t)
	ctx := context.Background()
	e.createBucket("b")
	e.putPolicy("pol", `{"Statement":[{"Effect":"Allow","Action":"s3:PutBucketPolicy","Resource":"arn:aws:s3:::b"}]}`)
	mallory := e.createUser("mallory", "pol")
	policy := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	authorized := make(chan struct{})
	hb.after("GetBucketConfig", "b", storage.ConfigPolicy, func() { close(authorized) })
	pw, out := stalledRequest(t, e, mallory, "PUT", "/b?policy", nil, policy)
	if _, err := pw.Write(policy[:10]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, authorized, "the authorization")
	disabled := true
	if _, err := e.iam.UpdateUser(ctx, "mallory", iam.UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(policy[10:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	objExpect(t, receive(t, out), 403, "AccessDenied")
	if _, err := e.backend.GetBucketConfig(ctx, "b", storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("policy stored by the revoked request: %v", err)
	}
	// Without the revocation the same request works.
	enabled := false
	if _, err := e.iam.UpdateUser(ctx, "mallory", iam.UserUpdate{Disabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	objExpect(t, e.signedAs(mallory, "PUT", "/b?policy", policy, nil), 204, "")
}

// TestPinnedDeleteOfMissingKeyAddsMarker: a delete decided on the lookup of
// a key with no object still creates the delete marker in a versioned
// bucket, but not over an object that appeared after the decision.
func TestPinnedDeleteOfMissingKeyAddsMarker(t *testing.T) {
	e, hb := newHookEnv(t)
	ctx := context.Background()
	e.createBucket("vb")
	if err := e.backend.SetBucketVersioning(ctx, "vb", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	e.putBucketConfig("vb", storage.ConfigPolicy, strings.ReplaceAll(protectedDenyPolicy, "arn:aws:s3:::b/*", "arn:aws:s3:::vb/*"))
	e.putPolicy("del", `{"Statement":[{"Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::vb/*"}]}`)
	alice := e.createUser("alice", "del")

	resp := e.signedAs(alice, "DELETE", "/vb/nokey", nil, nil)
	objExpect(t, resp, 204, "")
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("no delete marker reported: %v", resp.Header)
	}
	if info, err := e.backend.GetObjectInfo(ctx, "vb", "nokey", ""); err != nil || !info.DeleteMarker {
		t.Fatalf("no delete marker stored: %+v %v", info, err)
	}

	// A protected object created after the lookup found nothing.
	hb.after("GetObjectInfo", "vb", "late", func() { e.putObject("vb", "late", []byte("victim"), protectedMeta) })
	objExpect(t, e.signedAs(alice, "DELETE", "/vb/late", nil, nil), 403, "AccessDenied")
	if info, err := e.backend.GetObjectInfo(ctx, "vb", "late", ""); err != nil || info.DeleteMarker {
		t.Fatalf("protected object hidden: %+v %v", info, err)
	}
}

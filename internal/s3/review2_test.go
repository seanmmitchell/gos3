package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestObjectSandboxPDFExemptionStrict: only a Content-Type that is exactly
// application/pdf escapes the sandbox CSP. Browsers take the last MIME type
// of a comma-separated list, so "application/pdf; x=y, text/html" renders
// as HTML and must be sandboxed; an exempt PDF is served with the canonical
// type.
func TestObjectSandboxPDFExemptionStrict(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "x.html", []byte("<script>alert(1)</script>"), storage.ObjectMeta{ContentType: "application/pdf; x=y, text/html"})
	e.putObject("b", "doc.pdf", []byte("%PDF-1.4"), storage.ObjectMeta{ContentType: "application/pdf; name=a"})
	for _, tc := range []struct {
		method, path, csp, ctype string
	}{
		{"GET", "/b/x.html", "sandbox", ""},
		{"HEAD", "/b/x.html", "sandbox", ""},
		{"GET", "/b/doc.pdf?response-content-type=application%2Fpdf%3Bx%3Dy%2Ctext%2Fhtml", "sandbox", ""},
		{"GET", "/b/doc.pdf?response-content-type=application%2Fpdf%2Ctext%2Fhtml", "sandbox", ""},
		{"GET", "/b/doc.pdf?response-content-type=application%2Fpdf%3B%20x%3D%22a%2Cb%22", "sandbox", ""},
		{"GET", "/b/doc.pdf", "", "application/pdf"},
		{"HEAD", "/b/doc.pdf", "", "application/pdf"},
	} {
		resp := e.signed(tc.method, tc.path, nil, nil)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, resp.Data)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != tc.csp {
			t.Errorf("%s %s: CSP %q, want %q", tc.method, tc.path, got, tc.csp)
		}
		if tc.ctype != "" {
			if got := resp.Header.Get("Content-Type"); got != tc.ctype {
				t.Errorf("%s %s: Content-Type %q, want %q", tc.method, tc.path, got, tc.ctype)
			}
		}
	}
}

// presignWith returns a root-presigned URL for method + path whose query
// carries the given extra (signed) parameters, the way the AWS SDK for
// JavaScript v3 presigner moves x-amz-* headers into the query string.
func presignWith(t *testing.T, e *testEnv, c auth.Credentials, method, path string, params url.Values) string {
	t.Helper()
	u := e.url(path)
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + params.Encode()
	}
	signed, err := auth.PresignURL(method, u, c, "us-east-1", time.Minute, time.Now())
	if err != nil {
		t.Fatalf("PresignURL: %v", err)
	}
	return signed
}

// TestPresignedQueryHoistedHeaders: x-amz-* parameters in a presigned URL's
// query act as the request headers (copy source, metadata, tagging, ACL,
// storage class, object lock), so a presigned CopyObject copies instead of
// truncating the destination.
func TestPresignedQueryHoistedHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "src", []byte("precious"), storage.ObjectMeta{ContentType: "text/plain"})
	e.putObject("b", "dst", []byte("old"), storage.ObjectMeta{})
	ctx := context.Background()

	// Presigned CopyObject.
	u := presignWith(t, e, testRoot, "PUT", "/b/dst", url.Values{"x-amz-copy-source": {"b/src"}, "x-id": {"CopyObject"}})
	resp := e.do(e.newRequestURL("PUT", u, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Data), "CopyObjectResult") {
		t.Fatalf("presigned copy: %d %s", resp.StatusCode, resp.Data)
	}
	if got := e.objectData("b", "dst"); got != "precious" {
		t.Fatalf("copy destination = %q", got)
	}

	// Presigned PutObject with metadata, tags, ACL and storage class.
	u = presignWith(t, e, testRoot, "PUT", "/b/meta", url.Values{
		"x-amz-meta-foo": {"bar"}, "x-amz-tagging": {"t=1"}, "x-amz-acl": {"public-read"},
		"x-amz-storage-class": {"STANDARD_IA"}, "x-amz-checksum-crc32": {"AAAAAA=="},
		"x-amz-sdk-checksum-algorithm": {"CRC32"},
	})
	if resp := e.do(e.newRequestURL("PUT", u, []byte("x"))); resp.StatusCode != 200 {
		t.Fatalf("presigned put: %d %s", resp.StatusCode, resp.Data)
	}
	info, err := e.backend.GetObjectInfo(ctx, "b", "meta", "")
	if err != nil || info.UserMeta["foo"] != "bar" || info.Tags["t"] != "1" || info.ACL != "public-read" || info.StorageClass != "STANDARD_IA" {
		t.Fatalf("presigned put stored %+v %v", info, err)
	}

	// Metadata from the query.
	u = presignWith(t, e, testRoot, "PUT", "/b/hdr", url.Values{"x-amz-meta-foo": {"query"}})
	req := e.newRequestURL("PUT", u, []byte("x"))
	if resp := e.do(req); resp.StatusCode != 200 {
		t.Fatalf("presigned put: %d %s", resp.StatusCode, resp.Data)
	}
	if info, _ := e.backend.GetObjectInfo(ctx, "b", "hdr", ""); info.UserMeta["foo"] != "query" {
		t.Fatalf("hoisted metadata: %+v", info.UserMeta)
	}

	// Object lock parameters.
	if resp := e.signed("PUT", "/lockb", nil, objHdr("X-Amz-Bucket-Object-Lock-Enabled", "true")); resp.StatusCode != 200 {
		t.Fatalf("create lock bucket: %d %s", resp.StatusCode, resp.Data)
	}
	until := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	u = presignWith(t, e, testRoot, "PUT", "/lockb/o", url.Values{
		"x-amz-object-lock-mode": {"GOVERNANCE"}, "x-amz-object-lock-retain-until-date": {until},
		"x-amz-object-lock-legal-hold": {"ON"},
	})
	if resp := e.do(e.newRequestURL("PUT", u, []byte("x"))); resp.StatusCode != 200 {
		t.Fatalf("presigned lock put: %d %s", resp.StatusCode, resp.Data)
	}
	info, err = e.backend.GetObjectInfo(ctx, "lockb", "o", "")
	if err != nil || info.RetentionMode != "GOVERNANCE" || !info.LegalHold {
		t.Fatalf("presigned lock put stored %+v %v", info, err)
	}

	// Tampering with a hoisted parameter breaks the signature; control
	// characters are refused.
	tampered := strings.Replace(presignWith(t, e, testRoot, "PUT", "/b/dst", url.Values{"x-amz-copy-source": {"b/src"}}), "b%2Fsrc", "b%2Fmeta", 1)
	if resp := e.do(e.newRequestURL("PUT", tampered, nil)); resp.StatusCode != 403 {
		t.Fatalf("tampered copy source: %d %s", resp.StatusCode, resp.Data)
	}
	u = presignWith(t, e, testRoot, "PUT", "/b/bad", url.Values{"x-amz-meta-foo": {"a\r\nb"}})
	if resp := e.do(e.newRequestURL("PUT", u, []byte("x"))); resp.StatusCode != 400 {
		t.Fatalf("control character: %d %s", resp.StatusCode, resp.Data)
	}
	// Unsigned requests do not get query parameters as headers.
	e.putBucketConfig("b", storage.ConfigACL, "public-read-write")
	if resp := e.anon("PUT", "/b/anon?x-amz-copy-source=b%2Fsrc", []byte("mine"), nil); resp.StatusCode != 200 {
		t.Fatalf("anonymous put: %d %s", resp.StatusCode, resp.Data)
	}
	if got := e.objectData("b", "anon"); got != "mine" {
		t.Fatalf("anonymous put with query copy source stored %q", got)
	}
}

// newRequestURL builds a request for an absolute URL.
func (e *testEnv) newRequestURL(method, u string, body []byte) *http.Request {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// objectData reads an object's content from the backend.
func (e *testEnv) objectData(bucket, key string) string {
	e.t.Helper()
	_, rd, err := e.backend.OpenObject(context.Background(), bucket, key, "")
	if err != nil {
		e.t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer rd.Close()
	b, err := io.ReadAll(rd)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// TestRequestObjectTagsOnlyFromStoredSource: the x-amz-tagging header only
// provides s3:RequestObjectTag/* keys where it is what gets stored
// (PutObject, CreateMultipartUpload, CopyObject with REPLACE); it cannot
// satisfy a tag condition for PutObjectTagging (body tags), PostObject
// (form tags) or CopyObject with the COPY directive.
func TestRequestObjectTagsOnlyFromStoredSource(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("b1")
	e.putObject("b1", "x", []byte("x"), storage.ObjectMeta{Tags: map[string]string{"Owner": "evil"}})
	e.putPolicy("tagpol", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::b1/*",
		 "Condition":{"StringEquals":{"s3:RequestObjectTag/Project":"X"}}},
		{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectTagging"],"Resource":"arn:aws:s3:::b1/*"}]}`)
	u := e.createUser("tagger", "tagpol")
	evil := []byte(`<Tagging><TagSet><Tag><Key>Owner</Key><Value>evil</Value></Tag></TagSet></Tagging>`)
	hdr := func(kv ...string) http.Header {
		return objHdr(append([]string{"X-Amz-Tagging", "Project=X"}, kv...)...)
	}

	if resp := e.signedAs(u, "PUT", "/b1/x?tagging", evil, hdr()); resp.StatusCode != 403 {
		t.Errorf("PutObjectTagging with conflicting header: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(u, "PUT", "/b1/c", nil, hdr("X-Amz-Copy-Source", "/b1/x")); resp.StatusCode != 403 {
		t.Errorf("CopyObject COPY with header: %d %s", resp.StatusCode, resp.Data)
	}
	exp := time.Now().Add(time.Hour)
	conds := []any{map[string]string{"bucket": "b1"}, []any{"starts-with", "$key", ""}, []any{"starts-with", "$tagging", ""}}
	body, ct := objPostBody(t, objSignedPostFields(t, u, exp, conds, objPostField{"key", "p"},
		objPostField{"tagging", string(evil)}), "f.txt", []byte("x"))
	if resp := e.anon("POST", "/b1", body, hdr("Content-Type", ct)); resp.StatusCode != 403 {
		t.Errorf("PostObject with conflicting header: %d %s", resp.StatusCode, resp.Data)
	}
	// Where the header is the stored tag set, it counts.
	if resp := e.signedAs(u, "PUT", "/b1/ok", []byte("x"), hdr()); resp.StatusCode != 200 {
		t.Errorf("PutObject with header: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(u, "PUT", "/b1/c", nil, hdr("X-Amz-Copy-Source", "/b1/x", "X-Amz-Tagging-Directive", "REPLACE")); resp.StatusCode != 200 {
		t.Errorf("CopyObject REPLACE with header: %d %s", resp.StatusCode, resp.Data)
	}
	if info, _ := e.backend.GetObjectInfo(context.Background(), "b1", "c", ""); info.Tags["Project"] != "X" || len(info.Tags) != 1 {
		t.Errorf("copy tags: %v", info.Tags)
	}
}

// TestCopyObjectCopiedTagsNeedPermissions: copying a tagged source with the
// COPY tagging directive needs s3:GetObjectTagging on the source and
// s3:PutObjectTagging (with the copied tags as request tags) on the
// destination.
func TestCopyObjectCopiedTagsNeedPermissions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("tc")
	e.putObject("tc", "src", []byte("x"), storage.ObjectMeta{Tags: map[string]string{"approved": "true"}})
	e.putObject("tc", "plain", []byte("x"), storage.ObjectMeta{})
	e.putPolicy("getput", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::*"}]}`)
	carol := e.createUser("carol", "getput")
	e.putPolicy("getput-put-tags", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::*"}]}`)
	dave := e.createUser("dave", "getput-put-tags")
	e.putPolicy("getput-tags", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:GetObjectTagging","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::*"}]}`)
	erin := e.createUser("erin", "getput-tags")

	for _, c := range []struct {
		name  string
		creds auth.Credentials
		want  int
	}{{"carol", carol, 403}, {"dave", dave, 403}, {"erin", erin, 200}} {
		if resp := e.signedAs(c.creds, "PUT", "/tc/dst-"+c.name, nil, objHdr("X-Amz-Copy-Source", "/tc/src")); resp.StatusCode != c.want {
			t.Errorf("%s: copy of tagged source: %d %s", c.name, resp.StatusCode, resp.Data)
		}
	}
	if info, _ := e.backend.GetObjectInfo(context.Background(), "tc", "dst-erin", ""); info.Tags["approved"] != "true" {
		t.Errorf("erin's copy lost the tags: %v", info.Tags)
	}
	// Untagged sources, and REPLACE without tags, need no tag permissions.
	if resp := e.signedAs(carol, "PUT", "/tc/d1", nil, objHdr("X-Amz-Copy-Source", "/tc/plain")); resp.StatusCode != 200 {
		t.Errorf("carol: copy of untagged source: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(carol, "PUT", "/tc/d2", nil, objHdr("X-Amz-Copy-Source", "/tc/src", "X-Amz-Tagging-Directive", "REPLACE")); resp.StatusCode != 200 {
		t.Errorf("carol: copy with REPLACE and no tags: %d %s", resp.StatusCode, resp.Data)
	}
	if info, _ := e.backend.GetObjectInfo(context.Background(), "tc", "d2", ""); len(info.Tags) != 0 {
		t.Errorf("REPLACE copy kept tags: %v", info.Tags)
	}
}

// TestPostObjectConditionKeysFromFields: PostObject's condition keys come
// from the normalised form fields, including the object lock fields.
func TestPostObjectConditionKeysFromFields(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	e.createBucket("postacl")
	e.putBucketConfig("postacl", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectAcl"],"Resource":"arn:aws:s3:::postacl/*",
		 "Condition":{"StringEquals":{"s3:x-amz-acl":["public-read","public-read-write"]}}}]}`)
	exp := time.Now().Add(time.Hour)
	conds := []any{map[string]string{"bucket": "postacl"}, []any{"starts-with", "$key", ""}, []any{"starts-with", "$acl", ""}}
	for _, acl := range []string{"public-read", "public-read ", " public-read"} {
		body, ct := objPostBody(t, objSignedPostFields(t, bob, exp, conds, objPostField{"key", "p"}, objPostField{"acl", acl}), "f.txt", []byte("x"))
		if resp := objPost(e, "postacl", body, ct); resp.StatusCode != 403 {
			t.Errorf("POST acl %q: %d %s", acl, resp.StatusCode, resp.Data)
		}
	}
	if resp := e.anon("GET", "/postacl/p", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("object published: %d", resp.StatusCode)
	}

	if resp := e.signed("PUT", "/lockpost", nil, objHdr("X-Amz-Bucket-Object-Lock-Enabled", "true")); resp.StatusCode != 200 {
		t.Fatalf("create lock bucket: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("lockpost", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::lockpost/*",
		 "Condition":{"StringEquals":{"s3:object-lock-mode":"COMPLIANCE"}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectLegalHold"],"Resource":"arn:aws:s3:::lockpost/*",
		 "Condition":{"StringEquals":{"s3:object-lock-legal-hold":"ON"}}}]}`)
	until := time.Now().Add(3650 * 24 * time.Hour).UTC().Format(time.RFC3339)
	lconds := []any{map[string]string{"bucket": "lockpost"}, []any{"starts-with", "$key", ""},
		[]any{"starts-with", "$x-amz-object-lock-mode", ""}, []any{"starts-with", "$x-amz-object-lock-retain-until-date", ""},
		[]any{"starts-with", "$x-amz-object-lock-legal-hold", ""}}
	for _, tc := range [][]objPostField{
		{{"key", "l1"}, {"x-amz-object-lock-mode", "COMPLIANCE"}, {"x-amz-object-lock-retain-until-date", until}},
		{{"key", "l2"}, {"x-amz-object-lock-legal-hold", "ON"}},
	} {
		body, ct := objPostBody(t, objSignedPostFields(t, bob, exp, lconds, tc...), "f.txt", []byte("x"))
		// Request headers do not stand in for the form fields either.
		resp := e.anon("POST", "/lockpost", body, objHdr("Content-Type", ct, "X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Legal-Hold", "OFF"))
		if resp.StatusCode != 403 {
			t.Errorf("POST %v: %d %s", tc[1], resp.StatusCode, resp.Data)
		}
	}
	// Headers on PutObject are checked as before.
	if resp := e.signedAs(bob, "PUT", "/lockpost/h", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", until)); resp.StatusCode != 403 {
		t.Errorf("PutObject COMPLIANCE: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestObjectLockBodyConditionKeys: PutObjectRetention and
// PutObjectLegalHold are evaluated with the condition keys of their body,
// including s3:object-lock-remaining-retention-days (also set for
// PutObject's lock headers).
func TestObjectLockBodyConditionKeys(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	if resp := e.signed("PUT", "/lockb", nil, objHdr("X-Amz-Bucket-Object-Lock-Enabled", "true")); resp.StatusCode != 200 {
		t.Fatalf("create lock bucket: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("lockb", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::lockb/*",
		 "Condition":{"StringEquals":{"s3:object-lock-mode":"COMPLIANCE"}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::lockb/*",
		 "Condition":{"NumericGreaterThan":{"s3:object-lock-remaining-retention-days":"30"}}},
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObjectLegalHold","Resource":"arn:aws:s3:::lockb/*",
		 "Condition":{"StringEquals":{"s3:object-lock-legal-hold":"OFF"}}}]}`)
	if resp := e.signedAs(bob, "PUT", "/lockb/r", []byte("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, resp.Data)
	}
	retention := func(mode string, d time.Duration) []byte {
		return []byte(`<Retention><Mode>` + mode + `</Mode><RetainUntilDate>` + time.Now().Add(d).UTC().Format(time.RFC3339) + `</RetainUntilDate></Retention>`)
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		body []byte
		hdr  http.Header
		want int
	}{
		{"compliance", retention("COMPLIANCE", 10*day), nil, 403},
		{"compliance, governance header", retention("COMPLIANCE", 10*day), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE"), 403},
		{"governance 100 days", retention("GOVERNANCE", 100*day), nil, 403},
		{"governance 10 days", retention("GOVERNANCE", 10*day), nil, 200},
	} {
		if resp := e.signedAs(bob, "PUT", "/lockb/r?retention", tc.body, tc.hdr); resp.StatusCode != tc.want {
			t.Errorf("retention %s: %d %s", tc.name, resp.StatusCode, resp.Data)
		}
	}
	if info, _ := e.backend.GetObjectInfo(context.Background(), "lockb", "r", ""); info.RetentionMode != "GOVERNANCE" {
		t.Errorf("retention stored: %+v", info)
	}
	hold := func(s string) []byte { return []byte(`<LegalHold><Status>` + s + `</Status></LegalHold>`) }
	if resp := e.signedAs(bob, "PUT", "/lockb/r?legal-hold", hold("ON"), nil); resp.StatusCode != 200 {
		t.Errorf("legal hold ON: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/lockb/r?legal-hold", hold("OFF"), objHdr("X-Amz-Object-Lock-Legal-Hold", "ON")); resp.StatusCode != 403 {
		t.Errorf("legal hold OFF: %d %s", resp.StatusCode, resp.Data)
	}
	// PutObject headers get the remaining-days key too.
	until := time.Now().Add(100 * day).UTC().Format(time.RFC3339)
	if resp := e.signedAs(bob, "PUT", "/lockb/h", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", until)); resp.StatusCode != 403 {
		t.Errorf("PutObject 100-day retention: %d %s", resp.StatusCode, resp.Data)
	}
	// Unauthorized callers learn nothing from a missing bucket lock config.
	e.createBucket("nolock")
	if resp := e.anon("PUT", "/nolock/k?retention", retention("GOVERNANCE", day), nil); resp.StatusCode != 403 {
		t.Errorf("anonymous retention: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestPublicReadWriteACLCannotDeleteVersions: bucket WRITE (public-read-
// write) lets anonymous callers add delete markers but not permanently
// delete versions or abort uploads.
func TestPublicReadWriteACLCannotDeleteVersions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("prw")
	if resp := e.signed("PUT", "/prw?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil); resp.StatusCode != 200 {
		t.Fatalf("versioning: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("prw", storage.ConfigACL, "public-read-write")
	resp := e.signed("PUT", "/prw/doc", []byte("v1"), nil)
	vid := resp.Header.Get("X-Amz-Version-Id")
	if resp.StatusCode != 200 || vid == "" {
		t.Fatalf("put: %d %q", resp.StatusCode, vid)
	}
	if resp := e.anon("DELETE", "/prw/doc?versionId="+vid, nil, nil); resp.StatusCode != 403 {
		t.Fatalf("anonymous version delete: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.anon("DELETE", "/prw/doc", nil, nil); resp.StatusCode != 204 {
		t.Fatalf("anonymous delete (marker): %d %s", resp.StatusCode, resp.Data)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "prw", "doc", vid); err != nil {
		t.Fatalf("version gone: %v", err)
	}
	up, err := e.backend.CreateMultipartUpload(context.Background(), "prw", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := e.anon("DELETE", "/prw/mp?uploadId="+up.UploadID, nil, nil); resp.StatusCode != 403 {
		t.Fatalf("anonymous abort: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestRequireSSEPolicyMultipart: with the standard "require SSE" bucket
// policy, a multipart upload whose CreateMultipartUpload asked for SSE can
// upload and complete parts (their condition keys come from the upload);
// headers on the part requests do not count.
func TestRequireSSEPolicyMultipart(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	e.createBucket("encb")
	e.putBucketConfig("encb", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::encb/*","Condition":{"StringNotEquals":{"s3:x-amz-server-side-encryption":"AES256"}}},
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::encb/*","Condition":{"Null":{"s3:x-amz-server-side-encryption":"true"}}}]}`)
	if resp := e.signedAs(bob, "PUT", "/encb/single", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "AES256")); resp.StatusCode != 200 {
		t.Fatalf("single put with SSE: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "POST", "/encb/mp?uploads", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("create without SSE: %d %s", resp.StatusCode, resp.Data)
	}
	resp := e.signedAs(bob, "POST", "/encb/mp?uploads", nil, objHdr("X-Amz-Server-Side-Encryption", "AES256"))
	if resp.StatusCode != 200 {
		t.Fatalf("create with SSE: %d %s", resp.StatusCode, resp.Data)
	}
	var init struct{ UploadId string }
	if err := xml.Unmarshal(resp.Data, &init); err != nil || init.UploadId == "" {
		t.Fatalf("upload id: %v %s", err, resp.Data)
	}
	resp = e.signedAs(bob, "PUT", "/encb/mp?partNumber=1&uploadId="+init.UploadId, []byte("part"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("upload part: %d %s", resp.StatusCode, resp.Data)
	}
	complete := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + resp.Header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	if resp := e.signedAs(bob, "POST", "/encb/mp?uploadId="+init.UploadId, []byte(complete), nil); resp.StatusCode != 200 {
		t.Fatalf("complete: %d %s", resp.StatusCode, resp.Data)
	}
	// An upload created without SSE (by root, who bypasses the policy)
	// cannot be fed by bob with an SSE header on the part.
	up, err := e.backend.CreateMultipartUpload(context.Background(), "encb", "plain", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := e.signedAs(bob, "PUT", "/encb/plain?partNumber=1&uploadId="+up.UploadID, []byte("part"), objHdr("X-Amz-Server-Side-Encryption", "AES256")); resp.StatusCode != 403 {
		t.Fatalf("part of an unencrypted upload: %d %s", resp.StatusCode, resp.Data)
	}
}

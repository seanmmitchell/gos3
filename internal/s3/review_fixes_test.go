package s3

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestNewObjectACLAndTagsNeedPermissions: as in AWS, an ACL given with a
// new object needs s3:PutObjectAcl and tags need s3:PutObjectTagging; a
// principal limited to s3:PutObject (e.g. the writeonly policy) cannot
// publish objects or plant tags.
func TestNewObjectACLAndTagsNeedPermissions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("priv")
	e.putObject("priv", "src", []byte("source"), storage.ObjectMeta{Tags: map[string]string{"s": "1"}})
	w := e.createUser("writer", "writeonly")
	e.putPolicy("put-deny-acl", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject","s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::priv/*"},
		{"Effect":"Deny","Action":["s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::priv/*"}]}`)
	d := e.createUser("denied", "put-deny-acl")
	e.putPolicy("put-all", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject","s3:PutObjectAcl","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::priv/*"}]}`)
	ok := e.createUser("allowed", "put-all")

	for _, user := range []struct {
		name  string
		creds auth.Credentials
	}{{"writeonly", w}, {"explicit deny", d}} {
		for _, tc := range []struct {
			name, method, path string
			hdr                []string
		}{
			{"PutObject acl", "PUT", "/priv/a", []string{"X-Amz-Acl", "public-read"}},
			{"PutObject tagging", "PUT", "/priv/a", []string{"X-Amz-Tagging", "class=public"}},
			{"CreateMultipartUpload acl", "POST", "/priv/a?uploads", []string{"X-Amz-Acl", "public-read"}},
			{"CreateMultipartUpload tagging", "POST", "/priv/a?uploads", []string{"X-Amz-Tagging", "a=b"}},
			{"CopyObject acl", "PUT", "/priv/c", []string{"X-Amz-Copy-Source", "/priv/src", "X-Amz-Acl", "public-read"}},
			{"CopyObject replace tagging", "PUT", "/priv/c", []string{"X-Amz-Copy-Source", "/priv/src",
				"X-Amz-Tagging-Directive", "REPLACE", "X-Amz-Tagging", "a=b"}},
		} {
			var body []byte
			if tc.method == "PUT" && tc.hdr[0] != "X-Amz-Copy-Source" {
				body = []byte("secret")
			}
			resp := e.signedAs(user.creds, tc.method, tc.path, body, objHdr(tc.hdr...))
			if resp.StatusCode != 403 || resp.code() != "AccessDenied" {
				t.Errorf("%s: %s: %d %s, want 403 AccessDenied", user.name, tc.name, resp.StatusCode, resp.code())
			}
		}
	}
	if resp := e.anon("GET", "/priv/a", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("object published by a writer without s3:PutObjectAcl: %d", resp.StatusCode)
	}

	// Plain writes still work for writeonly; copies keep the source's tags
	// (COPY directive) without s3:PutObjectTagging.
	if resp := e.signedAs(w, "PUT", "/priv/a", []byte("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("writeonly plain put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(d, "PUT", "/priv/c", nil, objHdr("X-Amz-Copy-Source", "/priv/src")); resp.StatusCode != 200 {
		t.Fatalf("copy with source tags: %d %s", resp.StatusCode, resp.Data)
	}
	// With the permissions, ACL and tags are accepted.
	if resp := e.signedAs(ok, "PUT", "/priv/b", []byte("x"), objHdr("X-Amz-Acl", "public-read", "X-Amz-Tagging", "a=b")); resp.StatusCode != 200 {
		t.Fatalf("put with acl+tags and permissions: %d %s", resp.StatusCode, resp.Data)
	}
	info, err := e.backend.GetObjectInfo(context.Background(), "priv", "b", "")
	if err != nil || info.Tags["a"] != "b" || info.ACL != "public-read" {
		t.Fatalf("stored: %+v %v", info, err)
	}
}

// TestPostObjectACLAndTagsNeedPermissions covers the POST form fields.
func TestPostObjectACLAndTagsNeedPermissions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("priv")
	w := e.createUser("writer", "writeonly")
	exp := time.Now().Add(time.Hour)
	conds := []any{map[string]string{"bucket": "priv"}, []any{"starts-with", "$key", ""},
		[]any{"starts-with", "$acl", ""}, []any{"starts-with", "$tagging", ""}}
	tagging := `<Tagging><TagSet><Tag><Key>class</Key><Value>public</Value></Tag></TagSet></Tagging>`
	for _, tc := range []struct {
		name   string
		fields []objPostField
	}{
		{"acl", []objPostField{{"key", "p1"}, {"acl", "public-read"}}},
		{"tagging", []objPostField{{"key", "p2"}, {"tagging", tagging}}},
	} {
		body, ct := objPostBody(t, objSignedPostFields(t, w, exp, conds, tc.fields...), "f.txt", []byte("secret"))
		if resp := objPost(e, "priv", body, ct); resp.StatusCode != 403 || resp.code() != "AccessDenied" {
			t.Errorf("writeonly POST with %s: %d %s", tc.name, resp.StatusCode, resp.Data)
		}
	}
	body, ct := objPostBody(t, objSignedPostFields(t, w, exp, conds, objPostField{"key", "p3"}), "f.txt", []byte("ok"))
	if resp := objPost(e, "priv", body, ct); resp.StatusCode/100 != 2 {
		t.Fatalf("writeonly plain POST: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestRequestObjectTagConditions: the s3:RequestObjectTag/<key> and
// s3:RequestObjectTagKeys keys are evaluated for PutObjectTagging (tags in
// the body) and PostObject (tags in the form), not only for the
// x-amz-tagging header.
func TestRequestObjectTagConditions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("tb")
	e.putObject("tb", "k", []byte("x"), storage.ObjectMeta{})
	e.putPolicy("tag-rules", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::tb/*"},
		{"Effect":"Deny","Action":["s3:PutObject","s3:PutObjectTagging"],"Resource":"arn:aws:s3:::tb/*",
		 "Condition":{"StringEquals":{"s3:RequestObjectTag/class":"public"}}}]}`)
	u := e.createUser("tagger", "tag-rules")
	tagDoc := func(v string) []byte {
		return []byte(`<Tagging><TagSet><Tag><Key>class</Key><Value>` + v + `</Value></Tag></TagSet></Tagging>`)
	}
	if resp := e.signedAs(u, "PUT", "/tb/k?tagging", tagDoc("public"), nil); resp.StatusCode != 403 {
		t.Fatalf("PutObjectTagging class=public: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(u, "PUT", "/tb/k?tagging", tagDoc("private"), nil); resp.StatusCode != 200 {
		t.Fatalf("PutObjectTagging class=private: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(u, "PUT", "/tb/h", []byte("x"), objHdr("X-Amz-Tagging", "class=public")); resp.StatusCode != 403 {
		t.Fatalf("PutObject x-amz-tagging class=public: %d", resp.StatusCode)
	}
	exp := time.Now().Add(time.Hour)
	conds := []any{map[string]string{"bucket": "tb"}, []any{"starts-with", "$key", ""}, []any{"starts-with", "$tagging", ""}}
	body, ct := objPostBody(t, objSignedPostFields(t, u, exp, conds, objPostField{"key", "p"},
		objPostField{"tagging", string(tagDoc("public"))}), "f.txt", []byte("x"))
	if resp := objPost(e, "tb", body, ct); resp.StatusCode != 403 {
		t.Fatalf("PostObject tagging class=public: %d %s", resp.StatusCode, resp.Data)
	}

	// An Allow that requires a request tag is satisfied by the body.
	e.putPolicy("tag-allow", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:PutObjectTagging","Resource":"arn:aws:s3:::tb/*",
		 "Condition":{"StringEquals":{"s3:RequestObjectTag/class":"ok"}}}]}`)
	a := e.createUser("allowtag", "tag-allow")
	if resp := e.signedAs(a, "PUT", "/tb/k?tagging", tagDoc("ok"), nil); resp.StatusCode != 200 {
		t.Fatalf("conditional allow class=ok: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(a, "PUT", "/tb/k?tagging", tagDoc("no"), nil); resp.StatusCode != 403 {
		t.Fatalf("conditional allow class=no: %d %s", resp.StatusCode, resp.Data)
	}
	// Unauthorized callers still learn nothing about the key.
	if resp := e.signedAs(a, "PUT", "/tb/missing?tagging", tagDoc("no"), nil); resp.StatusCode != 403 {
		t.Fatalf("missing key, denied: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestObjectResponsesSandboxActiveContent: objects share the web UI's
// origin, so GetObject/HeadObject responses carry a sandbox CSP (PDFs
// excepted) and nosniff, including when response-content-type overrides
// the stored type.
func TestObjectResponsesSandboxActiveContent(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "x.html", []byte("<script>alert(1)</script>"), storage.ObjectMeta{ContentType: "text/html"})
	e.putObject("b", "doc.pdf", []byte("%PDF-1.4"), storage.ObjectMeta{ContentType: "Application/PDF"})
	for _, tc := range []struct {
		method, path, csp string
	}{
		{"GET", "/b/x.html", "sandbox"},
		{"HEAD", "/b/x.html", "sandbox"},
		{"GET", "/b/x.html?partNumber=1", "sandbox"},
		{"GET", "/b/doc.pdf", ""},
		{"GET", "/b/doc.pdf?response-content-type=text%2Fhtml", "sandbox"},
	} {
		resp := e.signed(tc.method, tc.path, nil, nil)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, resp.Data)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != tc.csp {
			t.Errorf("%s %s: CSP %q, want %q", tc.method, tc.path, got, tc.csp)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options %q", tc.method, tc.path, got)
		}
	}
}

// TestGetObjectIfRange: Range is only honoured when the If-Range validator
// still matches; otherwise the whole (new) object is returned with 200.
func TestGetObjectIfRange(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	info := e.putObject("b", "k", []byte("0123456789"), storage.ObjectMeta{})
	etag := `"` + info.ETag + `"`
	lm := info.LastModified.UTC().Format(http.TimeFormat)
	old := info.LastModified.Add(-time.Hour).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		ifRange string
		status  int
		body    string
	}{
		{"", 206, "0123"},
		{etag, 206, "0123"},
		{lm, 206, "0123"},
		{`"deadbeef"`, 200, "0123456789"},
		{"W/" + etag, 200, "0123456789"},
		{old, 200, "0123456789"},
		{"garbage", 200, "0123456789"},
	} {
		for _, m := range []string{"GET", "HEAD"} {
			hdr := objHdr("Range", "bytes=0-3")
			if tc.ifRange != "" {
				hdr.Set("If-Range", tc.ifRange)
			}
			resp := e.signed(m, "/b/k", nil, hdr)
			if resp.StatusCode != tc.status {
				t.Errorf("%s If-Range %q: %d, want %d", m, tc.ifRange, resp.StatusCode, tc.status)
				continue
			}
			if m == "GET" && string(resp.Data) != tc.body {
				t.Errorf("If-Range %q: body %q", tc.ifRange, resp.Data)
			}
			if tc.status == 200 && resp.Header.Get("Content-Range") != "" {
				t.Errorf("If-Range %q: Content-Range on 200", tc.ifRange)
			}
		}
	}
	// A mismatched validator also disables an unsatisfiable range.
	resp := e.signed("GET", "/b/k", nil, objHdr("Range", "bytes=50-60", "If-Range", `"deadbeef"`))
	if resp.StatusCode != 200 {
		t.Errorf("unsatisfiable range with stale If-Range: %d", resp.StatusCode)
	}
}

package s3

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestCopySourceConditionOnlyForCopies: s3:x-amz-copy-source describes the
// source a copy reads, so a PostObject (which ignores its own headers)
// carrying the header cannot satisfy a "PutObject only as a copy from
// here" Allow and upload arbitrary bytes.
func TestCopySourceConditionOnlyForCopies(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("pubsrc")
	e.createBucket("dstpost")
	e.putObject("pubsrc", "a", []byte("public source"), storage.ObjectMeta{})
	e.putPolicy("copyonly", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubsrc/*"},
		{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::dstpost/*",
		 "Condition":{"StringLike":{"s3:x-amz-copy-source":"pubsrc/*"}}}]}`)
	u := e.createUser("copier", "copyonly")

	if resp := e.signedAs(u, "PUT", "/dstpost/plain", []byte("x"), nil); resp.StatusCode != 403 {
		t.Fatalf("plain put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(u, "PUT", "/dstpost/copy", nil, objHdr("X-Amz-Copy-Source", "pubsrc/a")); resp.StatusCode != 200 {
		t.Fatalf("copy from pubsrc: %d %s", resp.StatusCode, resp.Data)
	}

	conds := []any{map[string]string{"bucket": "dstpost"}, []any{"starts-with", "$key", ""}}
	for _, hdr := range []http.Header{nil, objHdr("X-Amz-Copy-Source", "pubsrc/a"), objHdr("X-Amz-Copy-Source", "/pubsrc/a")} {
		fields := objSignedPostFields(t, u, time.Now().Add(time.Hour), conds, objPostField{"key", "p3"})
		body, ct := objPostBody(t, fields, "f.txt", []byte("ARBITRARY CONTENT"))
		h := hdr.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set("Content-Type", ct)
		if resp := e.anon("POST", "/dstpost", body, h); resp.StatusCode != 403 {
			t.Errorf("PostObject with %v: %d %s", hdr, resp.StatusCode, resp.Data)
		}
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "dstpost", "p3", ""); err == nil {
		t.Fatal("PostObject wrote dstpost/p3")
	}

	// x-amz-metadata-directive is likewise a CopyObject-only key.
	e.putPolicy("replaceonly", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::dstpost/*",
		 "Condition":{"StringEquals":{"s3:x-amz-metadata-directive":"REPLACE"}}}]}`)
	r := e.createUser("replacer", "replaceonly")
	if resp := e.signedAs(r, "PUT", "/dstpost/md", []byte("x"), objHdr("X-Amz-Metadata-Directive", "REPLACE")); resp.StatusCode != 403 {
		t.Errorf("PutObject with x-amz-metadata-directive: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestCopySourceConditionIsParsedSource: s3:x-amz-copy-source is the
// source the copy actually reads (decoded, either slash spelling), so a
// leading slash or percent-encoding neither dodges a Deny on the source
// nor trips a "deny unless from here" policy.
func TestCopySourceConditionIsParsedSource(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	alice := e.createUser("alice", "readwrite")
	for _, b := range []string{"secret-src", "pubsrc", "dst", "dst2"} {
		e.createBucket(b)
	}
	e.putObject("secret-src", "doc.txt", []byte("TOP SECRET"), storage.ObjectMeta{})
	e.putObject("pubsrc", "my dir/a", []byte("public"), storage.ObjectMeta{})
	e.putObject("pubsrc", "other", []byte("other"), storage.ObjectMeta{})
	e.putBucketConfig("dst", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::dst/*",
		 "Condition":{"StringLike":{"s3:x-amz-copy-source":"secret-src/*"}}}]}`)
	for i, src := range []string{"secret-src/doc.txt", "/secret-src/doc.txt", "%73ecret-src/doc.txt", "/%73ecret-src%2Fdoc.txt", " /secret-src/doc.txt "} {
		key := "/dst/c" + string(rune('a'+i))
		if resp := e.signedAs(alice, "PUT", key, nil, objHdr("X-Amz-Copy-Source", src)); resp.StatusCode != 403 {
			t.Errorf("CopyObject from %q: %d %s", src, resp.StatusCode, resp.Data)
		}
	}

	// UploadPartCopy reads the same key.
	resp := e.signedAs(alice, "POST", "/dst/mp?uploads", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create upload: %d %s", resp.StatusCode, resp.Data)
	}
	id := xmlField(t, resp.Data, "UploadId")
	for _, src := range []string{"secret-src/doc.txt", "/secret-src/doc.txt", "%73ecret-src/doc.txt"} {
		if resp := e.signedAs(alice, "PUT", "/dst/mp?partNumber=1&uploadId="+id, nil, objHdr("X-Amz-Copy-Source", src)); resp.StatusCode != 403 {
			t.Errorf("UploadPartCopy from %q: %d %s", src, resp.StatusCode, resp.Data)
		}
	}

	// "Deny unless copied from pubsrc/my dir/": legitimate copies pass in
	// any spelling, others are denied.
	e.putBucketConfig("dst2", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::dst2/*",
		 "Condition":{"StringNotLike":{"s3:x-amz-copy-source":"pubsrc/my dir/*"}}}]}`)
	for _, src := range []string{"pubsrc/my%20dir/a", "/pubsrc/my%20dir/a", "pubsrc/my dir/a"} {
		if resp := e.signedAs(alice, "PUT", "/dst2/ok", nil, objHdr("X-Amz-Copy-Source", src)); resp.StatusCode != 200 {
			t.Errorf("allowed copy from %q: %d %s", src, resp.StatusCode, resp.Data)
		}
	}
	for _, src := range []string{"pubsrc/other", "/pubsrc/other", "pubsrc/my%20dir"} {
		if resp := e.signedAs(alice, "PUT", "/dst2/no", nil, objHdr("X-Amz-Copy-Source", src)); resp.StatusCode/100 == 2 {
			t.Errorf("copy from %q: %d", src, resp.StatusCode)
		}
	}
}

// TestConditionalWriteConditionKeys: s3:if-none-match, s3:if-match and
// s3:ObjectCreationOperation are set as on AWS, so the documented
// "require If-None-Match on object creation" bucket policy works.
func TestConditionalWriteConditionKeys(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	e.createBucket("cw")
	e.createBucket("cw2")
	e.putObject("cw", "src", []byte("source"), storage.ObjectMeta{})
	e.putBucketConfig("cw", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::cw/*",
		 "Condition":{"Bool":{"s3:ObjectCreationOperation":"true"},"Null":{"s3:if-none-match":"true"}}}]}`)
	inm := objHdr("If-None-Match", "*")

	if resp := e.signedAs(bob, "PUT", "/cw/a", []byte("x"), nil); resp.StatusCode != 403 {
		t.Errorf("unconditional put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/cw/a", []byte("x"), inm); resp.StatusCode != 200 {
		t.Errorf("If-None-Match put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/cw/b", nil, objHdr("X-Amz-Copy-Source", "cw/src")); resp.StatusCode != 403 {
		t.Errorf("unconditional copy: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/cw/b", nil, objHdr("X-Amz-Copy-Source", "cw/src", "If-None-Match", "*")); resp.StatusCode != 200 {
		t.Errorf("If-None-Match copy: %d %s", resp.StatusCode, resp.Data)
	}

	// Multipart: create and part uploads are not object creation; the
	// completion is.
	resp := e.signedAs(bob, "POST", "/cw/mp?uploads", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create upload: %d %s", resp.StatusCode, resp.Data)
	}
	id := xmlField(t, resp.Data, "UploadId")
	resp = e.signedAs(bob, "PUT", "/cw/mp?partNumber=1&uploadId="+id, []byte("part"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("upload part: %d %s", resp.StatusCode, resp.Data)
	}
	etag := resp.Header.Get("ETag")
	complete := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + etag + `</ETag></Part></CompleteMultipartUpload>`)
	if resp := e.signedAs(bob, "POST", "/cw/mp?uploadId="+id, complete, nil); resp.StatusCode != 403 {
		t.Errorf("unconditional complete: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "POST", "/cw/mp?uploadId="+id, complete, inm); resp.StatusCode != 200 || !bytes.Contains(resp.Data, []byte("CompleteMultipartUploadResult")) {
		t.Errorf("If-None-Match complete: %d %s", resp.StatusCode, resp.Data)
	}

	// The Null-only form does not deny writes that send If-None-Match.
	e.putBucketConfig("cw2", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::cw2/*",
		 "Condition":{"Null":{"s3:if-none-match":"true"}}}]}`)
	if resp := e.signedAs(bob, "PUT", "/cw2/a", []byte("x"), inm); resp.StatusCode != 200 {
		t.Errorf("Null-only policy, If-None-Match put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/cw2/b", []byte("x"), nil); resp.StatusCode != 403 {
		t.Errorf("Null-only policy, unconditional put: %d %s", resp.StatusCode, resp.Data)
	}

	// s3:if-match carries the If-Match value.
	e.putBucketConfig("cw2", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::cw2/*",
		 "Condition":{"Null":{"s3:if-match":"true"},"Bool":{"s3:ObjectCreationOperation":"true"}}}]}`)
	info, err := e.backend.GetObjectInfo(context.Background(), "cw2", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := e.signedAs(bob, "PUT", "/cw2/a", []byte("y"), nil); resp.StatusCode != 403 {
		t.Errorf("If-Match policy, unconditional overwrite: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/cw2/a", []byte("y"), objHdr("If-Match", `"`+info.ETag+`"`)); resp.StatusCode != 200 {
		t.Errorf("If-Match policy, If-Match overwrite: %d %s", resp.StatusCode, resp.Data)
	}
}

// xmlField returns the text of the first <name> element of an XML body.
func xmlField(t *testing.T, data []byte, name string) string {
	t.Helper()
	_, rest, ok := bytes.Cut(data, []byte("<"+name+">"))
	v, _, ok2 := bytes.Cut(rest, []byte("</"+name+">"))
	if !ok || !ok2 || len(v) == 0 {
		t.Fatalf("no <%s> in %s", name, data)
	}
	return string(v)
}

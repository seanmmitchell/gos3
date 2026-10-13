package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- CopyObject ---

func TestObjectCopySDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("src")
	e.createBucket("dst")
	ctx := context.Background()
	data := []byte("copy me please")
	src := e.putObject("src", "a/b c", data, storage.ObjectMeta{
		ContentType: "text/plain", CacheControl: "no-store", UserMeta: map[string]string{"k": "v"},
		Tags: map[string]string{"t": "1"}, Checksums: map[string]string{ChecksumSHA256: computeChecksum(ChecksumSHA256, data)},
		ChecksumType: ChecksumTypeFullObject, WebsiteRedirect: "/x",
	})

	// COPY directives: metadata and tags come from the source.
	out, err := e.client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copy1"), CopySource: aws.String("src/" + url.PathEscape("a/b c")),
		ContentType: aws.String("ignored/with-copy"),
	})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if aws.ToString(out.CopyObjectResult.ETag) != `"`+src.ETag+`"` {
		t.Errorf("ETag %v", aws.ToString(out.CopyObjectResult.ETag))
	}
	if out.CopyObjectResult.LastModified == nil || aws.ToString(out.CopyObjectResult.ChecksumSHA256) != src.Checksums[ChecksumSHA256] {
		t.Errorf("result %+v", out.CopyObjectResult)
	}
	if out.CopySourceVersionId != nil || out.VersionId != nil {
		t.Errorf("unexpected versions %v %v", out.CopySourceVersionId, out.VersionId)
	}
	info := objGetInfo(t, e, "dst", "copy1", "")
	if info.ContentType != "text/plain" || info.CacheControl != "no-store" || info.UserMeta["k"] != "v" ||
		info.Tags["t"] != "1" || info.WebsiteRedirect != "" || info.Size != int64(len(data)) {
		t.Errorf("copied meta %+v", info.ObjectMeta)
	}
	get, err := e.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("dst"), Key: aws.String("copy1")})
	if err != nil || !bytes.Equal(objBody(t, get), data) {
		t.Fatalf("GET copy: %v", err)
	}

	// REPLACE directives.
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copy2"), CopySource: aws.String("src/a/b%20c"),
		MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String("application/x-new"),
		Metadata: map[string]string{"n": "new"}, TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("x=y"),
		StorageClass: types.StorageClassGlacier, ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	})
	if err != nil {
		t.Fatalf("CopyObject REPLACE: %v", err)
	}
	info = objGetInfo(t, e, "dst", "copy2", "")
	if info.ContentType != "application/x-new" || info.UserMeta["n"] != "new" || info.UserMeta["k"] != "" ||
		len(info.Tags) != 1 || info.Tags["x"] != "y" || info.StorageClass != "GLACIER" ||
		info.Checksums[ChecksumCRC32] != computeChecksum(ChecksumCRC32, data) || info.ETag != md5Hex(data) {
		t.Errorf("replaced meta %+v", info.ObjectMeta)
	}

	// Errors.
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("src/missing")})
	objSDKErr(t, err, 404, "NoSuchKey")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("nosrc/k")})
	objSDKErr(t, err, 404, "NoSuchBucket")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("nodst"), Key: aws.String("x"), CopySource: aws.String("src/a/b%20c")})
	objSDKErr(t, err, 404, "NoSuchBucket")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("a/b c"), CopySource: aws.String("src/a/b%20c")})
	objSDKErr(t, err, 400, "InvalidRequest")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("src/a/b%20c"),
		CopySourceIfMatch: aws.String(`"nope"`)})
	objSDKErr(t, err, 412, "PreconditionFailed")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("src/a/b%20c"),
		CopySourceIfNoneMatch: aws.String(`"` + src.ETag + `"`)})
	objSDKErr(t, err, 412, "PreconditionFailed")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("src/a/b%20c"),
		CopySourceIfModifiedSince: aws.Time(time.Now().Add(time.Hour))})
	objSDKErr(t, err, 412, "PreconditionFailed")
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x"), CopySource: aws.String("src/a/b%20c"),
		CopySourceIfMatch: aws.String(`"` + src.ETag + `"`)})
	if err != nil {
		t.Errorf("matching copy-source-if-match: %v", err)
	}

	// Copy in place with REPLACE is allowed (metadata update).
	_, err = e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("a/b c"), CopySource: aws.String("src/a/b%20c"),
		MetadataDirective: types.MetadataDirectiveReplace, Metadata: map[string]string{"updated": "yes"}})
	if err != nil {
		t.Fatalf("copy in place: %v", err)
	}
	if info := objGetInfo(t, e, "src", "a/b c", ""); info.UserMeta["updated"] != "yes" || info.Size != int64(len(data)) {
		t.Errorf("in-place copy %+v", info.ObjectMeta)
	}
}

func TestObjectCopyRaw(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	data := []byte("raw copy data")
	src := e.putObject("bkt", "src", data, storage.ObjectMeta{})
	resp := e.signed("PUT", "/bkt/dst", nil, objHdr("X-Amz-Copy-Source", "/bkt/src"))
	objExpect(t, resp, 200, "")
	var res struct {
		XMLName           xml.Name
		ETag              string
		LastModified      string
		ChecksumCRC64NVME string
		ChecksumType      string
	}
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatalf("%v: %s", err, resp.Data)
	}
	if res.XMLName.Local != "CopyObjectResult" || res.XMLName.Space != s3Namespace || res.ETag != `"`+src.ETag+`"` ||
		!strings.HasSuffix(res.LastModified, "Z") || res.ChecksumCRC64NVME != computeChecksum(ChecksumCRC64NVME, data) ||
		res.ChecksumType != ChecksumTypeFullObject {
		t.Errorf("result %+v", res)
	}
	for _, tc := range []struct {
		name   string
		hdr    http.Header
		status int
		code   string
	}{
		{"range not allowed", objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Copy-Source-Range", "bytes=0-1"), 400, "InvalidArgument"},
		{"bad directive", objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Metadata-Directive", "MOVE"), 400, "InvalidArgument"},
		{"bad tagging directive", objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Tagging-Directive", "X"), 400, "InvalidArgument"},
		{"bad source", objHdr("X-Amz-Copy-Source", "nokey"), 400, "InvalidArgument"},
		{"bad checksum algo", objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Checksum-Algorithm", "MD5"), 400, "InvalidRequest"},
		{"bad source version", objHdr("X-Amz-Copy-Source", "bkt/src?versionId=a%20b"), 400, "InvalidArgument"},
		{"missing source version", objHdr("X-Amz-Copy-Source", "bkt/src?versionId=abc"), 404, "NoSuchVersion"},
		{"self copy", objHdr("X-Amz-Copy-Source", "bkt/src"), 400, "InvalidRequest"},
		{"sse-c source", objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Algorithm", "AES256"), 501, "NotImplemented"},
		{"if-none-match dest exists", objHdr("X-Amz-Copy-Source", "bkt/src", "If-None-Match", "*"), 412, "PreconditionFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/bkt/dst"
			if tc.name == "self copy" {
				path = "/bkt/src"
			}
			objExpect(t, e.signed("PUT", path, nil, tc.hdr), tc.status, tc.code)
		})
	}
	// Self copy with a storage class change is allowed.
	objExpect(t, e.signed("PUT", "/bkt/src", nil, objHdr("X-Amz-Copy-Source", "bkt/src", "X-Amz-Storage-Class", "STANDARD_IA")), 200, "")
	if info := objGetInfo(t, e, "bkt", "src", ""); info.StorageClass != "STANDARD_IA" {
		t.Errorf("storage class %q", info.StorageClass)
	}
}

func TestObjectCopySizeLimit(t *testing.T) {
	e := newTestEnv(t, withOptions(func(o *Options) { o.MaxObjectSize = 8 }))
	e.createBucket("bkt")
	e.putObject("bkt", "big", []byte("0123456789"), storage.ObjectMeta{})
	objExpect(t, e.signed("PUT", "/bkt/copy", nil, objHdr("X-Amz-Copy-Source", "bkt/big")), 400, "InvalidRequest")
}

func TestObjectCopyVersions(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	v1 := e.putObject("bkt", "k", []byte("one"), storage.ObjectMeta{}).VersionID
	e.putObject("bkt", "k", []byte("two"), storage.ObjectMeta{})
	ctx := context.Background()
	out, err := e.client.CopyObject(ctx, &awss3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		CopySource: aws.String("bkt/k?versionId=" + v1)})
	if err != nil {
		t.Fatalf("restore old version: %v", err)
	}
	if aws.ToString(out.CopySourceVersionId) != v1 || aws.ToString(out.VersionId) == "" || aws.ToString(out.VersionId) == v1 {
		t.Errorf("versions src=%v dst=%v", aws.ToString(out.CopySourceVersionId), aws.ToString(out.VersionId))
	}
	g := e.signed("GET", "/bkt/k", nil, nil)
	if string(g.Data) != "one" {
		t.Errorf("latest %q", g.Data)
	}
	// Source delete markers.
	dm := e.signed("DELETE", "/bkt/k", nil, nil).Header.Get("X-Amz-Version-Id")
	objExpect(t, e.signed("PUT", "/bkt/c", nil, objHdr("X-Amz-Copy-Source", "bkt/k")), 404, "NoSuchKey")
	objExpect(t, e.signed("PUT", "/bkt/c", nil, objHdr("X-Amz-Copy-Source", "bkt/k?versionId="+dm)), 400, "InvalidRequest")
}

func TestObjectCopyAuthorization(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("secret")
	e.createBucket("mine")
	e.putObject("secret", "k", []byte("x"), storage.ObjectMeta{})
	e.putObject("mine", "k", []byte("y"), storage.ObjectMeta{})
	e.putPolicy("mine-only", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::mine/*"]}]}`)
	u := e.createUser("copier", "mine-only")
	objExpect(t, e.signedAs(u, "PUT", "/mine/stolen", nil, objHdr("X-Amz-Copy-Source", "secret/k")), 403, "AccessDenied")
	objExpect(t, e.signedAs(u, "PUT", "/mine/copy", nil, objHdr("X-Amz-Copy-Source", "mine/k")), 200, "")
	objExpect(t, e.signedAs(u, "PUT", "/secret/x", nil, objHdr("X-Amz-Copy-Source", "mine/k")), 403, "AccessDenied")
}

// --- DeleteObjects ---

func TestObjectDeleteObjectsSDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		e.putObject("bkt", fmt.Sprintf("k%d", i), []byte("x"), storage.ObjectMeta{})
	}
	out, err := e.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("k0")}, {Key: aws.String("k1")}, {Key: aws.String("missing")}},
	}})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(out.Deleted) != 3 || len(out.Errors) != 0 {
		t.Fatalf("result %+v", out)
	}
	for _, d := range out.Deleted {
		if d.VersionId != nil || aws.ToBool(d.DeleteMarker) {
			t.Errorf("unversioned entry %+v", d)
		}
	}
	if _, err := e.backend.GetObjectInfo(ctx, "bkt", "k0", ""); err == nil {
		t.Errorf("k0 not deleted")
	}
	// Quiet mode reports nothing on success.
	out, err = e.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("k2")}}, Quiet: aws.Bool(true),
	}})
	if err != nil || len(out.Deleted) != 0 || len(out.Errors) != 0 {
		t.Errorf("quiet: %v %+v", err, out)
	}
	_, err = e.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: aws.String("none"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("k")}}}})
	objSDKErr(t, err, 404, "NoSuchBucket")
}

func TestObjectDeleteObjectsVersioned(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	ctx := context.Background()
	v1 := e.putObject("bkt", "a", []byte("1"), storage.ObjectMeta{}).VersionID
	e.putObject("bkt", "b", []byte("2"), storage.ObjectMeta{})
	out, err := e.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("a"), VersionId: aws.String(v1)}, {Key: aws.String("b")}},
	}})
	if err != nil || len(out.Deleted) != 2 {
		t.Fatalf("DeleteObjects: %v %+v", err, out)
	}
	byKey := map[string]types.DeletedObject{}
	for _, d := range out.Deleted {
		byKey[aws.ToString(d.Key)] = d
	}
	if d := byKey["a"]; aws.ToString(d.VersionId) != v1 || aws.ToBool(d.DeleteMarker) {
		t.Errorf("version delete %+v", d)
	}
	dmEntry := byKey["b"]
	if !aws.ToBool(dmEntry.DeleteMarker) || aws.ToString(dmEntry.DeleteMarkerVersionId) == "" || dmEntry.VersionId != nil {
		t.Errorf("delete marker entry %+v", dmEntry)
	}
	// Deleting the delete marker by version reports DeleteMarker=true.
	dm := aws.ToString(dmEntry.DeleteMarkerVersionId)
	out, err = e.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("b"), VersionId: aws.String(dm)}},
	}})
	if err != nil || len(out.Deleted) != 1 || !aws.ToBool(out.Deleted[0].DeleteMarker) || aws.ToString(out.Deleted[0].DeleteMarkerVersionId) != dm {
		t.Errorf("delete DM: %v %+v", err, out)
	}
	if g := e.signed("GET", "/bkt/b", nil, nil); string(g.Data) != "2" {
		t.Errorf("b not restored: %d", g.StatusCode)
	}
}

func TestObjectDeleteObjectsPerKeyErrors(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	e.putObject("bkt", "pub/a", []byte("x"), storage.ObjectMeta{})
	e.putObject("bkt", "priv/b", []byte("x"), storage.ObjectMeta{})
	e.putPolicy("del-pub", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::bkt/pub/*"}]}`)
	u := e.createUser("deleter", "del-pub")
	out, err := e.clientFor(u).DeleteObjects(context.Background(), &awss3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("pub/a")}, {Key: aws.String("priv/b")}, {Key: aws.String("pub/c"), VersionId: aws.String("v1")}},
	}})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(out.Deleted) != 1 || aws.ToString(out.Deleted[0].Key) != "pub/a" {
		t.Errorf("deleted %+v", out.Deleted)
	}
	if len(out.Errors) != 2 {
		t.Fatalf("errors %+v", out.Errors)
	}
	for _, er := range out.Errors {
		if aws.ToString(er.Code) != "AccessDenied" || aws.ToString(er.Message) == "" {
			t.Errorf("error entry %+v", er)
		}
	}
	if aws.ToString(out.Errors[1].VersionId) != "v1" {
		t.Errorf("version in error %+v", out.Errors[1])
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "bkt", "priv/b", ""); err != nil {
		t.Errorf("priv/b was deleted")
	}
	// Anonymous callers get per-key AccessDenied too.
	body := `<Delete><Object><Key>pub/x</Key></Object></Delete>`
	resp := e.anon("POST", "/bkt?delete", []byte(body), objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16([]byte(body)))))
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Data), "<Code>AccessDenied</Code>") {
		t.Errorf("anonymous: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestObjectDeleteObjectsRaw(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putObject("bkt", "a&b<c", []byte("x"), storage.ObjectMeta{})
	md5h := func(b string) http.Header {
		return objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16([]byte(b))))
	}
	body := `<?xml version="1.0" encoding="UTF-8"?><Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Object><Key>a&amp;b&lt;c</Key></Object><Object><Key>bad</Key><VersionId>bad id</VersionId></Object></Delete>`
	resp := e.signed("POST", "/bkt?delete", []byte(body), md5h(body))
	objExpect(t, resp, 200, "")
	var res struct {
		XMLName xml.Name
		Deleted []struct{ Key string }
		Error   []struct{ Key, VersionId, Code string }
	}
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.XMLName.Local != "DeleteResult" || res.XMLName.Space != s3Namespace || len(res.Deleted) != 1 || res.Deleted[0].Key != "a&b<c" ||
		len(res.Error) != 1 || res.Error[0].Code != "InvalidArgument" || res.Error[0].VersionId != "bad id" {
		t.Errorf("result %s", resp.Data)
	}

	var many strings.Builder
	many.WriteString("<Delete>")
	for i := 0; i < 1001; i++ {
		fmt.Fprintf(&many, "<Object><Key>k%d</Key></Object>", i)
	}
	many.WriteString("</Delete>")
	for _, tc := range []struct {
		name   string
		body   string
		hdr    http.Header
		status int
		code   string
	}{
		{"too many keys", many.String(), md5h(many.String()), 400, "MalformedXML"},
		{"no keys", "<Delete></Delete>", md5h("<Delete></Delete>"), 400, "MalformedXML"},
		{"empty key", "<Delete><Object><Key></Key></Object></Delete>", nil, 400, "MalformedXML"},
		{"not xml", "nope", nil, 400, "MalformedXML"},
		{"wrong root", "<Remove><Object><Key>a</Key></Object></Remove>", nil, 400, "MalformedXML"},
		{"empty body", "", nil, 400, "MissingRequestBodyError"},
		{"md5 mismatch", "<Delete><Object><Key>a</Key></Object></Delete>", md5h("other"), 400, "BadDigest"},
		{"checksum mismatch", "<Delete><Object><Key>a</Key></Object></Delete>",
			objHdr("X-Amz-Checksum-Crc32", computeChecksum(ChecksumCRC32, []byte("other"))), 400, "BadDigest"},
		{"checksum ok", "<Delete><Quiet>true</Quiet><Object><Key>a</Key></Object></Delete>",
			objHdr("X-Amz-Checksum-Crc32", computeChecksum(ChecksumCRC32, []byte("<Delete><Quiet>true</Quiet><Object><Key>a</Key></Object></Delete>"))), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b []byte
			if tc.body != "" {
				b = []byte(tc.body)
			}
			objExpect(t, e.signed("POST", "/bkt?delete", b, tc.hdr), tc.status, tc.code)
		})
	}

	// Conditional per-key deletes with <ETag>.
	info := e.putObject("bkt", "cond", []byte("x"), storage.ObjectMeta{})
	body = `<Delete><Object><Key>cond</Key><ETag>"wrong"</ETag></Object></Delete>`
	resp = e.signed("POST", "/bkt?delete", []byte(body), md5h(body))
	if !strings.Contains(string(resp.Data), "<Code>PreconditionFailed</Code>") {
		t.Errorf("conditional: %s", resp.Data)
	}
	body = `<Delete><Object><Key>cond</Key><ETag>"` + info.ETag + `"</ETag></Object></Delete>`
	resp = e.signed("POST", "/bkt?delete", []byte(body), md5h(body))
	if !strings.Contains(string(resp.Data), "<Deleted><Key>cond</Key></Deleted>") {
		t.Errorf("conditional ok: %s", resp.Data)
	}
}

func TestObjectDeleteObjectsLocked(t *testing.T) {
	e := newTestEnv(t)
	objLockBucket(t, e, "lock")
	v := e.putObject("lock", "k", []byte("x"), storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour)}).VersionID
	body := `<Delete><Object><Key>k</Key><VersionId>` + v + `</VersionId></Object></Delete>`
	sum := base64.StdEncoding.EncodeToString(md5Sum16([]byte(body)))
	resp := e.signed("POST", "/lock?delete", []byte(body), objHdr("Content-MD5", sum))
	if !strings.Contains(string(resp.Data), "<Code>AccessDenied</Code>") {
		t.Errorf("locked: %s", resp.Data)
	}
	resp = e.signed("POST", "/lock?delete", []byte(body), objHdr("X-Amz-Bypass-Governance-Retention", "true", "Content-MD5", sum))
	if !strings.Contains(string(resp.Data), "<Deleted>") {
		t.Errorf("bypass: %s", resp.Data)
	}
}

func TestParseDirective(t *testing.T) {
	for _, tc := range []struct {
		v, want string
		ok      bool
	}{{"", "COPY", true}, {"COPY", "COPY", true}, {"replace", "REPLACE", true}, {" REPLACE ", "REPLACE", true}, {"MOVE", "", false}} {
		got, err := parseDirective(objHdr("X-Amz-Metadata-Directive", tc.v), "X-Amz-Metadata-Directive")
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("%q: %q %v", tc.v, got, err)
		}
	}
}

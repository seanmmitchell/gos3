package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// objPostField is one form field (order matters for POST uploads).
type objPostField struct{ name, value string }

// objPostBody builds a multipart/form-data POST body: fields, then the
// file part (omitted when filename is ""), then trailing fields.
func objPostBody(t *testing.T, fields []objPostField, filename string, file []byte, after ...objPostField) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f.name, f.value); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(file)
	}
	for _, f := range after {
		if err := mw.WriteField(f.name, f.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// objSignedPostFields signs a POST policy for creds with the given
// conditions and returns the auth fields followed by extra.
func objSignedPostFields(t *testing.T, c auth.Credentials, exp time.Time, conds []any, extra ...objPostField) []objPostField {
	t.Helper()
	form, err := auth.SignPostPolicy(c, "us-east-1", time.Now(), exp, conds)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]objPostField{}, extra...)
	for _, k := range []string{"x-amz-algorithm", "x-amz-credential", "x-amz-date", "policy", "x-amz-signature"} {
		out = append(out, objPostField{k, form[k]})
	}
	return out
}

// objPost sends a POST upload to bucket.
func objPost(e *testEnv, bucket string, body []byte, ct string) *testResponse {
	return e.anon("POST", "/"+bucket, body, objHdr("Content-Type", ct))
}

func TestObjectPostUpload(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	exp := time.Now().Add(time.Hour)
	file := []byte("file contents from a browser")
	base := []any{
		map[string]string{"bucket": "bkt"},
		[]any{"starts-with", "$key", "uploads/"},
		[]any{"content-length-range", 1, 1000},
	}

	// Default: 204 with ETag and Location; ${filename} substitution.
	fields := objSignedPostFields(t, testRoot, exp, base, objPostField{"key", "uploads/${filename}"})
	body, ct := objPostBody(t, fields, "photo.txt", file)
	resp := objPost(e, "bkt", body, ct)
	objExpect(t, resp, 204, "")
	if resp.Header.Get("ETag") != `"`+md5Hex(file)+`"` || !strings.HasSuffix(resp.Header.Get("Location"), "/bkt/uploads/photo.txt") {
		t.Errorf("headers %v", resp.Header)
	}
	info := objGetInfo(t, e, "bkt", "uploads/photo.txt", "")
	if info.Size != int64(len(file)) || info.ETag != md5Hex(file) || info.Checksums[ChecksumCRC64NVME] == "" {
		t.Errorf("stored %+v", info)
	}

	// 201 returns a PostResponse document.
	conds := append(append([]any{}, base...), map[string]string{"success_action_status": "201"})
	fields = objSignedPostFields(t, testRoot, exp, conds, objPostField{"key", "uploads/a b.txt"}, objPostField{"success_action_status", "201"})
	body, ct = objPostBody(t, fields, "x", file)
	resp = objPost(e, "bkt", body, ct)
	objExpect(t, resp, 201, "")
	var pr struct {
		XMLName                     xml.Name
		Location, Bucket, Key, ETag string
	}
	if err := xml.Unmarshal(resp.Data, &pr); err != nil || pr.XMLName.Local != "PostResponse" || pr.Bucket != "bkt" ||
		pr.Key != "uploads/a b.txt" || pr.ETag != `"`+md5Hex(file)+`"` || !strings.HasSuffix(pr.Location, "/bkt/uploads/a%20b.txt") {
		t.Errorf("PostResponse %v %s", err, resp.Data)
	}

	// 200 and redirects.
	conds = append(append([]any{}, base...), map[string]string{"success_action_status": "200"})
	fields = objSignedPostFields(t, testRoot, exp, conds, objPostField{"key", "uploads/ok"}, objPostField{"success_action_status", "200"})
	body, ct = objPostBody(t, fields, "x", file)
	objExpect(t, objPost(e, "bkt", body, ct), 200, "")
	conds = append(append([]any{}, base...), []any{"starts-with", "$success_action_redirect", "https://app.example/"})
	fields = objSignedPostFields(t, testRoot, exp, conds, objPostField{"key", "uploads/r"},
		objPostField{"success_action_redirect", "https://app.example/done?x=1"})
	body, ct = objPostBody(t, fields, "x", file)
	req := e.newRequest("POST", "/bkt", body, objHdr("Content-Type", ct))
	hc := *e.httpc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hresp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	loc, _ := url.Parse(hresp.Header.Get("Location"))
	if hresp.StatusCode != 303 || loc == nil || loc.Host != "app.example" || loc.Query().Get("x") != "1" ||
		loc.Query().Get("bucket") != "bkt" || loc.Query().Get("key") != "uploads/r" || loc.Query().Get("etag") != `"`+md5Hex(file)+`"` {
		t.Errorf("redirect %d %v", hresp.StatusCode, hresp.Header)
	}

	// Metadata, content type, tagging, ACL and checksum fields.
	tagging := `<Tagging><TagSet><Tag><Key>t</Key><Value>v</Value></Tag></TagSet></Tagging>`
	sum := computeChecksum(ChecksumSHA256, file)
	conds = append(append([]any{}, base...),
		map[string]string{"content-type": "text/plain"}, []any{"starts-with", "$x-amz-meta-color", ""},
		map[string]string{"tagging": tagging}, map[string]string{"acl": "public-read"},
		map[string]string{"x-amz-checksum-sha256": sum}, map[string]string{"cache-control": "no-cache"})
	fields = objSignedPostFields(t, testRoot, exp, conds, objPostField{"key", "uploads/meta"},
		objPostField{"Content-Type", "text/plain"}, objPostField{"x-amz-meta-color", "red"}, objPostField{"tagging", tagging},
		objPostField{"acl", "public-read"}, objPostField{"x-amz-checksum-sha256", sum}, objPostField{"Cache-Control", "no-cache"})
	body, ct = objPostBody(t, fields, "x", file, objPostField{"ignored-after-file", "1"})
	resp = objPost(e, "bkt", body, ct)
	objExpect(t, resp, 204, "")
	info = objGetInfo(t, e, "bkt", "uploads/meta", "")
	if info.ContentType != "text/plain" || info.UserMeta["color"] != "red" || info.Tags["t"] != "v" || info.ACL != aclPublicRead ||
		info.Checksums[ChecksumSHA256] != sum || info.CacheControl != "no-cache" {
		t.Errorf("meta %+v", info.ObjectMeta)
	}
	if resp.Header.Get("X-Amz-Checksum-Sha256") != sum {
		t.Errorf("checksum header %v", resp.Header)
	}
}

func TestObjectPostErrors(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	exp := time.Now().Add(time.Hour)
	file := []byte("0123456789")
	base := []any{map[string]string{"bucket": "bkt"}, []any{"starts-with", "$key", "up/"}, []any{"content-length-range", 1, 5}}
	signed := func(extra ...objPostField) []objPostField {
		return objSignedPostFields(t, testRoot, exp, base, extra...)
	}
	e.putPolicy("nothing", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	weak := e.createUser("weak", "nothing")
	badSig := signed(objPostField{"key", "up/k"})
	for i := range badSig {
		if badSig[i].name == "x-amz-signature" {
			badSig[i].value = strings.Repeat("0", 64)
		}
	}
	bigField := objPostField{"x-ignore-pad", strings.Repeat("p", maxPostFieldsSize)}
	for _, tc := range []struct {
		name     string
		fields   []objPostField
		filename string
		file     []byte
		status   int
		code     string
	}{
		{"too large for policy", signed(objPostField{"key", "up/k"}), "f", file, 400, "EntityTooLarge"},
		{"empty for policy", signed(objPostField{"key", "up/k"}), "f", []byte{}, 400, "EntityTooSmall"},
		{"key outside policy", signed(objPostField{"key", "other/k"}), "f", file[:3], 403, "AccessDenied"},
		{"extra field", signed(objPostField{"key", "up/k"}, objPostField{"x-amz-meta-x", "y"}), "f", file[:3], 403, "AccessDenied"},
		{"expired", objSignedPostFields(t, testRoot, time.Now().Add(-time.Minute), base, objPostField{"key", "up/k"}), "f", file[:3], 403, "AccessDenied"},
		{"bad signature", badSig, "f", file[:3], 403, "SignatureDoesNotMatch"},
		{"user without PutObject", objSignedPostFields(t, weak, exp, base, objPostField{"key", "up/k"}), "f", file[:3], 403, "AccessDenied"},
		{"anonymous", []objPostField{{"key", "up/k"}}, "f", file[:3], 403, "AccessDenied"},
		{"missing key", signed(), "f", file[:3], 400, "InvalidArgument"},
		{"missing file", signed(objPostField{"key", "up/k"}), "", nil, 400, "InvalidArgument"},
		{"fields too large", append(signed(objPostField{"key", "up/k"}), bigField), "f", file[:3], 400, "MaxPostPreDataLengthExceededError"},
		{"duplicate field", append(signed(objPostField{"key", "up/k"}), objPostField{"KEY", "up/j"}), "f", file[:3], 400, "InvalidArgument"},
		{"checksum mismatch", objSignedPostFields(t, testRoot, exp, append(append([]any{}, base...), map[string]string{"x-amz-checksum-crc32": "AAAAAA=="}),
			objPostField{"key", "up/k"}, objPostField{"x-amz-checksum-crc32", "AAAAAA=="}), "f", file[:3], 400, "BadDigest"},
		{"bad tagging", objSignedPostFields(t, testRoot, exp, append(append([]any{}, base...), map[string]string{"tagging": "<nope"}),
			objPostField{"key", "up/k"}, objPostField{"tagging", "<nope"}), "f", file[:3], 400, "MalformedXML"},
		{"sigv2", []objPostField{{"key", "up/k"}, {"AWSAccessKeyId", "x"}, {"signature", "y"}, {"policy", "e30="}}, "f", file[:3], 403, "AccessDenied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := objPostBody(t, tc.fields, tc.filename, tc.file)
			objExpect(t, objPost(e, "bkt", body, ct), tc.status, tc.code)
			if _, err := e.backend.GetObjectInfo(context.Background(), "bkt", "up/k", ""); err == nil {
				t.Fatalf("object stored despite error")
			}
		})
	}
	// Structural errors.
	objExpect(t, objPost(e, "bkt", []byte("garbage"), "multipart/form-data"), 400, "MalformedPOSTRequest")
	objExpect(t, objPost(e, "bkt", []byte("garbage"), "multipart/form-data; boundary=xyz"), 400, "MalformedPOSTRequest")
	body, ct := objPostBody(t, signed(objPostField{"key", "up/k"}), "f", file[:3])
	objExpect(t, objPost(e, "bkt", body[:len(body)-20], ct), 400, "IncompleteBody")
	objExpect(t, objPost(e, "nobucket", body, ct), 404, "NoSuchBucket")
	// Oversized preamble (e.g. huge part headers) is cut off.
	var huge bytes.Buffer
	huge.WriteString("--b\r\nContent-Disposition: form-data; name=\"key\"\r\nX-Pad: ")
	huge.WriteString(strings.Repeat("a", maxPostPreamble+10))
	huge.WriteString("\r\n\r\nv\r\n--b--\r\n")
	objExpect(t, objPost(e, "bkt", huge.Bytes(), "multipart/form-data; boundary=b"), 400, "MaxPostPreDataLengthExceededError")
}

func TestObjectPostAnonymousWithBucketPolicy(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("drop")
	e.putBucketConfig("drop", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::drop/inbox/*"}]}`)
	body, ct := objPostBody(t, []objPostField{{"key", "inbox/${filename}"}}, "note.txt", []byte("hi"))
	objExpect(t, objPost(e, "drop", body, ct), 204, "")
	if info := objGetInfo(t, e, "drop", "inbox/note.txt", ""); info.Size != 2 {
		t.Errorf("size %d", info.Size)
	}
	body, ct = objPostBody(t, []objPostField{{"key", "elsewhere/x"}}, "note.txt", []byte("hi"))
	objExpect(t, objPost(e, "drop", body, ct), 403, "AccessDenied")
}

func TestObjectPostVersionedAndLocation(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) { s.Domains = []string{"s3.test"} }))
	e.createBucket("vb")
	objVersioned(t, e, "vb", storage.VersioningEnabled)
	fields := objSignedPostFields(t, testRoot, time.Now().Add(time.Hour), []any{map[string]string{"bucket": "vb"}, []any{"starts-with", "$key", ""}},
		objPostField{"key", "k"})
	body, ct := objPostBody(t, fields, "f", []byte("x"))
	resp := e.anon("POST", "/", body, objHdr("Content-Type", ct, "Host", "vb.s3.test"))
	objExpect(t, resp, 204, "")
	if resp.Header.Get("X-Amz-Version-Id") == "" || resp.Header.Get("Location") != "http://vb.s3.test/k" {
		t.Errorf("headers %v", resp.Header)
	}
}

func TestPostPolicyMaxSize(t *testing.T) {
	for _, tc := range []struct {
		conds []any
		want  int64
	}{
		{nil, -1},
		{[]any{[]any{"content-length-range", float64(1), float64(10)}}, 10},
		{[]any{[]any{"Content-Length-Range", "1", "20"}, []any{"content-length-range", 0, 15}}, 20},
		{[]any{[]any{"eq", "$key", "x"}}, -1},
	} {
		var p *auth.PostPolicyConditions
		if tc.conds != nil {
			p = &auth.PostPolicyConditions{Conditions: tc.conds}
		}
		if got := postPolicyMaxSize(p); got != tc.want {
			t.Errorf("%v: %d", tc.conds, got)
		}
	}
}

package e2e

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// fetch performs an unsigned request to an absolute (presigned) URL.
func (e *env) fetch(t *testing.T, method, rawURL string, body []byte, hdr http.Header) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx(t), method, rawURL, rd)
	must(t, err, "NewRequest")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := e.httpc.Do(req)
	must(t, err, method+" "+rawURL)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must(t, err, "read body")
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

func TestPresignedSDK(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			key := "presigned dir/ü+file.txt"
			ps := s3.NewPresignClient(c, func(o *s3.PresignOptions) { o.Expires = 10 * time.Minute })

			put, err := ps.PresignPutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), ContentType: aws.String("text/plain")})
			must(t, err, "PresignPutObject")
			r := e.fetch(t, put.Method, put.URL, []byte("via presigned put"), put.SignedHeader)
			if r.status != 200 || r.header.Get("ETag") != quoted(md5Hex([]byte("via presigned put"))) {
				t.Fatalf("presigned PUT: %d %s", r.status, r.body)
			}
			get, err := ps.PresignGetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key),
				ResponseContentDisposition: aws.String(`attachment; filename="x.txt"`)})
			must(t, err, "PresignGetObject")
			r = e.fetch(t, http.MethodGet, get.URL, nil, nil)
			if r.status != 200 || string(r.body) != "via presigned put" || r.header.Get("Content-Type") != "text/plain" ||
				r.header.Get("Content-Disposition") != `attachment; filename="x.txt"` {
				t.Fatalf("presigned GET: %d %q %v", r.status, r.body, r.header)
			}
			head, err := ps.PresignHeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
			must(t, err, "PresignHeadObject")
			if r := e.fetch(t, http.MethodHead, head.URL, nil, nil); r.status != 200 || r.header.Get("Content-Length") != "17" {
				t.Errorf("presigned HEAD: %d %v", r.status, r.header)
			}
			// The presigned GET URL cannot be used for another method or key.
			if r := e.fetch(t, http.MethodDelete, get.URL, nil, nil); r.status != 403 || r.code() != "SignatureDoesNotMatch" {
				t.Errorf("presigned GET used for DELETE: %d %s", r.status, r.code())
			}
			tampered := strings.Replace(get.URL, url.PathEscape("ü"), "u", 1)
			if r := e.fetch(t, http.MethodGet, tampered, nil, nil); r.status != 403 {
				t.Errorf("tampered presigned URL: %d", r.status)
			}
			del, err := ps.PresignDeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
			must(t, err, "PresignDeleteObject")
			if r := e.fetch(t, http.MethodDelete, del.URL, nil, nil); r.status != 204 {
				t.Errorf("presigned DELETE: %d %s", r.status, r.body)
			}
			if r := e.fetch(t, http.MethodGet, get.URL, nil, nil); r.status != 404 || r.code() != "NoSuchKey" {
				t.Errorf("presigned GET after delete: %d %s", r.status, r.code())
			}
		})
	}
}

func TestPresignedErrors(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	e.put(t, b, "obj", []byte("x"))
	u := e.base + "/" + b + "/obj"

	// Expired.
	signed, err := auth.PresignURL(http.MethodGet, u, rootCreds, region, time.Minute, time.Now().Add(-2*time.Minute))
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, nil); r.status != 403 || r.code() != "AccessDenied" || !strings.Contains(string(r.body), "expired") {
		t.Errorf("expired URL: %d %s", r.status, r.body)
	}
	// Not yet valid (beyond the clock skew).
	signed, err = auth.PresignURL(http.MethodGet, u, rootCreds, region, time.Hour, time.Now().Add(30*time.Minute))
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, nil); r.status != 403 {
		t.Errorf("future URL: %d %s", r.status, r.body)
	}
	// Expires over 7 days.
	signed, err = auth.PresignURL(http.MethodGet, u, rootCreds, region, time.Hour, time.Now())
	must(t, err, "PresignURL")
	over := strings.Replace(signed, "X-Amz-Expires=3600", "X-Amz-Expires=604801", 1)
	if r := e.fetch(t, http.MethodGet, over, nil, nil); r.status != 400 || r.code() != "AuthorizationQueryParametersError" {
		t.Errorf("X-Amz-Expires > 7d: %d %s", r.status, r.code())
	}
	// Wrong secret.
	signed, err = auth.PresignURL(http.MethodGet, u, auth.Credentials{AccessKey: rootAccess, SecretKey: "not-the-secret"}, region, time.Hour, time.Now())
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, nil); r.status != 403 || r.code() != "SignatureDoesNotMatch" {
		t.Errorf("wrong secret: %d %s", r.status, r.code())
	}
	// Unknown key.
	signed, err = auth.PresignURL(http.MethodGet, u, auth.Credentials{AccessKey: "NOSUCHKEY", SecretKey: "whatever-secret"}, region, time.Hour, time.Now())
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, nil); r.status != 403 || r.code() != "InvalidAccessKeyId" {
		t.Errorf("unknown key: %d %s", r.status, r.code())
	}
	// A valid signature of a user without permission.
	user := e.createUser(t, userName("presign-wo"), "writeonly")
	signed, err = auth.PresignURL(http.MethodGet, u, user, region, time.Hour, time.Now())
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, nil); r.status != 403 || r.code() != "AccessDenied" {
		t.Errorf("presigned without permission: %d %s", r.status, r.code())
	}
	// Header and query auth together.
	signed, err = auth.PresignURL(http.MethodGet, u, rootCreds, region, time.Hour, time.Now())
	must(t, err, "PresignURL")
	if r := e.fetch(t, http.MethodGet, signed, nil, http.Header{"Authorization": {"AWS4-HMAC-SHA256 Credential=x"}}); r.status != 400 {
		t.Errorf("header + query auth: %d %s", r.status, r.code())
	}
}

func TestPresignedVirtualHost(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	e.put(t, b, "vh/obj", []byte("virtual"))
	ps := s3.NewPresignClient(e.vhost(rootCreds))
	get, err := ps.PresignGetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("vh/obj")})
	must(t, err, "PresignGetObject vhost")
	if !strings.Contains(get.URL, "://"+b+"."+vhostZone+":") {
		t.Fatalf("not a virtual-host URL: %s", get.URL)
	}
	if r := e.fetch(t, http.MethodGet, get.URL, nil, nil); r.status != 200 || string(r.body) != "virtual" {
		t.Errorf("vhost presigned GET: %d %s", r.status, r.body)
	}
}

func TestPresignedMinio(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			mc := e.minioClient(t, rootCreds, false)
			b := e.newBucket(t)
			key := "minio presign/ö"
			pu, err := mc.PresignedPutObject(ctx(t), b, key, 5*time.Minute)
			must(t, err, "PresignedPutObject")
			if r := e.fetch(t, http.MethodPut, pu.String(), []byte("minio presigned"), nil); r.status != 200 {
				t.Fatalf("minio presigned PUT: %d %s", r.status, r.body)
			}
			params := url.Values{"response-content-type": {"application/x-minio"}}
			pg, err := mc.PresignedGetObject(ctx(t), b, key, 5*time.Minute, params)
			must(t, err, "PresignedGetObject")
			r := e.fetch(t, http.MethodGet, pg.String(), nil, nil)
			if r.status != 200 || string(r.body) != "minio presigned" || r.header.Get("Content-Type") != "application/x-minio" {
				t.Errorf("minio presigned GET: %d %q %v", r.status, r.body, r.header.Get("Content-Type"))
			}
			ph, err := mc.PresignedHeadObject(ctx(t), b, key, 5*time.Minute, nil)
			must(t, err, "PresignedHeadObject")
			if r := e.fetch(t, http.MethodHead, ph.String(), nil, nil); r.status != 200 {
				t.Errorf("minio presigned HEAD: %d", r.status)
			}
		})
	}
}

// postForm builds a multipart/form-data body: fields in order, then the
// file part.
func postForm(t *testing.T, fields [][2]string, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		must(t, w.WriteField(f[0], f[1]), "WriteField")
	}
	fw, err := w.CreateFormFile("file", filename)
	must(t, err, "CreateFormFile")
	_, _ = fw.Write(content)
	must(t, w.Close(), "multipart close")
	return &buf, w.FormDataContentType()
}

func (e *env) post(t *testing.T, rawURL string, body *bytes.Buffer, contentType string) response {
	t.Helper()
	return e.fetch(t, http.MethodPost, rawURL, body.Bytes(), http.Header{"Content-Type": {contentType}})
}

func TestPostPolicyMinio(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			mc := e.minioClient(t, rootCreds, false)
			b := e.newBucket(t)
			p := minio.NewPostPolicy()
			must(t, p.SetBucket(b), "SetBucket")
			must(t, p.SetKeyStartsWith("uploads/"), "SetKeyStartsWith")
			must(t, p.SetExpires(time.Now().UTC().Add(10*time.Minute)), "SetExpires")
			must(t, p.SetContentType("image/png"), "SetContentType")
			must(t, p.SetContentLengthRange(1, 1024), "SetContentLengthRange")
			must(t, p.SetUserMetadata("origin", "browser"), "SetUserMetadata")
			must(t, p.SetSuccessStatusAction("201"), "SetSuccessStatusAction")
			u, form, err := mc.PresignedPostPolicy(ctx(t), p)
			must(t, err, "PresignedPostPolicy")

			send := func(key string, content []byte, mutate func(map[string]string)) response {
				f := map[string]string{}
				for k, v := range form {
					f[k] = v
				}
				f["key"] = key
				if mutate != nil {
					mutate(f)
				}
				var fields [][2]string
				for k, v := range f {
					fields = append(fields, [2]string{k, v})
				}
				body, ct := postForm(t, fields, "photo.png", content)
				return e.post(t, u.String(), body, ct)
			}
			png := []byte("\x89PNG fake image data")
			r := send("uploads/${filename}", png, nil)
			if r.status != http.StatusCreated {
				t.Fatalf("POST upload: %d %s", r.status, r.body)
			}
			var pr struct {
				Location, Bucket, Key, ETag string
			}
			must(t, xml.Unmarshal(r.body, &pr), "decode PostResponse")
			if pr.Bucket != b || pr.Key != "uploads/photo.png" || pr.ETag != quoted(md5Hex(png)) || !strings.Contains(pr.Location, b) {
				t.Errorf("PostResponse %+v", pr)
			}
			h, err := e.root().HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("uploads/photo.png")})
			must(t, err, "HeadObject")
			if aws.ToString(h.ContentType) != "image/png" || h.Metadata["origin"] != "browser" || aws.ToInt64(h.ContentLength) != int64(len(png)) {
				t.Errorf("uploaded object: %q %v %d", aws.ToString(h.ContentType), h.Metadata, aws.ToInt64(h.ContentLength))
			}

			// Policy violations.
			if r := send("elsewhere/x", png, nil); r.status != 403 || r.code() != "AccessDenied" {
				t.Errorf("key outside starts-with: %d %s", r.status, r.body)
			}
			if r := send("uploads/big", bytes.Repeat([]byte("x"), 2000), nil); r.status != 400 || r.code() != "EntityTooLarge" {
				t.Errorf("too large: %d %s", r.status, r.body)
			}
			if r := send("uploads/x", png, func(f map[string]string) { f["Content-Type"] = "text/html" }); r.status != 403 {
				t.Errorf("content type mismatch: %d %s", r.status, r.body)
			}
			if r := send("uploads/x", png, func(f map[string]string) { f["x-amz-signature"] = strings.Repeat("0", 64) }); r.status != 403 || r.code() != "SignatureDoesNotMatch" {
				t.Errorf("bad signature: %d %s", r.status, r.body)
			}
			if r := send("uploads/x", png, func(f map[string]string) { f["x-amz-meta-extra"] = "nope" }); r.status != 403 {
				t.Errorf("extra field not in policy: %d %s", r.status, r.body)
			}
		})
	}
}

// TestPostPolicyRaw builds the POST policy with gos3's own signer and
// checks the success_action_status/redirect variants and anonymous POSTs
// allowed by a bucket policy.
func TestPostPolicyRaw(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	u := e.base + "/" + b
	now := time.Now().UTC()
	pf, err := auth.SignPostPolicy(rootCreds, region, now, now.Add(10*time.Minute), []any{
		map[string]string{"bucket": b},
		[]any{"starts-with", "$key", ""},
		[]any{"starts-with", "$success_action_redirect", ""},
		[]any{"eq", "$success_action_status", "204"},
	})
	must(t, err, "SignPostPolicy")
	fields := func(extra ...[2]string) [][2]string {
		var f [][2]string
		for k, v := range pf {
			f = append(f, [2]string{k, v})
		}
		return append(f, extra...)
	}

	body, ct := postForm(t, fields([2]string{"key", "plain"}, [2]string{"success_action_status", "204"}), "f.txt", []byte("one"))
	if r := e.post(t, u, body, ct); r.status != 204 || r.header.Get("ETag") != quoted(md5Hex([]byte("one"))) || r.header.Get("Location") == "" {
		t.Errorf("204 POST: %d %v %s", r.status, r.header, r.body)
	}
	body, ct = postForm(t, fields([2]string{"key", "redirected"}, [2]string{"success_action_redirect", "https://app.example.com/done?x=1"},
		[2]string{"success_action_status", "204"}), "f.txt", []byte("two"))
	r := e.post(t, u, body, ct)
	loc, _ := url.Parse(r.header.Get("Location"))
	if r.status != http.StatusSeeOther || loc == nil || loc.Host != "app.example.com" || loc.Query().Get("bucket") != b ||
		loc.Query().Get("key") != "redirected" || loc.Query().Get("etag") != quoted(md5Hex([]byte("two"))) || loc.Query().Get("x") != "1" {
		t.Errorf("redirect POST: %d %q", r.status, r.header.Get("Location"))
	}
	if got := string(e.get(t, b, "redirected")); got != "two" {
		t.Errorf("redirected object %q", got)
	}

	// Anonymous POST without policy: denied unless a bucket policy allows it.
	body, ct = postForm(t, [][2]string{{"key", "anon"}}, "f.txt", []byte("anon"))
	if r := e.post(t, u, body, ct); r.status != 403 {
		t.Errorf("anonymous POST: %d %s", r.status, r.body)
	}
	_, err = e.root().PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::%s/drop/*"}]}`, b))})
	must(t, err, "PutBucketPolicy")
	body, ct = postForm(t, [][2]string{{"key", "drop/anon"}}, "f.txt", []byte("anon"))
	if r := e.post(t, u, body, ct); r.status != 204 {
		t.Errorf("anonymous POST allowed by policy: %d %s", r.status, r.body)
	}
	body, ct = postForm(t, [][2]string{{"key", "other/anon"}}, "f.txt", []byte("anon"))
	if r := e.post(t, u, body, ct); r.status != 403 {
		t.Errorf("anonymous POST outside policy: %d %s", r.status, r.body)
	}
}

package e2e

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// TestPresignedQueryHeaders: x-amz-* request headers that a presigner
// (such as the AWS SDK for JavaScript v3) moved into the signed query
// string act as headers: a presigned CopyObject copies, metadata is kept.
func TestPresignedQueryHeaders(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			b := e.newBucket(t)
			e.put(t, b, "src", []byte("precious"))
			e.put(t, b, "dst", []byte("old"))
			presign := func(method, key string, q url.Values) string {
				u, err := auth.PresignURL(method, e.base+"/"+b+"/"+key+"?"+q.Encode(), rootCreds, region, time.Minute, time.Now())
				must(t, err, "PresignURL")
				return u
			}
			r := e.fetch(t, http.MethodPut, presign(http.MethodPut, "dst", url.Values{"x-amz-copy-source": {b + "/src"}, "x-id": {"CopyObject"}}), nil, nil)
			if r.status != 200 {
				t.Fatalf("presigned copy: %d %s", r.status, r.body)
			}
			if got := e.get(t, b, "dst"); string(got) != "precious" {
				t.Fatalf("copy destination: %q", got)
			}
			r = e.fetch(t, http.MethodPut, presign(http.MethodPut, "meta", url.Values{"x-amz-meta-foo": {"bar"}, "x-amz-storage-class": {"STANDARD_IA"},
				"x-amz-checksum-crc32": {"AAAAAA=="}}), []byte("data"), nil)
			if r.status != 200 {
				t.Fatalf("presigned put: %d %s", r.status, r.body)
			}
			out, err := e.root().HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("meta")})
			must(t, err, "HeadObject")
			if out.Metadata["foo"] != "bar" || out.StorageClass != "STANDARD_IA" {
				t.Fatalf("presigned put metadata: %v %q", out.Metadata, out.StorageClass)
			}
		})
	}
}

// TestGlobalCORSAnonymous: the default global CORS rule ("*") answers
// signed requests only; unsigned responses carry no
// Access-Control-Allow-Origin and unsigned preflights are refused.
func TestGlobalCORSAnonymous(t *testing.T) {
	t.Parallel()
	e := memEnv
	b := e.newBucket(t)
	e.put(t, b, "k", []byte("x"))
	_, err := e.root().PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::` + b + `/*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`)})
	must(t, err, "PutBucketPolicy")
	origin := map[string]string{"Origin": "https://evil.example"}
	if r := e.anon(t, http.MethodGet, "/"+b+"/k", nil, origin); r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("anonymous GET: %d ACAO %q", r.status, r.header.Get("Access-Control-Allow-Origin"))
	}
	if r := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/k", nil, origin); r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("signed GET: %d ACAO %q", r.status, r.header.Get("Access-Control-Allow-Origin"))
	}
	pre := func(acrh string) response {
		h := map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "PUT"}
		if acrh != "" {
			h["Access-Control-Request-Headers"] = acrh
		}
		return e.anon(t, http.MethodOptions, "/"+b+"/k", nil, h)
	}
	if r := pre(""); r.status != 403 || r.code() != "AccessForbidden" {
		t.Errorf("unsigned preflight: %d %s", r.status, r.code())
	}
	if r := pre("authorization,x-amz-date,x-amz-content-sha256"); r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("signed preflight: %d %v", r.status, r.header)
	}
}

// TestObjectCommaContentTypeSandboxed: a Content-Type list whose last
// entry browsers would render as HTML is sandboxed even if it starts with
// application/pdf.
func TestObjectCommaContentTypeSandboxed(t *testing.T) {
	t.Parallel()
	e := memEnv
	b := e.newBucket(t)
	e.put(t, b, "x", []byte("<script>alert(document.domain)</script>"), func(in *s3.PutObjectInput) {
		in.ContentType = aws.String("application/pdf; x=y, text/html")
	})
	for _, path := range []string{"/" + b + "/x", "/" + b + "/x?response-content-type=" + url.QueryEscape("application/pdf,text/html")} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			if r := e.signed(t, rootCreds, m, path, nil, nil); r.status != 200 || r.header.Get("Content-Security-Policy") != "sandbox" {
				t.Errorf("%s %s: %d CSP %q", m, path, r.status, r.header.Get("Content-Security-Policy"))
			}
		}
	}
}

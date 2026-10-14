package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- helpers (prefixed bc: s3-bucketconfig tests) ---

// bcCode returns the S3 error code of an SDK error ("" for nil).
func bcCode(err error) string {
	if err == nil {
		return ""
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return "non-API error: " + err.Error()
}

// bcSDKErr fails unless err is an API error with code.
func bcSDKErr(t *testing.T, err error, code string) {
	t.Helper()
	if got := bcCode(err); got != code {
		t.Fatalf("error code = %q, want %q (err=%v)", got, code, err)
	}
}

// bcExpect checks a raw response's status and error code ("" = none).
func bcExpect(t *testing.T, resp *testResponse, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || resp.code() != code {
		t.Fatalf("got %d %q, want %d %q; body: %s", resp.StatusCode, resp.code(), status, code, resp.Data)
	}
}

// bcNS fails unless an XML response carries the declaration and the S3
// namespace on its root element.
func bcNS(t *testing.T, resp *testResponse, root string) {
	t.Helper()
	body := string(resp.Data)
	if !strings.HasPrefix(body, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Errorf("missing XML declaration: %s", body)
	}
	if !strings.Contains(body, "<"+root+` xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Errorf("root %s without S3 namespace: %s", root, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/xml" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// bcHdr builds a header from key/value pairs.
func bcHdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// bcMD5 returns the base64 Content-MD5 of b.
func bcMD5(b []byte) string {
	s := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

// bcEnv returns a test env with bucket "cfg" created.
func bcEnv(t *testing.T, opts ...testEnvOption) *testEnv {
	t.Helper()
	e := newTestEnv(t, opts...)
	e.createBucket("cfg")
	return e
}

// bcVersioned enables versioning on bucket directly in the backend.
func bcVersioned(t *testing.T, e *testEnv, bucket string) {
	t.Helper()
	if err := e.backend.SetBucketVersioning(context.Background(), bucket, storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
}

var bcCtx = context.Background()

// --- Generic behaviour ---

// TestBucketConfigNoSuchBucket checks that every configuration operation
// answers NoSuchBucket (with BucketName) for a missing bucket.
func TestBucketConfigNoSuchBucket(t *testing.T) {
	e := newTestEnv(t)
	subs := []string{"versioning", "tagging", "cors", "policy", "policyStatus", "lifecycle", "encryption", "acl",
		"object-lock", "ownershipControls", "website", "notification", "logging", "publicAccessBlock",
		"replication", "accelerate", "requestPayment"}
	for _, sub := range subs {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			resp := e.signed(method, "/nobucket?"+sub, []byte("<x/>"), nil)
			if resp.StatusCode == http.StatusMethodNotAllowed {
				continue
			}
			if resp.StatusCode != http.StatusNotFound || resp.code() != "NoSuchBucket" {
				t.Errorf("%s ?%s: %d %s", method, sub, resp.StatusCode, resp.Data)
				continue
			}
			if !bytes.Contains(resp.Data, []byte("<BucketName>nobucket</BucketName>")) {
				t.Errorf("%s ?%s: no BucketName: %s", method, sub, resp.Data)
			}
		}
	}
}

// TestBucketConfigBodies checks body handling shared by all PUTs: empty
// bodies, malformed XML, wrong root elements, Content-MD5 validation and
// size limits.
func TestBucketConfigBodies(t *testing.T) {
	e := bcEnv(t)
	tagging := []byte(`<Tagging><TagSet><Tag><Key>a</Key><Value>b</Value></Tag></TagSet></Tagging>`)
	for _, tc := range []struct {
		name   string
		path   string
		body   []byte
		hdr    http.Header
		status int
		code   string
	}{
		{"empty", "/cfg?tagging", nil, nil, 400, "MissingRequestBodyError"},
		{"malformed", "/cfg?tagging", []byte("<Tagging><TagSet>"), nil, 400, "MalformedXML"},
		{"wrong root", "/cfg?tagging", []byte("<Other/>"), nil, 400, "MalformedXML"},
		{"not xml", "/cfg?versioning", []byte("hello"), nil, 400, "MalformedXML"},
		{"bad md5", "/cfg?tagging", tagging, bcHdr("Content-MD5", bcMD5([]byte("other"))), 400, "BadDigest"},
		{"invalid md5", "/cfg?tagging", tagging, bcHdr("Content-MD5", "not-base64!"), 400, "InvalidDigest"},
		{"good md5", "/cfg?tagging", tagging, bcHdr("Content-MD5", bcMD5(tagging)), 204, ""},
		{"bad crc32", "/cfg?tagging", tagging, bcHdr("x-amz-checksum-crc32", "AAAAAA=="), 400, "BadDigest"},
		{"too large", "/cfg?tagging", bytes.Repeat([]byte(" "), maxBucketConfigBody+1), nil, 400, "MaxMessageLengthExceeded"},
		{"policy too large", "/cfg?policy", append([]byte(`{"Statement":[]`), bytes.Repeat([]byte(" "), 20<<10)...), nil, 400, "MalformedPolicy"},
		{"lifecycle empty", "/cfg?lifecycle", nil, nil, 400, "MissingRequestBodyError"},
		{"cors empty", "/cfg?cors", nil, nil, 400, "MissingRequestBodyError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, tc.path, tc.body, tc.hdr), tc.status, tc.code)
		})
	}
}

// TestBucketConfigAuthorization checks that the configuration operations
// are authorized with their IAM actions (core) and that anonymous callers
// are denied.
func TestBucketConfigAuthorization(t *testing.T) {
	e := bcEnv(t)
	requireAuthz(t, e)
	ro := e.clientFor(e.createUser("reader", "readonly"))
	if _, err := ro.GetBucketVersioning(bcCtx, &awss3.GetBucketVersioningInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatalf("readonly GetBucketVersioning: %v", err)
	}
	_, err := ro.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}})
	bcSDKErr(t, err, "AccessDenied")
	bcExpect(t, e.anon(http.MethodGet, "/cfg?versioning", nil, nil), 403, "AccessDenied")
	bcExpect(t, e.anon(http.MethodPut, "/cfg?policy", []byte(`{}`), nil), 403, "AccessDenied")
}

// TestBucketConfigTLSTrailers runs PUTs through the SDK over TLS, where it
// sends checksums as aws-chunked trailers.
func TestBucketConfigTLSTrailers(t *testing.T) {
	e := bcEnv(t, withTLS())
	if _, err := e.client.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.PutBucketCors(bcCtx, &awss3.PutBucketCorsInput{Bucket: aws.String("cfg"),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"}}}}}); err != nil {
		t.Fatal(err)
	}
	out, err := e.client.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
	if err != nil || len(out.TagSet) != 1 || *out.TagSet[0].Key != "k" {
		t.Fatalf("GetBucketTagging: %+v %v", out, err)
	}
}

// --- Versioning ---

func TestBucketVersioningSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	get := func() types.BucketVersioningStatus {
		t.Helper()
		out, err := c.GetBucketVersioning(bcCtx, &awss3.GetBucketVersioningInput{Bucket: aws.String("cfg")})
		if err != nil {
			t.Fatal(err)
		}
		if out.MFADelete != "" {
			t.Fatalf("MFADelete reported: %q", out.MFADelete)
		}
		return out.Status
	}
	if st := get(); st != "" {
		t.Fatalf("new bucket status %q", st)
	}
	resp := e.signed(http.MethodGet, "/cfg?versioning", nil, nil)
	bcExpect(t, resp, 200, "")
	bcNS(t, resp, "VersioningConfiguration")
	if bytes.Contains(resp.Data, []byte("Status")) || bytes.Contains(resp.Data, []byte("MfaDelete")) {
		t.Fatalf("unversioned bucket body: %s", resp.Data)
	}
	for _, st := range []types.BucketVersioningStatus{types.BucketVersioningStatusSuspended, types.BucketVersioningStatusEnabled, types.BucketVersioningStatusEnabled, types.BucketVersioningStatusSuspended} {
		if _, err := c.PutBucketVersioning(bcCtx, &awss3.PutBucketVersioningInput{Bucket: aws.String("cfg"),
			VersioningConfiguration: &types.VersioningConfiguration{Status: st, MFADelete: types.MFADeleteDisabled}}); err != nil {
			t.Fatalf("put %s: %v", st, err)
		}
		if got := get(); got != st {
			t.Fatalf("status %q, want %q", got, st)
		}
	}
	bi, _ := e.backend.GetBucket(bcCtx, "cfg")
	if bi.Versioning != storage.VersioningSuspended {
		t.Fatalf("backend status %q", bi.Versioning)
	}
}

func TestBucketVersioningErrors(t *testing.T) {
	e := bcEnv(t)
	if err := e.backend.CreateBucket(bcCtx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	e.createBucket("replicated")
	bcVersioned(t, e, "replicated")
	e.putBucketConfig("replicated", storage.ConfigReplication, "<ReplicationConfiguration/>")
	v := func(inner string) []byte {
		return []byte("<VersioningConfiguration>" + inner + "</VersioningConfiguration>")
	}
	for _, tc := range []struct {
		name, bucket string
		body         []byte
		status       int
		code         string
	}{
		{"no status", "cfg", v(""), 400, "IllegalVersioningConfigurationException"},
		{"bad status", "cfg", v("<Status>On</Status>"), 400, "MalformedXML"},
		{"lowercase", "cfg", v("<Status>enabled</Status>"), 400, "MalformedXML"},
		{"mfa enabled", "cfg", v("<Status>Enabled</Status><MfaDelete>Enabled</MfaDelete>"), 501, "NotImplemented"},
		{"mfa bad", "cfg", v("<Status>Enabled</Status><MfaDelete>Yes</MfaDelete>"), 400, "MalformedXML"},
		{"suspend locked", "locked", v("<Status>Suspended</Status>"), 409, "InvalidBucketState"},
		{"enable locked", "locked", v("<Status>Enabled</Status>"), 200, ""},
		{"suspend replicated", "replicated", v("<Status>Suspended</Status>"), 409, "InvalidBucketState"},
		{"namespaced", "cfg", []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/"+tc.bucket+"?versioning", tc.body, nil), tc.status, tc.code)
		})
	}
	if resp := e.signed(http.MethodDelete, "/cfg?versioning", nil, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE ?versioning: %d", resp.StatusCode)
	}
}

// --- Bucket tagging ---

func TestBucketTaggingSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchTagSet")
	tags := []types.Tag{{Key: aws.String("env"), Value: aws.String("prod")}, {Key: aws.String("team"), Value: aws.String("")}, {Key: aws.String("ü key/x"), Value: aws.String("v=1+2@a:b")}}
	if _, err := c.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"), Tagging: &types.Tagging{TagSet: tags}}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.TagSet) != len(tags) {
		t.Fatalf("got %d tags", len(out.TagSet))
	}
	for i := range tags {
		if *out.TagSet[i].Key != *tags[i].Key || *out.TagSet[i].Value != *tags[i].Value {
			t.Fatalf("tag %d = %s=%s", i, *out.TagSet[i].Key, *out.TagSet[i].Value)
		}
	}
	resp := e.signed(http.MethodGet, "/cfg?tagging", nil, nil)
	bcNS(t, resp, "Tagging")

	// Replace, then delete.
	if _, err := c.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("only"), Value: aws.String("one")}}}}); err != nil {
		t.Fatal(err)
	}
	if out, _ := c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")}); len(out.TagSet) != 1 {
		t.Fatalf("after replace: %+v", out.TagSet)
	}
	bcExpect(t, e.signed(http.MethodDelete, "/cfg?tagging", nil, nil), 204, "")
	_, err = c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchTagSet")
	// Deleting again is fine.
	if _, err := c.DeleteBucketTagging(bcCtx, &awss3.DeleteBucketTaggingInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
}

func TestBucketTaggingValidation(t *testing.T) {
	e := bcEnv(t)
	tagsXML := func(pairs ...string) []byte {
		var b strings.Builder
		b.WriteString("<Tagging><TagSet>")
		for i := 0; i+1 < len(pairs); i += 2 {
			fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", pairs[i], pairs[i+1])
		}
		b.WriteString("</TagSet></Tagging>")
		return []byte(b.String())
	}
	var fifty, fiftyOne []string
	for i := 0; i < 51; i++ {
		if i < 50 {
			fifty = append(fifty, fmt.Sprintf("k%d", i), "v")
		}
		fiftyOne = append(fiftyOne, fmt.Sprintf("k%d", i), "v")
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"fifty", tagsXML(fifty...), 204, ""},
		{"fifty-one", tagsXML(fiftyOne...), 400, "InvalidTag"},
		{"duplicate", tagsXML("a", "1", "a", "2"), 400, "InvalidTag"},
		{"aws prefix", tagsXML("aws:x", "1"), 400, "InvalidTag"},
		{"AWS prefix", tagsXML("AWS:x", "1"), 400, "InvalidTag"},
		{"empty key", tagsXML("", "1"), 400, "InvalidTag"},
		{"key 128", tagsXML(strings.Repeat("k", 128), "1"), 204, ""},
		{"key 129", tagsXML(strings.Repeat("k", 129), "1"), 400, "InvalidTag"},
		{"value 256", tagsXML("k", strings.Repeat("v", 256)), 204, ""},
		{"value 257", tagsXML("k", strings.Repeat("v", 257)), 400, "InvalidTag"},
		{"bad char", tagsXML("k", "a&amp;b"), 400, "InvalidTag"},
		{"empty set removes", tagsXML(), 204, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?tagging", tc.body, nil), tc.status, tc.code)
		})
	}
	bcExpect(t, e.signed(http.MethodGet, "/cfg?tagging", nil, nil), 404, "NoSuchTagSet")
}

// --- CORS ---

func TestBucketCorsSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketCors(bcCtx, &awss3.GetBucketCorsInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchCORSConfiguration")
	if r := preflight(e, "/cfg/obj", "https://app.example", "PUT", ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight before config: %d", r.StatusCode)
	}
	rules := []types.CORSRule{
		{ID: aws.String("web"), AllowedMethods: []string{"GET", "PUT"}, AllowedOrigins: []string{"https://*.example"},
			AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag", "x-amz-request-id"}, MaxAgeSeconds: aws.Int32(600)},
		{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"}},
	}
	if _, err := c.PutBucketCors(bcCtx, &awss3.PutBucketCorsInput{Bucket: aws.String("cfg"), CORSConfiguration: &types.CORSConfiguration{CORSRules: rules}}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketCors(bcCtx, &awss3.GetBucketCorsInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CORSRules) != 2 || aws.ToString(out.CORSRules[0].ID) != "web" || aws.ToInt32(out.CORSRules[0].MaxAgeSeconds) != 600 ||
		strings.Join(out.CORSRules[0].ExposeHeaders, ",") != "ETag,x-amz-request-id" || out.CORSRules[1].MaxAgeSeconds != nil {
		t.Fatalf("rules: %+v", out.CORSRules)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?cors", nil, nil), "CORSConfiguration")

	// The new configuration applies to preflights immediately.
	r := preflight(e, "/cfg/obj", "https://app.example", "PUT", "content-type")
	if r.StatusCode != 200 || r.Header.Get("Access-Control-Allow-Origin") != "https://app.example" || r.Header.Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight: %d %v", r.StatusCode, r.Header)
	}
	bcExpect(t, e.signed(http.MethodDelete, "/cfg?cors", nil, nil), 204, "")
	if r := preflight(e, "/cfg/obj", "https://app.example", "PUT", ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight after delete: %d", r.StatusCode)
	}
	_, err = c.GetBucketCors(bcCtx, &awss3.GetBucketCorsInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchCORSConfiguration")
}

func TestBucketCorsValidation(t *testing.T) {
	e := bcEnv(t)
	rule := func(inner string) string { return "<CORSRule>" + inner + "</CORSRule>" }
	doc := func(rules ...string) []byte {
		return []byte("<CORSConfiguration>" + strings.Join(rules, "") + "</CORSConfiguration>")
	}
	ok := rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin>")
	var many []string
	for i := 0; i < 101; i++ {
		many = append(many, ok)
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"ok", doc(ok), 200, ""},
		{"hundred", doc(many[:100]...), 200, ""},
		{"no rules", doc(), 400, "MalformedXML"},
		{"101 rules", doc(many...), 400, "MalformedXML"},
		{"bad method", doc(rule("<AllowedMethod>PATCH</AllowedMethod><AllowedOrigin>*</AllowedOrigin>")), 400, "InvalidRequest"},
		{"no method", doc(rule("<AllowedOrigin>*</AllowedOrigin>")), 400, "MalformedXML"},
		{"no origin", doc(rule("<AllowedMethod>GET</AllowedMethod>")), 400, "MalformedXML"},
		{"two origin wildcards", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://*.*.x</AllowedOrigin>")), 400, "InvalidRequest"},
		{"two header wildcards", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><AllowedHeader>x-*-*</AllowedHeader>")), 400, "InvalidRequest"},
		{"wildcard expose", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><ExposeHeader>x-*</ExposeHeader>")), 400, "InvalidRequest"},
		{"negative max age", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><MaxAgeSeconds>-1</MaxAgeSeconds>")), 400, "MalformedXML"},
		{"zero max age", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><MaxAgeSeconds>0</MaxAgeSeconds>")), 200, ""},
		{"non-numeric max age", doc(rule("<AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin><MaxAgeSeconds>x</MaxAgeSeconds>")), 400, "MalformedXML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?cors", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Policy ---

const bcPublicReadPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"pub","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::cfg/*"}]}`

func TestBucketPolicySDK(t *testing.T) {
	e := bcEnv(t)
	requireAuthz(t, e)
	c := e.client
	e.putObject("cfg", "obj", []byte("hello"), storage.ObjectMeta{})
	_, err := c.GetBucketPolicy(bcCtx, &awss3.GetBucketPolicyInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchBucketPolicy")
	_, err = c.GetBucketPolicyStatus(bcCtx, &awss3.GetBucketPolicyStatusInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchBucketPolicy")
	bcExpect(t, e.anon(http.MethodGet, "/cfg/obj", nil, nil), 403, "AccessDenied")

	if _, err := c.PutBucketPolicy(bcCtx, &awss3.PutBucketPolicyInput{Bucket: aws.String("cfg"), Policy: aws.String(bcPublicReadPolicy)}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketPolicy(bcCtx, &awss3.GetBucketPolicyInput{Bucket: aws.String("cfg")})
	if err != nil || aws.ToString(out.Policy) != bcPublicReadPolicy {
		t.Fatalf("GetBucketPolicy: %v %q", err, aws.ToString(out.Policy))
	}
	resp := e.signed(http.MethodGet, "/cfg?policy", nil, nil)
	if resp.Header.Get("Content-Type") != "application/json" || string(resp.Data) != bcPublicReadPolicy {
		t.Fatalf("raw policy: %v %s", resp.Header, resp.Data)
	}
	st, err := c.GetBucketPolicyStatus(bcCtx, &awss3.GetBucketPolicyStatusInput{Bucket: aws.String("cfg")})
	if err != nil || !aws.ToBool(st.PolicyStatus.IsPublic) {
		t.Fatalf("policy status: %v %+v", err, st)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?policyStatus", nil, nil), "PolicyStatus")
	// The new policy takes effect immediately.
	bcExpect(t, e.anon(http.MethodGet, "/cfg/obj", nil, nil), 200, "")

	if _, err := c.DeleteBucketPolicy(bcCtx, &awss3.DeleteBucketPolicyInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg/obj", nil, nil), 403, "AccessDenied")
	_, err = c.GetBucketPolicy(bcCtx, &awss3.GetBucketPolicyInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchBucketPolicy")
	bcExpect(t, e.signed(http.MethodDelete, "/cfg?policy", nil, nil), 204, "")
}

func TestBucketPolicyStatus(t *testing.T) {
	e := bcEnv(t)
	stmt := func(principal, cond string) string {
		s := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":` + principal +
			`,"Action":"s3:GetObject","Resource":"arn:aws:s3:::cfg/*"`
		if cond != "" {
			s += `,"Condition":` + cond
		}
		return s + "}]}"
	}
	for _, tc := range []struct {
		name   string
		policy string
		public bool
	}{
		{"star", stmt(`"*"`, ""), true},
		{"aws star", stmt(`{"AWS":"*"}`, ""), true},
		{"source ip", stmt(`"*"`, `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`), false},
		{"any ip", stmt(`"*"`, `{"IpAddress":{"aws:SourceIp":"0.0.0.0/0"}}`), true},
		{"source vpc", stmt(`"*"`, `{"StringEquals":{"aws:SourceVpc":"vpc-123"}}`), false},
		{"secure transport only", stmt(`"*"`, `{"Bool":{"aws:SecureTransport":"true"}}`), true},
		{"user", stmt(`{"AWS":"alice"}`, ""), false},
		{"deny all", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::cfg/*"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?policy", []byte(tc.policy), nil), 204, "")
			out, err := e.client.GetBucketPolicyStatus(bcCtx, &awss3.GetBucketPolicyStatusInput{Bucket: aws.String("cfg")})
			if err != nil {
				t.Fatal(err)
			}
			if got := aws.ToBool(out.PolicyStatus.IsPublic); got != tc.public {
				t.Fatalf("IsPublic = %v, want %v", got, tc.public)
			}
		})
	}
}

func TestBucketPolicyValidation(t *testing.T) {
	e := bcEnv(t)
	for _, tc := range []struct {
		name, policy string
		status       int
		code         string
	}{
		{"not json", "nope", 400, "MalformedPolicy"},
		{"no principal", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::cfg/*"}]}`, 400, "MalformedPolicy"},
		{"other bucket", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}]}`, 400, "MalformedPolicy"},
		{"bad action", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"ec2:Run","Resource":"arn:aws:s3:::cfg/*"}]}`, 400, "MalformedPolicy"},
		{"unsupported user ARN", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::123456789012:user/team/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::cfg/*"}]}`, 400, "MalformedPolicy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?policy", []byte(tc.policy), nil), tc.status, tc.code)
		})
	}
	// BlockPublicPolicy rejects public policies only.
	e.putBucketConfig("cfg", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><BlockPublicPolicy>true</BlockPublicPolicy></PublicAccessBlockConfiguration>`)
	bcExpect(t, e.signed(http.MethodPut, "/cfg?policy", []byte(bcPublicReadPolicy), nil), 403, "AccessDenied")
	restricted := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::cfg/*","Condition":{"IpAddress":{"aws:SourceIp":"192.0.2.0/24"}}}]}`
	bcExpect(t, e.signed(http.MethodPut, "/cfg?policy", []byte(restricted), nil), 204, "")
}

// --- Lifecycle ---

func TestBucketLifecycleSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketLifecycleConfiguration(bcCtx, &awss3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchLifecycleConfiguration")
	in := &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{
		{ID: aws.String("logs"), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("logs/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(30)}},
		{Status: types.ExpirationStatusDisabled,
			Filter:                         &types.LifecycleRuleFilter{Tag: &types.Tag{Key: aws.String("tmp"), Value: aws.String("1")}},
			NoncurrentVersionExpiration:    &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(7)},
			AbortIncompleteMultipartUpload: nil},
		{ID: aws.String("uploads"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(2)}},
	}}
	if _, err := c.PutBucketLifecycleConfiguration(bcCtx, &awss3.PutBucketLifecycleConfigurationInput{Bucket: aws.String("cfg"), LifecycleConfiguration: in}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketLifecycleConfiguration(bcCtx, &awss3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rules) != 3 {
		t.Fatalf("rules: %+v", out.Rules)
	}
	r0, r1 := out.Rules[0], out.Rules[1]
	if aws.ToString(r0.ID) != "logs" || aws.ToInt32(r0.Expiration.Days) != 30 || r0.Filter == nil || aws.ToString(r0.Filter.Prefix) != "logs/" {
		t.Fatalf("rule 0: %+v", r0)
	}
	if aws.ToString(r1.ID) == "" || r1.Status != types.ExpirationStatusDisabled || aws.ToInt32(r1.NoncurrentVersionExpiration.NoncurrentDays) != 7 {
		t.Fatalf("rule 1 (generated ID expected): %+v", r1)
	}
	if aws.ToInt32(out.Rules[2].AbortIncompleteMultipartUpload.DaysAfterInitiation) != 2 {
		t.Fatalf("rule 2: %+v", out.Rules[2])
	}
	// The generated ID is stable across reads.
	out2, _ := c.GetBucketLifecycleConfiguration(bcCtx, &awss3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("cfg")})
	if aws.ToString(out2.Rules[1].ID) != aws.ToString(r1.ID) {
		t.Fatalf("ID changed: %q vs %q", aws.ToString(out2.Rules[1].ID), aws.ToString(r1.ID))
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?lifecycle", nil, nil), "LifecycleConfiguration")

	if _, err := c.DeleteBucketLifecycle(bcCtx, &awss3.DeleteBucketLifecycleInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetBucketLifecycleConfiguration(bcCtx, &awss3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchLifecycleConfiguration")
}

func TestBucketLifecycleRaw(t *testing.T) {
	e := bcEnv(t)
	// Legacy form: top-level Prefix per rule.
	legacy := []byte(`<LifecycleConfiguration><Rule><ID>old</ID><Prefix>tmp/</Prefix><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	bcExpect(t, e.signed(http.MethodPut, "/cfg?lifecycle", legacy, bcHdr("Content-MD5", bcMD5(legacy))), 200, "")
	resp := e.signed(http.MethodGet, "/cfg?lifecycle", nil, nil)
	bcExpect(t, resp, 200, "")
	if !bytes.Contains(resp.Data, []byte("<Prefix>tmp/</Prefix>")) || !bytes.Contains(resp.Data, []byte("<ID>old</ID>")) {
		t.Fatalf("legacy round trip: %s", resp.Data)
	}
	stored, err := e.backend.GetBucketConfig(bcCtx, "cfg", storage.ConfigLifecycle)
	if err != nil || !bytes.Equal(stored, resp.Data) {
		t.Fatalf("stored document differs from response: %s vs %s (%v)", stored, resp.Data, err)
	}
	for _, tc := range []struct {
		name, body string
		code       string
	}{
		{"no filter", `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, "MalformedXML"},
		{"bad status", `<LifecycleConfiguration><Rule><Filter/><Status>On</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, "MalformedXML"},
		{"zero days", `<LifecycleConfiguration><Rule><Filter/><Status>Enabled</Status><Expiration><Days>0</Days></Expiration></Rule></LifecycleConfiguration>`, "InvalidArgument"},
		{"unknown element", `<LifecycleConfiguration><Rule><Filter/><Status>Enabled</Status><Bogus/></Rule></LifecycleConfiguration>`, "MalformedXML"},
		{"wrong root", `<Lifecycle/>`, "MalformedXML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?lifecycle", []byte(tc.body), nil), 400, tc.code)
		})
	}
	// A failed PUT leaves the previous configuration in place.
	if resp2 := e.signed(http.MethodGet, "/cfg?lifecycle", nil, nil); !bytes.Equal(resp2.Data, resp.Data) {
		t.Fatalf("configuration changed by failed PUT: %s", resp2.Data)
	}
	bcExpect(t, e.signed(http.MethodDelete, "/cfg?lifecycle", nil, nil), 204, "")
	bcExpect(t, e.signed(http.MethodGet, "/cfg?lifecycle", nil, nil), 404, "NoSuchLifecycleConfiguration")
}

// --- Encryption ---

func TestBucketEncryptionSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketEncryption(bcCtx, &awss3.GetBucketEncryptionInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "ServerSideEncryptionConfigurationNotFoundError")
	for _, tc := range []struct {
		algo   types.ServerSideEncryption
		key    string
		bkey   *bool
		expect bool
	}{
		{types.ServerSideEncryptionAes256, "", nil, false},
		{types.ServerSideEncryptionAwsKms, "arn:aws:kms:us-east-1:000000000000:key/abc", aws.Bool(true), true},
		{types.ServerSideEncryptionAwsKmsDsse, "", aws.Bool(false), false},
	} {
		if _, err := c.PutBucketEncryption(bcCtx, &awss3.PutBucketEncryptionInput{Bucket: aws.String("cfg"),
			ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: tc.algo, KMSMasterKeyID: aws.String(tc.key)},
				BucketKeyEnabled:                   tc.bkey,
			}}}}); err != nil {
			t.Fatalf("%s: %v", tc.algo, err)
		}
		out, err := c.GetBucketEncryption(bcCtx, &awss3.GetBucketEncryptionInput{Bucket: aws.String("cfg")})
		if err != nil {
			t.Fatal(err)
		}
		rules := out.ServerSideEncryptionConfiguration.Rules
		if len(rules) != 1 || rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm != tc.algo ||
			aws.ToString(rules[0].ApplyServerSideEncryptionByDefault.KMSMasterKeyID) != tc.key ||
			aws.ToBool(rules[0].BucketKeyEnabled) != tc.expect {
			t.Fatalf("%s: %+v", tc.algo, rules)
		}
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?encryption", nil, nil), "ServerSideEncryptionConfiguration")
	if _, err := c.DeleteBucketEncryption(bcCtx, &awss3.DeleteBucketEncryptionInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetBucketEncryption(bcCtx, &awss3.GetBucketEncryptionInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "ServerSideEncryptionConfigurationNotFoundError")
}

func TestBucketEncryptionValidation(t *testing.T) {
	e := bcEnv(t)
	doc := func(rules string) []byte {
		return []byte("<ServerSideEncryptionConfiguration>" + rules + "</ServerSideEncryptionConfiguration>")
	}
	rule := func(algo, key string) string {
		s := "<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>" + algo + "</SSEAlgorithm>"
		if key != "" {
			s += "<KMSMasterKeyID>" + key + "</KMSMasterKeyID>"
		}
		return s + "</ApplyServerSideEncryptionByDefault></Rule>"
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"aes", doc(rule("AES256", "")), 200, ""},
		{"kms no key", doc(rule("aws:kms", "")), 200, ""},
		{"aes with key", doc(rule("AES256", "k")), 400, "InvalidArgument"},
		{"bad algo", doc(rule("DES", "")), 400, "MalformedXML"},
		{"no rules", doc(""), 400, "MalformedXML"},
		{"two rules", doc(rule("AES256", "") + rule("AES256", "")), 400, "MalformedXML"},
		{"no default", doc("<Rule><BucketKeyEnabled>true</BucketKeyEnabled></Rule>"), 400, "MalformedXML"},
		{"bad bool", doc("<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>maybe</BucketKeyEnabled></Rule>"), 400, "MalformedXML"},
		{"long key", doc(rule("aws:kms", strings.Repeat("k", 2049))), 400, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?encryption", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Concurrency ---

// TestBucketConfigConcurrent hammers configuration operations from many
// goroutines (run with -race): versioning/object-lock transitions stay
// consistent and every request gets a well-formed answer.
func TestBucketConfigConcurrent(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				var err error
				switch (i + j) % 5 {
				case 0:
					st := types.BucketVersioningStatusEnabled
					if j%2 == 0 {
						st = types.BucketVersioningStatusSuspended
					}
					_, err = c.PutBucketVersioning(bcCtx, &awss3.PutBucketVersioningInput{Bucket: aws.String("cfg"),
						VersioningConfiguration: &types.VersioningConfiguration{Status: st}})
					if code := bcCode(err); code == "InvalidBucketState" {
						err = nil // object lock got enabled first
					}
				case 1:
					_, err = c.PutObjectLockConfiguration(bcCtx, &awss3.PutObjectLockConfigurationInput{Bucket: aws.String("cfg"),
						ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}})
					if code := bcCode(err); code == "InvalidBucketState" {
						err = nil // versioning not enabled at that moment
					}
				case 2:
					_, err = c.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"),
						Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String(fmt.Sprintf("k%d", i)), Value: aws.String("v")}}}})
				case 3:
					_, err = c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
					if bcCode(err) == "NoSuchTagSet" {
						err = nil
					}
				case 4:
					_, err = c.PutBucketPolicy(bcCtx, &awss3.PutBucketPolicyInput{Bucket: aws.String("cfg"), Policy: aws.String(bcPublicReadPolicy)})
				}
				if err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	bi, err := e.backend.GetBucket(bcCtx, "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if bi.ObjectLockEnabled && bi.Versioning != storage.VersioningEnabled {
		t.Fatalf("object lock enabled with versioning %q", bi.Versioning)
	}
}

// bcFailBackend makes the bucket-state mutations of a backend fail with
// err while it is set.
type bcFailBackend struct {
	storage.Backend
	mu  sync.Mutex
	err error
}

func (b *bcFailBackend) setErr(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

func (b *bcFailBackend) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *bcFailBackend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	if err := b.failure(); err != nil {
		return err
	}
	return b.Backend.PutBucketConfig(ctx, bucket, key, data)
}

func (b *bcFailBackend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	if err := b.failure(); err != nil {
		return err
	}
	return b.Backend.DeleteBucketConfig(ctx, bucket, key)
}

func (b *bcFailBackend) SetBucketVersioning(ctx context.Context, bucket string, st storage.VersioningStatus) error {
	if err := b.failure(); err != nil {
		return err
	}
	return b.Backend.SetBucketVersioning(ctx, bucket, st)
}

func (b *bcFailBackend) SetBucketObjectLock(ctx context.Context, bucket string, on bool) error {
	if err := b.failure(); err != nil {
		return err
	}
	return b.Backend.SetBucketObjectLock(ctx, bucket, on)
}

// TestBucketConfigBackendErrors checks that storage failures are reported
// (InternalError, or NoSuchBucket when the bucket vanished mid-request)
// and leave the previous state in place.
func TestBucketConfigBackendErrors(t *testing.T) {
	fb := &bcFailBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		fb.Backend = o.Backend
		o.Backend = fb
	}))
	e.createBucket("cfg")
	e.createBucket("ver")
	bcVersioned(t, e, "ver")
	puts := []struct{ path, body string }{
		{"/cfg?tagging", "<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>"},
		{"/cfg?versioning", "<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"},
		{"/ver?object-lock", "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"},
		{"/cfg?policy", bcPublicReadPolicy},
		{"/cfg?acl", "<AccessControlPolicy><AccessControlList/></AccessControlPolicy>"},
		{"/cfg?lifecycle", "<LifecycleConfiguration><Rule><Filter/><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>"},
		{"/cfg?notification", "<NotificationConfiguration/>"},
		{"/cfg?requestPayment", "<RequestPaymentConfiguration><Payer>BucketOwner</Payer></RequestPaymentConfiguration>"},
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{errors.New("disk on fire"), 500, "InternalError"},
		{storage.ErrBucketNotFound, 404, "NoSuchBucket"},
	} {
		fb.setErr(tc.err)
		for _, p := range puts {
			bcExpect(t, e.signed(http.MethodPut, p.path, []byte(p.body), nil), tc.status, tc.code)
		}
		bcExpect(t, e.signed(http.MethodDelete, "/cfg?cors", nil, nil), tc.status, tc.code)
	}
	fb.setErr(nil)
	if bi, _ := e.backend.GetBucket(bcCtx, "ver"); bi.ObjectLockEnabled {
		t.Fatal("object lock enabled despite failure")
	}
	bcExpect(t, e.signed(http.MethodGet, "/cfg?tagging", nil, nil), 404, "NoSuchTagSet")
	if !strings.Contains(e.logs.String(), "disk on fire") {
		t.Fatal("internal error not logged")
	}
}

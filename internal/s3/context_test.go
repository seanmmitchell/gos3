package s3

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestBucketInfoCache(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	rc := &reqContext{bucket: "bkt", query: url.Values{}}
	s := e.server
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil || bi.Name != "bkt" {
		t.Fatal(bi, err)
	}
	if v, err := s.isVersioned(ctx, rc); err != nil || v {
		t.Fatal(v, err)
	}
	if err := e.backend.SetBucketVersioning(ctx, "bkt", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.versioning(ctx, rc); st != storage.VersioningUnversioned {
		t.Fatal("cache not used")
	}
	rc.forgetBucket()
	if st, _ := s.versioning(ctx, rc); st != storage.VersioningEnabled {
		t.Fatal("cache not dropped")
	}
	if v, _ := s.isVersioned(ctx, rc); !v {
		t.Fatal("not versioned")
	}
	missing := &reqContext{bucket: "nope"}
	if _, err := s.bucketInfo(ctx, missing); codeOf(err) != "NoSuchBucket" {
		t.Fatal(err)
	}
	if _, err := s.isVersioned(ctx, missing); codeOf(err) != "NoSuchBucket" {
		t.Fatal(err)
	}
	if _, err := s.lookupBucket(ctx, ""); codeOf(err) != "NoSuchBucket" {
		t.Fatal(err)
	}
	if err := bucketError(storage.ErrBucketNotEmpty, "x"); codeOf(err) != "BucketNotEmpty" {
		t.Fatal(err)
	}
}

func TestReqContextAccessors(t *testing.T) {
	rc := &reqContext{query: url.Values{"versionId": {"v1"}}}
	if rc.contentLength() != -1 || len(rc.trailer()) != 0 || rc.versionID() != "v1" {
		t.Fatal("nil auth defaults")
	}
	rc.auth = &auth.Result{DecodedContentLength: 7, Trailer: func() http.Header { return nil }}
	if rc.contentLength() != 7 || rc.trailer() == nil {
		t.Fatal("auth values")
	}
	for id, want := range map[auth.Identity]string{
		auth.Anonymous:                    "",
		{IsRoot: true, User: "root"}:      "root",
		{User: "alice", AccessKey: "AK1"}: "alice",
	} {
		rc.identity = id
		if rc.userName() != want {
			t.Errorf("%+v => %q", id, rc.userName())
		}
	}
}

func TestResponseWriterFeatures(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.override("GetObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		rc.addLog(slog.String("custom", "yes"))
		w.WriteHeader(http.StatusEarlyHints) // informational: does not complete the response
		w.Header().Set("Content-Type", "text/plain")
		if err := http.NewResponseController(w).Flush(); err != nil { // via Unwrap
			return err
		}
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("streamed"))
		w.WriteHeader(500) // ignored after the response started
		return nil
	})
	resp := e.signed("GET", "/bkt/k", nil, nil)
	if resp.StatusCode != 200 || string(resp.Data) != "streamed" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	if !strings.Contains(e.logs.String(), `"custom":"yes"`) {
		t.Fatal("custom log attribute missing")
	}
}

func TestConditionContext(t *testing.T) {
	now := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	e := newTestEnv(t, withOptions(func(o *Options) { o.Now = func() time.Time { return now } }))
	r := httptest.NewRequest("PUT", "/b/k?prefix=p/&delimiter=/&max-keys=5", nil)
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("User-Agent", "ua/1")
	r.Header.Set("Referer", "https://ref")
	r.Header.Set("X-Amz-Acl", "public-read")
	r.Header.Set("X-Amz-Server-Side-Encryption", "AES256")
	r.Header.Set("X-Amz-Storage-Class", "GLACIER")
	r.Header.Set("X-Amz-Tagging", "a=1&b=2")
	r.Header.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
	rc := &reqContext{
		query:    r.URL.Query(),
		identity: auth.Identity{User: "alice", AccessKey: "AK"},
		auth:     &auth.Result{Method: auth.MethodPresignedV4, PayloadMode: auth.PayloadUnsigned},
		sourceIP: "192.0.2.1",
		secure:   true,
	}
	c := e.server.conditionContext(r, rc, authzTarget{VersionID: "v1", Conditions: map[string][]string{"x:extra": {"1"}}})
	for k, want := range map[string]string{
		"aws:CurrentTime": "2025-03-04T05:06:07Z", "aws:EpochTime": "1741064767", "aws:SecureTransport": "true",
		"aws:SourceIp": "192.0.2.1", "aws:UserAgent": "ua/1", "aws:Referer": "https://ref", "aws:username": "alice",
		"aws:PrincipalType": "User", "s3:authType": "REST-QUERY-STRING", "s3:signatureversion": "AWS4-HMAC-SHA256",
		"s3:x-amz-content-sha256": "UNSIGNED-PAYLOAD", "s3:prefix": "p/", "s3:delimiter": "/", "s3:max-keys": "5",
		"s3:VersionId": "v1", "s3:x-amz-acl": "public-read", "s3:x-amz-server-side-encryption": "AES256",
		"s3:x-amz-storage-class": "GLACIER", "s3:RequestObjectTag/a": "1", "s3:object-lock-mode": "GOVERNANCE", "x:extra": "1",
	} {
		if got := c[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", k, got, want)
		}
	}
	keys := c["s3:RequestObjectTagKeys"]
	if len(keys) != 2 {
		t.Errorf("tag keys %v", keys)
	}
	rc.identity, rc.auth = auth.Anonymous, &auth.Result{Method: auth.MethodAnonymous}
	c = e.server.conditionContext(httptest.NewRequest("GET", "/", nil), rc, authzTarget{})
	if !reflect.DeepEqual(c["aws:PrincipalType"], []string{"Anonymous"}) || c["s3:authType"] != nil || c["aws:username"] != nil || c["s3:signatureversion"] != nil {
		t.Fatalf("anonymous context %v", c)
	}
	rc.identity, rc.auth = auth.Identity{IsRoot: true, User: "root"}, &auth.Result{Method: auth.MethodHeaderV4}
	c = e.server.conditionContext(httptest.NewRequest("GET", "/", nil), rc, authzTarget{})
	if c["aws:PrincipalType"][0] != "Account" || c["s3:authType"][0] != "REST-HEADER" {
		t.Fatalf("root context %v", c)
	}
}

// TestConditionContextTruncatesClientValues: free-form client values are cut
// to iam.MaxRequestValueLen bytes (at a character boundary) before
// policies glob or substitute them (H-02, H-03).
func TestConditionContextTruncatesClientValues(t *testing.T) {
	e := newTestEnv(t)
	long := strings.Repeat("é", iam.MaxRequestValueLen) // 2 bytes per character
	r := httptest.NewRequest("GET", "/b?prefix="+url.QueryEscape(long)+"&delimiter="+url.QueryEscape(long), nil)
	r.Header.Set("User-Agent", long)
	r.Header.Set("Referer", long)
	rc := &reqContext{query: r.URL.Query(), identity: auth.Anonymous, auth: &auth.Result{Method: auth.MethodAnonymous}}
	c := e.server.conditionContext(r, rc, authzTarget{})
	want := long[:iam.MaxRequestValueLen]
	for _, k := range []string{"aws:UserAgent", "aws:Referer", "s3:prefix", "s3:delimiter"} {
		if got := c[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %d bytes, want the first %d", k, len(got[0]), len(want))
		}
	}
	if got := iam.TruncateRequestValue("a" + long); len(got) != iam.MaxRequestValueLen-1 || !utf8.ValidString(got) {
		t.Errorf("TruncateRequestValue split a character: %d bytes", len(got))
	}
}

func TestMiscHelpers(t *testing.T) {
	if objectReadAction("") != "s3:GetObject" || objectReadAction("v") != "s3:GetObjectVersion" {
		t.Fatal("objectReadAction")
	}
	if !isPublicCannedACL("authenticated-read") || isPublicCannedACL("private") {
		t.Fatal("isPublicCannedACL")
	}
	if _, _, err := (noCredentials{}).LookupCredentials("x"); codeOf(err) != "InvalidAccessKeyId" {
		t.Fatal(err)
	}
	if maybeURLEncode("a b", true) != "a+b" {
		t.Fatal("maybeURLEncode")
	}
	if _, err := parsePublicAccessBlock([]byte("<x")); err == nil {
		t.Fatal("bad PAB parsed")
	}
	if v, _ := parseChecksumType("composite"); v != ChecksumTypeComposite {
		t.Fatal(v)
	}
	if !plausibleBucketName("Legacy_Bucket") || plausibleBucketName(strings.Repeat("a", 256)) || plausibleBucketName("a\\b") || plausibleBucketName("\xff") {
		t.Fatal("plausibleBucketName")
	}
	r := &http.Request{Method: "GET", URL: &url.URL{}}
	if rawRequestPath(r) != "/" || errorResource(r) != "/" {
		t.Fatal("empty path")
	}
	if rawRequestPath(&http.Request{}) != "/" {
		t.Fatal("nil URL")
	}
	if _, err := readBody(errReader{}, 10); codeOf(err) != "RequestTimeout" {
		t.Fatal(err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestPayloadReaderAlgorithmAndSDKHeaders(t *testing.T) {
	p, err := newTestPayload(t, []byte("abc"), map[string]string{"x-amz-checksum-algorithm": "SHA1"}, 3, nil, payloadOptions{})
	if err != nil || p.Algorithm() != ChecksumSHA1 {
		t.Fatal(p, err)
	}
	if p.Checksums() != nil || p.ChecksumType() != "" {
		t.Fatal("checksums before EOF")
	}
}

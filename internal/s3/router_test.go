package s3

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

func TestParseTarget(t *testing.T) {
	domains := []string{"s3.local", "Example.COM:9000", ".deep.s3.local", "https://web.test:8443/path", ""}
	for _, tc := range []struct {
		name       string
		host, uri  string
		bucket     string
		key        string
		vhost      bool
		wantErr    string
		noRequestU bool // build the request without RequestURI
	}{
		{name: "root", host: "localhost:9000", uri: "/"},
		{name: "bucket", host: "localhost", uri: "/bkt", bucket: "bkt"},
		{name: "bucket slash", host: "localhost", uri: "/bkt/", bucket: "bkt"},
		{name: "key", host: "localhost", uri: "/bkt/a/b.txt", bucket: "bkt", key: "a/b.txt"},
		{name: "trailing slash key", host: "localhost", uri: "/bkt/dir/", bucket: "bkt", key: "dir/"},
		{name: "double slashes", host: "localhost", uri: "/bkt//a//b//", bucket: "bkt", key: "/a//b//"},
		{name: "dot segments kept", host: "localhost", uri: "/bkt/a/../b/./c", bucket: "bkt", key: "a/../b/./c"},
		{name: "escaped", host: "localhost", uri: "/bkt/sp%20ace%2Fslash+plus%25", bucket: "bkt", key: "sp ace/slash+plus%"},
		{name: "unicode", host: "localhost", uri: "/bkt/%E6%97%A5%E6%9C%AC", bucket: "bkt", key: "日本"},
		{name: "query ignored", host: "localhost", uri: "/bkt/k?versionId=1&x=%2F", bucket: "bkt", key: "k"},
		{name: "bad escape", host: "localhost", uri: "/bkt/%zz", wantErr: "InvalidURI"},
		{name: "empty bucket segment", host: "localhost", uri: "//key", wantErr: "InvalidBucketName"},
		{name: "slash in bucket", host: "localhost", uri: "/a%2Fb/k", wantErr: "InvalidBucketName"},
		{name: "control char bucket", host: "localhost", uri: "/a%00b", wantErr: "InvalidBucketName"},
		{name: "vhost", host: "photos.s3.local", uri: "/a/b", bucket: "photos", key: "a/b", vhost: true},
		{name: "vhost port + case", host: "Photos.S3.Local:9000", uri: "/k", bucket: "photos", key: "k", vhost: true},
		{name: "vhost dotted bucket", host: "my.bucket.s3.local", uri: "/", bucket: "my.bucket", vhost: true},
		{name: "vhost longest domain", host: "b.deep.s3.local", uri: "/x", bucket: "b", key: "x", vhost: true},
		{name: "vhost reserved path is a key", host: "b.s3.local", uri: "/_gos3/info", bucket: "b", key: "_gos3/info", vhost: true},
		{name: "vhost double slash key", host: "b.s3.local", uri: "//x", bucket: "b", key: "/x", vhost: true},
		{name: "vhost trailing dot", host: "b.example.com.", uri: "/x", bucket: "b", key: "x", vhost: true},
		{name: "domain itself is path style", host: "s3.local:9000", uri: "/b/k", bucket: "b", key: "k"},
		{name: "unrelated suffix", host: "xs3.local", uri: "/b/k", bucket: "b", key: "k"},
		{name: "vhost domain with scheme", host: "b.web.test", uri: "/k", bucket: "b", key: "k", vhost: true},
		{name: "ipv6 host", host: "[::1]:9000", uri: "/b/k", bucket: "b", key: "k"},
		{name: "no RequestURI", host: "localhost", uri: "/bkt/x%2Fy", bucket: "bkt", key: "x/y", noRequestU: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *http.Request
			if tc.noRequestU {
				r = httptest.NewRequest("GET", "http://placeholder"+tc.uri, nil)
				r.RequestURI = ""
			} else {
				r = httptest.NewRequest("GET", "/", nil)
				r.RequestURI = tc.uri // as received on the wire
			}
			r.Host = tc.host
			got, err := parseTarget(r, domains)
			if tc.wantErr != "" {
				if code := s3err.As(err); err == nil || code.Code != tc.wantErr {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Bucket != tc.bucket || got.Key != tc.key || got.VirtualHost != tc.vhost {
				t.Fatalf("got %+v, want bucket=%q key=%q vhost=%v", got, tc.bucket, tc.key, tc.vhost)
			}
		})
	}
}

func TestRawRequestPathPrefersRequestURI(t *testing.T) {
	r := httptest.NewRequest("GET", "/bkt/a%2Fb?x=1", nil)
	r.URL.Path = "/rewritten" // e.g. by http.StripPrefix
	r.URL.RawPath = ""
	if p := rawRequestPath(r); p != "/bkt/a%2Fb" {
		t.Fatalf("raw path %q", p)
	}
}

func TestDetectOp(t *testing.T) {
	type tc struct {
		method, target, query string
		hdr                   map[string]string
		want                  string // op name or "!Code"
	}
	cases := []tc{
		{"GET", "/", "", nil, "ListBuckets"},
		{"GET", "/", "x-id=ListBuckets", nil, "ListBuckets"},
		{"PUT", "/", "", nil, "!MethodNotAllowed"},
		{"HEAD", "/", "", nil, "!MethodNotAllowed"},

		{"PUT", "/b", "", nil, "CreateBucket"},
		{"DELETE", "/b", "", nil, "DeleteBucket"},
		{"HEAD", "/b", "", nil, "HeadBucket"},
		{"HEAD", "/b", "acl", nil, "HeadBucket"},
		{"GET", "/b", "", nil, "ListObjects"},
		{"GET", "/b", "list-type=2&prefix=a", nil, "ListObjectsV2"},
		{"GET", "/b", "list-type=1", nil, "ListObjects"},
		{"GET", "/b", "versions", nil, "ListObjectVersions"},
		{"GET", "/b", "uploads", nil, "ListMultipartUploads"},
		{"GET", "/b", "location", nil, "GetBucketLocation"},
		{"PUT", "/b", "location", nil, "!MethodNotAllowed"},
		{"POST", "/b", "delete", nil, "DeleteObjects"},
		{"POST", "/b", "", map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, "PostObject"},
		{"POST", "/b", "", map[string]string{"Content-Type": "text/plain"}, "!MethodNotAllowed"},
		{"PATCH", "/b", "", nil, "!MethodNotAllowed"},

		{"GET", "/b", "versioning", nil, "GetBucketVersioning"},
		{"PUT", "/b", "versioning", nil, "PutBucketVersioning"},
		{"DELETE", "/b", "versioning", nil, "!MethodNotAllowed"},
		{"GET", "/b", "tagging", nil, "GetBucketTagging"},
		{"PUT", "/b", "tagging", nil, "PutBucketTagging"},
		{"DELETE", "/b", "tagging", nil, "DeleteBucketTagging"},
		{"GET", "/b", "cors", nil, "GetBucketCors"},
		{"PUT", "/b", "cors", nil, "PutBucketCors"},
		{"DELETE", "/b", "cors", nil, "DeleteBucketCors"},
		{"GET", "/b", "policy", nil, "GetBucketPolicy"},
		{"PUT", "/b", "policy", nil, "PutBucketPolicy"},
		{"DELETE", "/b", "policy", nil, "DeleteBucketPolicy"},
		{"GET", "/b", "policyStatus", nil, "GetBucketPolicyStatus"},
		{"PUT", "/b", "policyStatus", nil, "!MethodNotAllowed"},
		{"GET", "/b", "lifecycle", nil, "GetBucketLifecycle"},
		{"PUT", "/b", "lifecycle", nil, "PutBucketLifecycle"},
		{"DELETE", "/b", "lifecycle", nil, "DeleteBucketLifecycle"},
		{"GET", "/b", "encryption", nil, "GetBucketEncryption"},
		{"PUT", "/b", "encryption", nil, "PutBucketEncryption"},
		{"DELETE", "/b", "encryption", nil, "DeleteBucketEncryption"},
		{"GET", "/b", "acl", nil, "GetBucketAcl"},
		{"PUT", "/b", "acl", nil, "PutBucketAcl"},
		{"GET", "/b", "object-lock", nil, "GetObjectLockConfiguration"},
		{"PUT", "/b", "object-lock", nil, "PutObjectLockConfiguration"},
		{"GET", "/b", "ownershipControls", nil, "GetBucketOwnershipControls"},
		{"PUT", "/b", "ownershipControls", nil, "PutBucketOwnershipControls"},
		{"DELETE", "/b", "ownershipControls", nil, "DeleteBucketOwnershipControls"},
		{"GET", "/b", "website", nil, "GetBucketWebsite"},
		{"PUT", "/b", "website", nil, "PutBucketWebsite"},
		{"DELETE", "/b", "website", nil, "DeleteBucketWebsite"},
		{"GET", "/b", "notification", nil, "GetBucketNotification"},
		{"PUT", "/b", "notification", nil, "PutBucketNotification"},
		{"GET", "/b", "logging", nil, "GetBucketLogging"},
		{"PUT", "/b", "logging", nil, "PutBucketLogging"},
		{"GET", "/b", "publicAccessBlock", nil, "GetPublicAccessBlock"},
		{"PUT", "/b", "publicAccessBlock", nil, "PutPublicAccessBlock"},
		{"DELETE", "/b", "publicAccessBlock", nil, "DeletePublicAccessBlock"},
		{"GET", "/b", "replication", nil, "GetBucketReplication"},
		{"PUT", "/b", "replication", nil, "PutBucketReplication"},
		{"DELETE", "/b", "replication", nil, "DeleteBucketReplication"},
		{"GET", "/b", "accelerate", nil, "GetBucketAccelerate"},
		{"PUT", "/b", "accelerate", nil, "PutBucketAccelerate"},
		{"GET", "/b", "requestPayment", nil, "GetBucketRequestPayment"},
		{"PUT", "/b", "requestPayment", nil, "PutBucketRequestPayment"},
		{"GET", "/b", "analytics", nil, "!NotImplemented"},
		{"GET", "/b", "inventory&id=1", nil, "!NotImplemented"},
		{"PUT", "/b", "metrics", nil, "!NotImplemented"},
		{"GET", "/b", "intelligent-tiering", nil, "!NotImplemented"},
		{"GET", "/b", "unknownparam=1", nil, "ListObjects"},

		{"PUT", "/b/k", "", nil, "PutObject"},
		{"PUT", "/b/k", "x-id=PutObject", nil, "PutObject"},
		{"PUT", "/b/k", "", map[string]string{"X-Amz-Copy-Source": "/src/k"}, "CopyObject"},
		{"GET", "/b/k", "", nil, "GetObject"},
		{"GET", "/b/k", "versionId=v1&partNumber=2", nil, "GetObject"},
		{"HEAD", "/b/k", "", nil, "HeadObject"},
		{"HEAD", "/b/k", "tagging", nil, "HeadObject"},
		{"DELETE", "/b/k", "", nil, "DeleteObject"},
		{"DELETE", "/b/k", "versionId=v1", nil, "DeleteObject"},
		{"GET", "/b/k", "tagging", nil, "GetObjectTagging"},
		{"PUT", "/b/k", "tagging", nil, "PutObjectTagging"},
		{"DELETE", "/b/k", "tagging", nil, "DeleteObjectTagging"},
		{"GET", "/b/k", "acl", nil, "GetObjectAcl"},
		{"PUT", "/b/k", "acl", nil, "PutObjectAcl"},
		{"DELETE", "/b/k", "acl", nil, "!MethodNotAllowed"},
		{"GET", "/b/k", "retention", nil, "GetObjectRetention"},
		{"PUT", "/b/k", "retention", nil, "PutObjectRetention"},
		{"GET", "/b/k", "legal-hold", nil, "GetObjectLegalHold"},
		{"PUT", "/b/k", "legal-hold", nil, "PutObjectLegalHold"},
		{"GET", "/b/k", "attributes", nil, "GetObjectAttributes"},
		{"PUT", "/b/k", "attributes", nil, "!MethodNotAllowed"},
		{"POST", "/b/k", "uploads", nil, "CreateMultipartUpload"},
		{"GET", "/b/k", "uploads", nil, "!MethodNotAllowed"},
		{"PUT", "/b/k", "partNumber=1&uploadId=u", nil, "UploadPart"},
		{"PUT", "/b/k", "partNumber=1&uploadId=u", map[string]string{"X-Amz-Copy-Source": "src/k"}, "UploadPartCopy"},
		{"POST", "/b/k", "uploadId=u", nil, "CompleteMultipartUpload"},
		{"DELETE", "/b/k", "uploadId=u", nil, "AbortMultipartUpload"},
		{"GET", "/b/k", "uploadId=u", nil, "ListParts"},
		{"POST", "/b/k", "restore", nil, "RestoreObject"},
		{"POST", "/b/k", "select&select-type=2", nil, "SelectObjectContent"},
		{"POST", "/b/k", "", nil, "!MethodNotAllowed"},
		{"GET", "/b/k", "torrent", nil, "!NotImplemented"},
		{"PUT", "/b/k", "renameObject", nil, "!NotImplemented"},
		{"PATCH", "/b/k", "", nil, "!MethodNotAllowed"},
		{"GET", "/b/k", "versioning", nil, "GetObject"}, // bucket subresource on an object is ignored
	}
	for _, c := range cases {
		name := c.method + " " + c.target + "?" + c.query
		t.Run(name, func(t *testing.T) {
			q, _ := url.ParseQuery(c.query)
			h := http.Header{}
			for k, v := range c.hdr {
				h.Set(k, v)
			}
			var tg requestTarget
			parts := strings.SplitN(strings.TrimPrefix(c.target, "/"), "/", 2)
			tg.Bucket = parts[0]
			if len(parts) == 2 {
				tg.Key = parts[1]
			}
			got, err := detectOp(c.method, tg, q, h)
			if strings.HasPrefix(c.want, "!") {
				e := s3err.As(err)
				if err == nil || e.Code != c.want[1:] {
					t.Fatalf("got op %q err %v, want %s", got, err, c.want)
				}
				if e.Code == "MethodNotAllowed" && (e.Extra["Method"] != c.method || e.Extra["ResourceType"] == "") {
					t.Fatalf("MethodNotAllowed extras %v", e.Extra)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			if lookupOp(got) == nil {
				t.Fatalf("op %q missing from opTable", got)
			}
		})
	}
}

// TestOpTable checks that every routable operation exists and has a
// handler, an IAM action and a sensible resource kind.
func TestOpTable(t *testing.T) {
	routed := map[string]bool{"ListBuckets": true, "CreateBucket": true, "DeleteBucket": true, "HeadBucket": true,
		"ListObjects": true, "ListObjectsV2": true, "DeleteObjects": true, "PostObject": true, "PutObject": true,
		"CopyObject": true, "GetObject": true, "HeadObject": true, "DeleteObject": true, "UploadPartCopy": true}
	for _, sr := range bucketSubresources {
		for _, op := range []string{sr.get, sr.put, sr.delete} {
			if op != "" {
				routed[op] = true
			}
		}
	}
	for _, sr := range objectSubresources {
		for _, op := range []string{sr.get, sr.put, sr.delete, sr.post} {
			if op != "" {
				routed[op] = true
			}
		}
	}
	for name := range routed {
		if lookupOp(name) == nil {
			t.Errorf("routed op %q has no spec", name)
		}
	}
	for _, o := range opTable {
		if !routed[o.Name] {
			t.Errorf("op %q is never routed", o.Name)
		}
		if o.handler == nil || !strings.HasPrefix(o.Action, "s3:") {
			t.Errorf("op %q: handler/action missing", o.Name)
		}
		if o.Kind == resService && o.Name != "ListBuckets" {
			t.Errorf("op %q has service kind", o.Name)
		}
	}
	if got := lookupOp("GetObject").action("v1"); got != "s3:GetObjectVersion" {
		t.Errorf("versioned action %q", got)
	}
	if got := lookupOp("GetObject").action(""); got != "s3:GetObject" {
		t.Errorf("action %q", got)
	}
	if got := lookupOp("PutObject").action("v1"); got != "s3:PutObject" {
		t.Errorf("action %q", got)
	}
}

// TestRoutingEndToEnd drives path-style and virtual-hosted-style requests
// through the full pipeline to overridden handlers.
func TestRoutingEndToEnd(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) { s.Domains = []string{"s3.test"} }))
	e.createBucket("bkt")
	for _, op := range []string{"GetObject", "ListObjectsV2", "GetBucketCors", "PutObject"} {
		e.override(op, okHandler)
	}
	for _, tc := range []struct {
		method, path, host string
		want               string
	}{
		{"GET", "/bkt/dir//file.txt", "", "GetObject bkt dir//file.txt"},
		{"GET", "/bkt?list-type=2", "", "ListObjectsV2 bkt "},
		{"GET", "/?cors", "bkt.s3.test", "GetBucketCors bkt "},
		{"GET", "/a%20b/c", "bkt.s3.test", "GetObject bkt a b/c"},
		{"PUT", "/bkt/new%2Fkey", "", "PutObject bkt new/key"},
	} {
		h := http.Header{}
		if tc.host != "" {
			h.Set("Host", tc.host)
		}
		resp := e.signed(tc.method, tc.path, nil, h)
		if resp.StatusCode != 200 || string(resp.Data) != tc.want {
			t.Errorf("%s %s (%s): %d %q, want %q", tc.method, tc.path, tc.host, resp.StatusCode, resp.Data, tc.want)
		}
	}
	// A key over 1024 bytes is rejected before the handler runs.
	resp := e.signed("GET", "/bkt/"+strings.Repeat("k", 1025), nil, nil)
	if resp.StatusCode != 400 || resp.code() != "KeyTooLongError" {
		t.Fatalf("long key: %d %s", resp.StatusCode, resp.code())
	}
	resp = e.signed("GET", "/bkt/"+strings.Repeat("k", 1024), nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("1024-byte key: %d %s", resp.StatusCode, resp.Data)
	}
}

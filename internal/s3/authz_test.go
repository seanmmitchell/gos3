package s3

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

const publicReadPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"PublicRead","Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::pub/*"]}]}`

func newAuthzEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	requireAuthz(t, e)
	for _, op := range []string{"GetObject", "HeadObject", "PutObject", "ListObjects", "ListObjectsV2", "DeleteObject", "ListBuckets", "CreateBucket", "GetBucketPolicy"} {
		e.override(op, okHandler)
	}
	return e
}

func TestAuthzAnonymous(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("priv")
	e.putObject("priv", "k", []byte("x"), storage.ObjectMeta{})

	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/", 403, "AccessDenied"},
		{"GET", "/priv/k", 403, "AccessDenied"},
		{"PUT", "/priv/k", 403, "AccessDenied"},
		{"GET", "/priv", 403, "AccessDenied"},
		{"PUT", "/newbucket", 403, "AccessDenied"},
		{"GET", "/missing/k", 404, "NoSuchBucket"}, // like AWS, existence leaks before authz
	} {
		resp := e.anon(tc.method, tc.path, nil, nil)
		if resp.StatusCode != tc.status || resp.code() != tc.code {
			t.Errorf("anon %s %s: %d %s, want %d %s", tc.method, tc.path, resp.StatusCode, resp.code(), tc.status, tc.code)
		}
	}
	// Root is allowed everywhere (handlers decide the rest).
	for _, p := range []string{"/", "/priv/k", "/priv", "/missing/k"} {
		if resp := e.signed("GET", p, nil, nil); resp.StatusCode != 200 {
			t.Errorf("root GET %s: %d %s", p, resp.StatusCode, resp.Data)
		}
	}
	if !strings.Contains(e.logs.String(), `"reason":"no matching allow"`) {
		t.Errorf("decision reason not logged at debug:\n%s", e.logs.String())
	}
}

func TestAuthzBucketPolicy(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("pub")
	e.putObject("pub", "a/b.txt", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("pub", storage.ConfigPolicy, publicReadPolicy)

	if resp := e.anon("GET", "/pub/a/b.txt", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("public read: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.anon("HEAD", "/pub/a/b.txt", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("public head: %d", resp.StatusCode)
	}
	if resp := e.anon("PUT", "/pub/a/b.txt", []byte("y"), nil); resp.StatusCode != 403 {
		t.Fatalf("public write: %d", resp.StatusCode)
	}
	if resp := e.anon("GET", "/pub", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("public list: %d", resp.StatusCode)
	}

	// Policy change is picked up immediately (cache revalidates on bytes).
	e.putBucketConfig("pub", storage.ConfigPolicy, strings.Replace(publicReadPolicy, `"Allow"`, `"Deny"`, 1))
	if resp := e.anon("GET", "/pub/a/b.txt", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("after policy change: %d", resp.StatusCode)
	}
	if err := e.backend.DeleteBucketConfig(context.Background(), "pub", storage.ConfigPolicy); err != nil {
		t.Fatal(err)
	}
	e.server.invalidateBucketConfig("pub")
	if resp := e.anon("GET", "/pub/a/b.txt", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("after policy delete: %d", resp.StatusCode)
	}
}

func TestAuthzExplicitDeny(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("pub")
	e.putObject("pub", "secret/x", []byte("x"), storage.ObjectMeta{})
	e.putObject("pub", "open/x", []byte("x"), storage.ObjectMeta{})
	alice := e.createUser("alice", iam.PolicyReadWrite)
	if resp := e.signedAs(alice, "GET", "/pub/secret/x", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("readwrite user: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Statement":[
		{"Effect":"Deny","Principal":{"AWS":["alice"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/secret/*"},
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	if resp := e.signedAs(alice, "GET", "/pub/secret/x", nil, nil); resp.StatusCode != 403 || resp.code() != "AccessDenied" {
		t.Fatalf("explicit deny: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(alice, "GET", "/pub/open/x", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("other prefix: %d", resp.StatusCode)
	}
	// Root bypasses bucket policies.
	if resp := e.signed("GET", "/pub/secret/x", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("root: %d", resp.StatusCode)
	}
	// Read-only users cannot write; write-only users cannot read.
	bob := e.createUser("bob", iam.PolicyReadOnly)
	if resp := e.signedAs(bob, "PUT", "/pub/open/y", []byte("1"), nil); resp.StatusCode != 403 {
		t.Fatalf("readonly put: %d", resp.StatusCode)
	}
	carol := e.createUser("carol", iam.PolicyWriteOnly)
	if resp := e.signedAs(carol, "PUT", "/pub/open/y", []byte("1"), nil); resp.StatusCode != 200 {
		t.Fatalf("writeonly put: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestAuthzConditions(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("cond")
	e.putObject("cond", "tagged", []byte("x"), storage.ObjectMeta{Tags: map[string]string{"class": "secret"}})
	e.putObject("cond", "plain", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("cond", storage.ConfigPolicy, `{"Statement":[
		{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::cond","arn:aws:s3:::cond/*"]},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::cond/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/class":"secret"}}},
		{"Effect":"Deny","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::cond","Condition":{"StringNotEquals":{"s3:prefix":"public/"}}}]}`)
	if resp := e.anon("GET", "/cond/plain", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("plain: %d", resp.StatusCode)
	}
	if resp := e.anon("GET", "/cond/tagged", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("tagged: %d", resp.StatusCode)
	}
	if resp := e.anon("GET", "/cond?prefix=public/", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("list public/: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.anon("GET", "/cond?prefix=private/", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("list private/: %d", resp.StatusCode)
	}

	// aws:SourceIp: deny everything from loopback for a user.
	e.putPolicy("noloop", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"},
		{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`)
	dave := e.createUser("dave", "noloop")
	if resp := e.signedAs(dave, "GET", "/cond/plain", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("source ip deny: %d", resp.StatusCode)
	}
	// Identity policy that tests existing object tags.
	e.putPolicy("tagread", `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{"s3:ExistingObjectTag/class":"secret"}}}]}`)
	erin := e.createUser("erin", "tagread")
	if err := e.backend.DeleteBucketConfig(context.Background(), "cond", storage.ConfigPolicy); err != nil {
		t.Fatal(err)
	}
	if resp := e.signedAs(erin, "GET", "/cond/tagged", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("tag allow: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(erin, "GET", "/cond/plain", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("tag allow (untagged): %d", resp.StatusCode)
	}
}

func TestAuthzCannedACLs(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("acl")
	e.putObject("acl", "pub", []byte("x"), storage.ObjectMeta{ACL: "public-read"})
	e.putObject("acl", "auth", []byte("x"), storage.ObjectMeta{ACL: "authenticated-read"})
	e.putObject("acl", "priv", []byte("x"), storage.ObjectMeta{})
	nobody := e.createUser("nobody")

	check := func(name string, resp *testResponse, want int) {
		t.Helper()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d (%s)", name, resp.StatusCode, want, resp.Data)
		}
	}
	check("public-read object", e.anon("GET", "/acl/pub", nil, nil), 200)
	check("public-read object versioned", e.anon("GET", "/acl/pub?versionId=null", nil, nil), 200)
	check("private object", e.anon("GET", "/acl/priv", nil, nil), 403)
	check("authenticated-read anon", e.anon("GET", "/acl/auth", nil, nil), 403)
	check("authenticated-read user", e.signedAs(nobody, "GET", "/acl/auth", nil, nil), 200)
	check("object ACL does not grant writes", e.anon("PUT", "/acl/pub", []byte("1"), nil), 403)
	check("bucket list private", e.anon("GET", "/acl", nil, nil), 403)

	// Bucket ACL governs listing and writes, not object reads.
	e.putBucketConfig("acl", storage.ConfigACL, "public-read-write")
	check("bucket public-read-write list", e.anon("GET", "/acl", nil, nil), 200)
	check("bucket public-read-write put", e.anon("PUT", "/acl/new", []byte("1"), nil), 200)
	check("bucket ACL does not expose objects", e.anon("GET", "/acl/priv", nil, nil), 403)

	// AccessControlPolicy XML form.
	e.putBucketConfig("acl", storage.ConfigACL, `<AccessControlPolicy><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant></AccessControlList></AccessControlPolicy>`)
	check("acl xml list", e.anon("GET", "/acl", nil, nil), 200)
	check("acl xml put", e.anon("PUT", "/acl/new", []byte("1"), nil), 403)

	// IgnorePublicAcls drops canned ACL grants.
	e.putBucketConfig("acl", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`)
	check("IgnorePublicAcls object", e.anon("GET", "/acl/pub", nil, nil), 403)
	check("IgnorePublicAcls bucket", e.anon("GET", "/acl", nil, nil), 403)
	if err := e.backend.DeleteBucketConfig(context.Background(), "acl", storage.ConfigPublicAccessBlock); err != nil {
		t.Fatal(err)
	}
	check("PAB removed", e.anon("GET", "/acl/pub", nil, nil), 200)

	// BucketOwnerEnforced disables ACLs.
	e.putBucketConfig("acl", storage.ConfigOwnership, `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)
	check("BucketOwnerEnforced object", e.anon("GET", "/acl/pub", nil, nil), 403)
	check("BucketOwnerEnforced bucket", e.anon("GET", "/acl", nil, nil), 403)
}

func TestAuthzRestrictPublicBuckets(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("pub")
	e.putObject("pub", "k", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Allow","Principal":{"AWS":"frank"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	frank := e.createUser("frank")
	if resp := e.anon("GET", "/pub/k", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("before: %d", resp.StatusCode)
	}
	e.putBucketConfig("pub", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	if resp := e.anon("GET", "/pub/k", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("restricted anon: %d", resp.StatusCode)
	}
	// Grants to named users still apply.
	if resp := e.signedAs(frank, "PUT", "/pub/k", []byte("1"), nil); resp.StatusCode != 200 {
		t.Fatalf("restricted named user: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestAuthorizeHelperSecondaryResource(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("src")
	e.createBucket("dst")
	e.putObject("src", "k", []byte("x"), storage.ObjectMeta{})
	e.putPolicy("dstonly", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::dst/*"}]}`)
	gina := e.createUser("gina", "dstonly")
	var srcErr error
	e.override("CopyObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		cs, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
		if err != nil {
			return err
		}
		if srcErr = s.authorize(r, rc, objectReadAction(cs.VersionID), cs.Bucket, cs.Key, cs.VersionID); srcErr != nil {
			return srcErr
		}
		writeEmpty(w, 200)
		return nil
	})
	h := http.Header{"X-Amz-Copy-Source": {"/src/k"}}
	if resp := e.signedAs(gina, "PUT", "/dst/k", nil, h); resp.StatusCode != 403 || srcErr == nil {
		t.Fatalf("source not authorized: %d %v", resp.StatusCode, srcErr)
	}
	e.putBucketConfig("src", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"gina"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::src/*"}]}`)
	if resp := e.signedAs(gina, "PUT", "/dst/k", nil, h); resp.StatusCode != 200 {
		t.Fatalf("source authorized: %d %s", resp.StatusCode, resp.Data)
	}
	// Missing source bucket => NoSuchBucket from the helper.
	h.Set("X-Amz-Copy-Source", "nobucket/k")
	if resp := e.signedAs(gina, "PUT", "/dst/k", nil, h); resp.code() != "NoSuchBucket" {
		t.Fatalf("missing source bucket: %s", resp.Data)
	}
}

func TestAuthzStoredPolicyCorruptFailsClosed(t *testing.T) {
	e := newAuthzEnv(t)
	e.createBucket("bad")
	e.putBucketConfig("bad", storage.ConfigPolicy, `{not json`)
	if resp := e.anon("GET", "/bad/k", nil, nil); resp.StatusCode != 500 {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signed("GET", "/bad/k", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("root unaffected: %d", resp.StatusCode)
	}
}

func TestConfigCache(t *testing.T) {
	c := newConfigCache(3)
	parses := 0
	parse := func(b []byte) (any, error) {
		parses++
		if string(b) == "bad" {
			return nil, fmt.Errorf("bad")
		}
		return string(b) + "!", nil
	}
	get := func(bucket, raw string) any {
		v, _ := c.get(bucket, "k", []byte(raw), parse)
		return v
	}
	first, second := get("a", "1"), get("a", "1") // the second call must hit the cache
	if first != "1!" || second != "1!" || parses != 1 {
		t.Fatalf("cache miss: %d", parses)
	}
	if get("a", "2") != "2!" || parses != 2 {
		t.Fatal("stale entry served")
	}
	if _, err := c.get("e", "k", []byte("bad"), parse); err == nil {
		t.Fatal("error not returned")
	}
	if _, err := c.get("e", "k", []byte("bad"), parse); err == nil || parses != 3 {
		t.Fatal("error not cached")
	}
	get("b", "1")
	get("c", "1") // evicts LRU
	if c.len() != 3 {
		t.Fatalf("len %d", c.len())
	}
	c.invalidate("c")
	if c.len() != 2 {
		t.Fatalf("invalidate: %d", c.len())
	}
}

// TestAuthzGetObjectAttributesRequiresObjectRead checks that
// GetObjectAttributes, whose response reveals the content's ETag (its MD5)
// and checksums, requires s3:GetObject (s3:GetObjectVersion with a
// versionId) besides s3:GetObjectAttributes (s3:GetObjectVersionAttributes),
// as on AWS: missing or denying either action yields AccessDenied.
func TestAuthzGetObjectAttributesRequiresObjectRead(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("pub")
	info := e.putObject("pub", "k", []byte("hunter2"), storage.ObjectMeta{})
	e.putObject("pub", "acl", []byte("hunter2"), storage.ObjectMeta{ACL: "public-read"})
	allow := func(name string, actions ...string) auth.Credentials {
		t.Helper()
		e.putPolicy(name, fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Action":["%s"],"Resource":"arn:aws:s3:::pub/*"}]}`, strings.Join(actions, `","`)))
		return e.createUser(name, name)
	}
	e.putPolicy("attrs-deny-get", `{"Statement":[
		{"Effect":"Allow","Action":["s3:GetObjectAttributes","s3:GetObject"],"Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	denyGet := e.createUser("attrs-deny-get", "attrs-deny-get")
	attrsOnly := allow("attrs-only", "s3:GetObjectAttributes")
	both := allow("attrs-get", "s3:GetObjectAttributes", "s3:GetObject")
	versionAttrsOnly := allow("vattrs-get", "s3:GetObjectVersionAttributes", "s3:GetObject")
	versionBoth := allow("vattrs-getversion", "s3:GetObjectVersionAttributes", "s3:GetObjectVersion")

	hdr := http.Header{"X-Amz-Object-Attributes": {"ETag,ObjectSize"}}
	vpath := "/pub/k?attributes&versionId=" + info.VersionID
	for _, tc := range []struct {
		who    string
		creds  auth.Credentials
		path   string
		status int
	}{
		{"attributes only", attrsOnly, "/pub/k?attributes", 403},
		{"attributes, GetObject denied", denyGet, "/pub/k?attributes", 403},
		{"attributes and GetObject", both, "/pub/k?attributes", 200},
		{"version: GetObject but not GetObjectVersion", versionAttrsOnly, vpath, 403},
		{"version: GetObjectAttributes but not GetObjectVersionAttributes", both, vpath, 403},
		{"version: both version actions", versionBoth, vpath, 200},
	} {
		resp := e.signedAs(tc.creds, "GET", tc.path, nil, hdr)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: GET %s = %d %s, want %d", tc.who, tc.path, resp.StatusCode, resp.Data, tc.status)
			continue
		}
		if tc.status == 403 {
			if resp.code() != "AccessDenied" {
				t.Errorf("%s: code %s", tc.who, resp.code())
			}
			if strings.Contains(string(resp.Data), md5Hex([]byte("hunter2"))) {
				t.Errorf("%s: ETag revealed: %s", tc.who, resp.Data)
			}
		}
	}

	// Anonymous callers: the bucket policy must grant both actions; the
	// object's canned ACL can stand in for s3:GetObject.
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObjectAttributes","Resource":"arn:aws:s3:::pub/*"}]}`)
	if resp := e.anon("GET", "/pub/k?attributes", nil, hdr); resp.StatusCode != 403 {
		t.Errorf("anonymous, attributes only: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.anon("GET", "/pub/acl?attributes", nil, hdr); resp.StatusCode != 200 {
		t.Errorf("anonymous, attributes + public-read object ACL: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObjectAttributes","s3:GetObject"],"Resource":"arn:aws:s3:::pub/*"}]}`)
	if resp := e.anon("GET", "/pub/k?attributes", nil, hdr); resp.StatusCode != 200 {
		t.Errorf("anonymous, both: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Statement":[
		{"Effect":"Allow","Principal":"*","Action":["s3:GetObjectAttributes","s3:GetObject"],"Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	if resp := e.anon("GET", "/pub/acl?attributes", nil, hdr); resp.StatusCode != 403 {
		t.Errorf("anonymous, GetObject denied by the bucket policy: %d %s", resp.StatusCode, resp.Data)
	}
	// A bucket policy Deny of s3:GetObject also binds IAM users.
	if resp := e.signedAs(both, "GET", "/pub/k?attributes", nil, hdr); resp.StatusCode != 403 {
		t.Errorf("user, GetObject denied by the bucket policy: %d %s", resp.StatusCode, resp.Data)
	}
}

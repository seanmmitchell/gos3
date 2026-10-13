package s3

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// TestHeaderObjectAttrsMatchConditionKeys: a header value is either stored
// exactly as its condition key says or refused. Unicode whitespace around
// x-amz-acl, object lock and encryption values is not trimmed away after
// the policy saw it, and a compact ISO 8601 retain-until date is compared
// as the date it stands for.
func TestHeaderObjectAttrsMatchConditionKeys(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	if resp := e.signed("PUT", "/nb1", nil, objHdr("X-Amz-Bucket-Object-Lock-Enabled", "true")); resp.StatusCode != 200 {
		t.Fatalf("create lock bucket: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("nb1", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectAcl","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"StringEquals":{"s3:x-amz-acl":["public-read","public-read-write","authenticated-read"]}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectAcl","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"StringEquals":{"s3:object-lock-mode":"COMPLIANCE"}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectLegalHold"],"Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"StringEquals":{"s3:object-lock-legal-hold":"ON"}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObject","s3:PutObjectRetention"],"Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"DateGreaterThan":{"s3:object-lock-retain-until-date":"2090-01-01T00:00:00Z"}}},
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"StringEquals":{"s3:x-amz-server-side-encryption":"aws:kms"}}},
		{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::nb1/*",
		 "Condition":{"StringEquals":{"s3:x-amz-storage-class":"GLACIER"}}}]}`)
	const nbsp, nel = " ", "\u0085"
	soon := time.Now().Add(48 * time.Hour).UTC()
	for i, h := range []http.Header{
		objHdr("X-Amz-Acl", "public-read"+nbsp),
		objHdr("X-Amz-Acl", nel+"public-read"),
		objHdr("X-Amz-Object-Lock-Mode", "COMPLIANCE"+nbsp, "X-Amz-Object-Lock-Retain-Until-Date", soon.Format(time.RFC3339)),
		objHdr("X-Amz-Object-Lock-Legal-Hold", "ON"+nbsp),
		objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Retain-Until-Date", "20990101T000000Z"),
		objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Retain-Until-Date", "2099-01-01T00:00:00.000+0000"),
		objHdr("X-Amz-Server-Side-Encryption", "aws:kms"+nbsp),
		objHdr("X-Amz-Storage-Class", "GLACIER"+nbsp),
	} {
		key := "/nb1/o" + string(rune('a'+i))
		if resp := e.signedAs(bob, "PUT", key, []byte("x"), h); resp.StatusCode/100 == 2 {
			info, _ := e.backend.GetObjectInfo(context.Background(), "nb1", key[len("/nb1/"):], "")
			t.Errorf("PutObject %v: %d; stored acl=%q mode=%q until=%v hold=%v sse=%q class=%q", h, resp.StatusCode,
				info.ACL, info.RetentionMode, info.RetainUntil, info.LegalHold, info.SSE, info.StorageClass)
		}
		if resp := e.signedAs(bob, "POST", "/nb1/mp"+string(rune('a'+i))+"?uploads", nil, h); resp.StatusCode/100 == 2 {
			t.Errorf("CreateMultipartUpload %v: %d", h, resp.StatusCode)
		}
	}
	if resp := e.signedAs(bob, "PUT", "/nb1/plain", []byte("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("plain put: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/nb1/plain?acl", nil, objHdr("X-Amz-Acl", "public-read"+nbsp)); resp.StatusCode/100 == 2 {
		t.Errorf("PutObjectAcl with NBSP: %d", resp.StatusCode)
	}
	if resp := e.anon("GET", "/nb1/plain", nil, nil); resp.StatusCode != 403 {
		t.Errorf("object published: %d", resp.StatusCode)
	}
	// Canonical values still work where the policy allows them.
	if resp := e.signedAs(bob, "PUT", "/nb1/gov", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", soon.Format("20060102T150405Z"), "X-Amz-Acl", "private")); resp.StatusCode != 200 {
		t.Errorf("governance put: %d %s", resp.StatusCode, resp.Data)
	}

	// CreateBucket / PutBucketAcl validate the canned ACL the same way.
	if resp := e.signed("PUT", "/nb2", nil, objHdr("X-Amz-Acl", "public-read"+nbsp)); resp.StatusCode/100 == 2 {
		t.Errorf("CreateBucket with NBSP ACL: %d", resp.StatusCode)
	}
	if resp := e.signed("PUT", "/nb1?acl", nil, objHdr("X-Amz-Acl", "public-read"+nbsp)); resp.StatusCode/100 == 2 {
		t.Errorf("PutBucketAcl with NBSP ACL: %d", resp.StatusCode)
	}
}

// TestSignatureAgeCondition: s3:signatureAge is the age of the signature in
// milliseconds, so AWS's recommended "deny old presigned URLs" bucket
// policy applies to presigned URLs, header-signed requests and POST forms.
func TestSignatureAgeCondition(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	dave := e.createUser("dave", "readwrite")
	e.createBucket("sigage")
	e.putObject("sigage", "k", []byte("secret"), storage.ObjectMeta{})
	e.putBucketConfig("sigage", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::sigage","arn:aws:s3:::sigage/*"],
		 "Condition":{"NumericGreaterThan":{"s3:signatureAge":"600000"}}}]}`)
	presign := func(signedAt time.Time) string {
		u, err := auth.PresignURL("GET", e.url("/sigage/k"), dave, "us-east-1", time.Hour, signedAt)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if resp := e.do(e.newRequestURL("GET", presign(time.Now()), nil)); resp.StatusCode != 200 {
		t.Fatalf("fresh presigned URL: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.do(e.newRequestURL("GET", presign(time.Now().Add(-20*time.Minute)), nil)); resp.StatusCode != 403 {
		t.Errorf("20-minute-old presigned URL: %d %s", resp.StatusCode, resp.Data)
	}

	// Header authentication (within the 15-minute clock skew).
	e.putBucketConfig("sigage", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::sigage","arn:aws:s3:::sigage/*"],
		 "Condition":{"NumericGreaterThan":{"s3:signatureAge":"60000"}}}]}`)
	req := e.newRequest("GET", "/sigage/k", nil, nil)
	if err := auth.SignRequest(req, dave, "us-east-1", "s3", "", time.Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(req); resp.StatusCode != 403 {
		t.Errorf("5-minute-old header signature: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(dave, "GET", "/sigage/k", nil, nil); resp.StatusCode != 200 {
		t.Errorf("fresh header signature: %d %s", resp.StatusCode, resp.Data)
	}

	// POST policy forms: the age of the form's x-amz-date.
	conds := []any{map[string]string{"bucket": "sigage"}, []any{"starts-with", "$key", ""}}
	form, err := auth.SignPostPolicy(dave, "us-east-1", time.Now().Add(-10*time.Minute), time.Now().Add(time.Hour), conds)
	if err != nil {
		t.Fatal(err)
	}
	fields := []objPostField{{"key", "posted"}}
	for _, k := range []string{"x-amz-algorithm", "x-amz-credential", "x-amz-date", "policy", "x-amz-signature"} {
		fields = append(fields, objPostField{k, form[k]})
	}
	body, ct := objPostBody(t, fields, "f.txt", []byte("x"))
	if resp := objPost(e, "sigage", body, ct); resp.StatusCode != 403 {
		t.Errorf("10-minute-old POST policy: %d %s", resp.StatusCode, resp.Data)
	}
	body, ct = objPostBody(t, objSignedPostFields(t, dave, time.Now().Add(time.Hour), conds, objPostField{"key", "posted"}), "f.txt", []byte("x"))
	if resp := objPost(e, "sigage", body, ct); resp.StatusCode/100 != 2 {
		t.Errorf("fresh POST policy: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestGlobalWildcardCORSAnonymousPostObject: PostObject is not
// authenticated by the core, so an Authorization header or presign query
// parameters on it do not make an anonymous form upload "signed" for the
// global "*" CORS rule: the cross-origin anonymous upload is refused (see
// checkAnonymousRequest) and gets no CORS headers.
func TestGlobalWildcardCORSAnonymousPostObject(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) {
		s.CORS = config.CORSConfig{AllowedOrigins: []string{"*"}}
	}))
	requireAuthz(t, e)
	e.createBucket("pwb")
	e.putBucketConfig("pwb", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
		"Action":"s3:PutObject","Resource":"arn:aws:s3:::pwb/*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`)
	for i, tc := range []struct {
		path string
		hdr  http.Header
	}{
		{"/pwb", nil},
		{"/pwb?X-Amz-Credential=x", nil},
		{"/pwb?X-Amz-Signature=x&X-Amz-Algorithm=AWS4-HMAC-SHA256", nil},
		{"/pwb", http.Header{"Authorization": {"x"}}},
	} {
		body, ct := objPostBody(t, []objPostField{{"key", "k" + string(rune('a'+i))}}, "f.txt", []byte("x"))
		h := objHdr("Content-Type", ct, "Origin", "https://evil.example")
		for k, v := range tc.hdr {
			h[k] = v
		}
		resp := e.anon("POST", tc.path, body, h)
		if resp.StatusCode != 403 {
			t.Fatalf("POST %s %v: %d %s", tc.path, tc.hdr, resp.StatusCode, resp.Data)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("anonymous POST %s %v: Access-Control-Allow-Origin %q", tc.path, tc.hdr, got)
		}
	}
}

// TestPresignedQueryHoistedChecksumSettings: of the x-amz-checksum-*
// query parameters only the checksum values (computed by presigners over
// an empty body) are ignored; the algorithm, type and mode settings are
// signed request headers like any other.
func TestPresignedQueryHoistedChecksumSettings(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "src", []byte("data"), storage.ObjectMeta{})

	u := presignWith(t, e, testRoot, "POST", "/b/mp?uploads=", url.Values{"x-amz-checksum-algorithm": {"SHA256"}})
	resp := e.do(e.newRequestURL("POST", u, nil))
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Checksum-Algorithm") != "SHA256" {
		t.Errorf("presigned CreateMultipartUpload: %d %v %s", resp.StatusCode, resp.Header, resp.Data)
	}

	u = presignWith(t, e, testRoot, "PUT", "/b/dst", url.Values{"x-amz-copy-source": {"b/src"}, "x-amz-checksum-algorithm": {"SHA1"}})
	if resp := e.do(e.newRequestURL("PUT", u, nil)); resp.StatusCode != 200 {
		t.Fatalf("presigned copy: %d %s", resp.StatusCode, resp.Data)
	}
	if info, err := e.backend.GetObjectInfo(context.Background(), "b", "dst", ""); err != nil || info.Checksums[ChecksumSHA1] == "" {
		t.Errorf("presigned copy checksums: %v %v", info.Checksums, err)
	}

	u = presignWith(t, e, testRoot, "GET", "/b/dst", url.Values{"x-amz-checksum-mode": {"ENABLED"}})
	if resp := e.do(e.newRequestURL("GET", u, nil)); resp.StatusCode != 200 || resp.Header.Get("X-Amz-Checksum-Sha1") == "" {
		t.Errorf("presigned GetObject with checksum mode: %d %v", resp.StatusCode, resp.Header)
	}

	// Checksum values are still ignored (they describe an empty body).
	u = presignWith(t, e, testRoot, "PUT", "/b/up", url.Values{"x-amz-checksum-sha256": {"47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="}, "x-amz-checksum-type": {"FULL_OBJECT"}})
	if resp := e.do(e.newRequestURL("PUT", u, []byte("not empty"))); resp.StatusCode != 200 {
		t.Errorf("presigned put with an empty-body checksum: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestPresignedQueryParametersNotDroppable: rewriting a signed parameter
// so that net/url would drop it (%3B to ';', a literal '%') keeps the
// signature valid, so routing must still see the parameter: a presigned
// listing scoped to a prefix cannot be widened to the whole bucket, nor a
// response override stripped.
func TestPresignedQueryParametersNotDroppable(t *testing.T) {
	e := newTestEnv(t)
	carol := e.createUser("carol", "readonly")
	e.createBucket("b")
	for _, k := range []string{"team;a/one", "other/x", "secret/payroll.csv", "100%/x"} {
		e.putObject("b", k, []byte("x"), storage.ObjectMeta{})
	}
	for _, tc := range []struct{ prefix, from, to string }{
		{"team;a/", "%3B", ";"},
		{"100%/", "%25", "%"},
	} {
		u := presignWith(t, e, carol, "GET", "/b", url.Values{"list-type": {"2"}, "prefix": {tc.prefix}})
		if !strings.Contains(u, tc.from) {
			t.Fatalf("presigned URL %s lacks %s", u, tc.from)
		}
		u = strings.Replace(u, tc.from, tc.to, 1)
		resp := e.do(e.newRequestURL("GET", u, nil))
		if resp.StatusCode != 200 {
			t.Fatalf("rewritten presigned list %s: %d %s", tc.prefix, resp.StatusCode, resp.Data)
		}
		if strings.Contains(string(resp.Data), "secret/payroll.csv") || !strings.Contains(string(resp.Data), "<Prefix>"+tc.prefix) {
			t.Errorf("rewritten presigned list %s returned %s", tc.prefix, resp.Data)
		}
	}
	u := presignWith(t, e, carol, "GET", "/b/other/x", url.Values{"response-content-disposition": {"attachment; filename=x.html"}})
	resp := e.do(e.newRequestURL("GET", strings.Replace(u, "%3B", ";", 1), nil))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Disposition") != "attachment; filename=x.html" {
		t.Errorf("rewritten response override: %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
}

// TestACLBodyConditionKeys: an AccessControlPolicy body is authorized with
// the canned ACL it maps to as s3:x-amz-acl (and its grants as
// s3:x-amz-grant-*), so "deny public ACLs" policies cover the body form of
// PutObjectAcl and PutBucketAcl too.
func TestACLBodyConditionKeys(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bob := e.createUser("bob", "readwrite")
	e.createBucket("nb1")
	e.putObject("nb1", "body1", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("nb1", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObjectAcl","s3:PutBucketAcl"],"Resource":["arn:aws:s3:::nb1","arn:aws:s3:::nb1/*"],
		 "Condition":{"StringEquals":{"s3:x-amz-acl":["public-read","public-read-write","authenticated-read"]}}},
		{"Effect":"Deny","Principal":"*","Action":["s3:PutObjectAcl","s3:PutBucketAcl"],"Resource":["arn:aws:s3:::nb1","arn:aws:s3:::nb1/*"],
		 "Condition":{"StringLike":{"s3:x-amz-grant-write":"uri=*AllUsers"}}}]}`)
	grant := func(perm, uri string) string {
		return `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>` + uri + `</URI></Grantee><Permission>` + perm + `</Permission></Grant>`
	}
	acp := func(grants ...string) []byte {
		return []byte(`<AccessControlPolicy><AccessControlList>` + strings.Join(grants, "") + `</AccessControlList></AccessControlPolicy>`)
	}
	if resp := e.signedAs(bob, "PUT", "/nb1/body1?acl", acp(grant("READ", groupAllUsers)), nil); resp.StatusCode != 403 {
		t.Errorf("PutObjectAcl body AllUsers READ: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.anon("GET", "/nb1/body1", nil, nil); resp.StatusCode != 403 {
		t.Errorf("object published: %d", resp.StatusCode)
	}
	if resp := e.signedAs(bob, "PUT", "/nb1?acl", acp(grant("READ", groupAllUsers)), nil); resp.StatusCode != 403 {
		t.Errorf("PutBucketAcl body AllUsers READ: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/nb1?acl", acp(grant("READ", groupAllUsers), grant("WRITE", groupAllUsers)), nil); resp.StatusCode != 403 {
		t.Errorf("PutBucketAcl body AllUsers READ+WRITE: %d %s", resp.StatusCode, resp.Data)
	}
	// A private body is allowed.
	if resp := e.signedAs(bob, "PUT", "/nb1/body1?acl", acp(), nil); resp.StatusCode != 200 {
		t.Errorf("PutObjectAcl private body: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signedAs(bob, "PUT", "/nb1?acl", acp(), nil); resp.StatusCode != 200 {
		t.Errorf("PutBucketAcl private body: %d %s", resp.StatusCode, resp.Data)
	}
}

// racingDeleteBackend is a storage.ConditionalDeleter whose DeleteObjectIf first
// lets a concurrent write commit (beforeCheck), modelling a PUT or lock
// update that wins the race against the delete's own lookup.
type racingDeleteBackend struct {
	*memory.Backend
	mu          sync.Mutex
	beforeCheck func()
	calls       int
}

func (b *racingDeleteBackend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if f := b.beforeCheck; f != nil {
		b.beforeCheck = nil
		f()
	}
	info, err := b.GetObjectInfo(ctx, bucket, key, versionID)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if err := check(info); err != nil {
		return storage.DeleteResult{}, err
	}
	return b.DeleteObject(ctx, bucket, key, versionID)
}

// TestConditionalDeleteAtomic: DeleteObject's If-Match and object lock
// checks run inside the backend's delete (storage.ConditionalDeleter), so a write
// committed after the request started is checked, not deleted blindly.
func TestConditionalDeleteAtomic(t *testing.T) {
	var rb *racingDeleteBackend
	e := newTestEnv(t, withOptions(func(o *Options) {
		rb = &racingDeleteBackend{Backend: o.Backend.(*memory.Backend)}
		o.Backend = rb
	}))
	ctx := context.Background()
	e.createBucket("b")
	old := e.putObject("b", "k", []byte("old"), storage.ObjectMeta{})
	rb.beforeCheck = func() { e.putObject("b", "k", []byte("new-content"), storage.ObjectMeta{}) }
	if resp := e.signed("DELETE", "/b/k", nil, objHdr("If-Match", `"`+old.ETag+`"`)); resp.StatusCode != 412 {
		t.Errorf("If-Match on a replaced object: %d %s", resp.StatusCode, resp.Data)
	}
	if got := e.objectData("b", "k"); got != "new-content" {
		t.Errorf("newer object deleted: %q", got)
	}
	if resp := e.signed("DELETE", "/b/k", nil, objHdr("If-Match", `"`+md5Hex([]byte("new-content"))+`"`)); resp.StatusCode != 204 {
		t.Errorf("matching If-Match: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signed("DELETE", "/b/k", nil, objHdr("If-Match", "*")); resp.StatusCode != 404 {
		t.Errorf("If-Match on a missing object: %d %s", resp.StatusCode, resp.Data)
	}

	// A legal hold placed after the request started protects the version.
	if resp := e.signed("PUT", "/lockb", nil, objHdr("X-Amz-Bucket-Object-Lock-Enabled", "true")); resp.StatusCode != 200 {
		t.Fatalf("create lock bucket: %d %s", resp.StatusCode, resp.Data)
	}
	resp := e.signed("PUT", "/lockb/v", []byte("x"), nil)
	vid := resp.Header.Get("X-Amz-Version-Id")
	rb.beforeCheck = func() {
		if _, err := e.backend.UpdateObjectMeta(ctx, "lockb", "v", vid, func(m *storage.ObjectMeta) error { m.LegalHold = true; return nil }); err != nil {
			t.Error(err)
		}
	}
	if resp := e.signed("DELETE", "/lockb/v?versionId="+vid, nil, nil); resp.StatusCode != 403 {
		t.Errorf("delete of a version locked meanwhile: %d %s", resp.StatusCode, resp.Data)
	}
	if _, err := e.backend.GetObjectInfo(ctx, "lockb", "v", vid); err != nil {
		t.Errorf("locked version deleted: %v", err)
	}
	// Deleting a version that does not exist still succeeds.
	if resp := e.signed("DELETE", "/lockb/v?versionId=nope", nil, nil); resp.StatusCode != 204 {
		t.Errorf("delete of a missing version: %d %s", resp.StatusCode, resp.Data)
	}
	if rb.calls != 5 {
		t.Errorf("DeleteObjectIf calls = %d, want 5", rb.calls)
	}
}

// TestTLSVersionCondition: s3:TlsVersion is set for TLS connections, so the
// usual "deny TLS older than 1.2" Deny is evaluated (and passes on 1.3).
func TestTLSVersionCondition(t *testing.T) {
	e := newTestEnv(t, withTLS())
	requireAuthz(t, e)
	dave := e.createUser("dave", "readwrite")
	e.createBucket("tlsb")
	e.putObject("tlsb", "k", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("tlsb", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::tlsb/*",
		 "Condition":{"NumericLessThan":{"s3:TlsVersion":"1.3"}}}]}`)
	if resp := e.signedAs(dave, "GET", "/tlsb/k", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("TLS 1.3 request: %d %s", resp.StatusCode, resp.Data)
	}
	e.putBucketConfig("tlsb", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::tlsb/*",
		 "Condition":{"NumericLessThan":{"s3:TlsVersion":"1.4"}}}]}`)
	if resp := e.signedAs(dave, "GET", "/tlsb/k", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("TLS 1.3 request, deny below 1.4: %d %s", resp.StatusCode, resp.Data)
	}
}

package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- Tagging ---

func TestObjectTaggingSDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	e.putObject("bkt", "k", []byte("x"), storage.ObjectMeta{})
	get, err := e.client.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || len(get.TagSet) != 0 {
		t.Fatalf("empty tagging: %v %+v", err, get)
	}
	_, err = e.client.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("b"), Value: aws.String("2")}, {Key: aws.String("a"), Value: aws.String("")}}}})
	if err != nil {
		t.Fatalf("PutObjectTagging: %v", err)
	}
	get, err = e.client.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || len(get.TagSet) != 2 || aws.ToString(get.TagSet[0].Key) != "a" || aws.ToString(get.TagSet[1].Value) != "2" {
		t.Fatalf("GetObjectTagging: %v %+v", err, get)
	}
	if h, _ := e.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); h == nil {
		t.Fatal("head failed")
	}
	tooMany := make([]types.Tag, 11)
	for i := range tooMany {
		tooMany[i] = types.Tag{Key: aws.String(fmt.Sprintf("k%d", i)), Value: aws.String("v")}
	}
	_, err = e.client.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Tagging: &types.Tagging{TagSet: tooMany}})
	objSDKErr(t, err, 400, "InvalidTag")
	_, err = e.client.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("missing"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("b")}}}})
	objSDKErr(t, err, 404, "NoSuchKey")
	if _, err := e.client.DeleteObjectTagging(ctx, &awss3.DeleteObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); err != nil {
		t.Fatalf("DeleteObjectTagging: %v", err)
	}
	if info := objGetInfo(t, e, "bkt", "k", ""); len(info.Tags) != 0 {
		t.Errorf("tags left: %v", info.Tags)
	}
}

func TestObjectTaggingRawAndVersions(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	v1 := e.putObject("bkt", "k", []byte("1"), storage.ObjectMeta{Tags: map[string]string{"old": "yes"}}).VersionID
	v2 := e.putObject("bkt", "k", []byte("2"), storage.ObjectMeta{}).VersionID

	resp := e.signed("GET", "/bkt/k?tagging&versionId="+v1, nil, nil)
	objExpect(t, resp, 200, "")
	if resp.Header.Get("X-Amz-Version-Id") != v1 || !strings.Contains(string(resp.Data), `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet><Tag><Key>old</Key><Value>yes</Value></Tag></TagSet></Tagging>`) {
		t.Errorf("GET tagging v1: %v %s", resp.Header, resp.Data)
	}
	body := []byte(`<Tagging><TagSet><Tag><Key>new</Key><Value>1</Value></Tag></TagSet></Tagging>`)
	resp = e.signed("PUT", "/bkt/k?tagging", body, nil)
	objExpect(t, resp, 200, "")
	if resp.Header.Get("X-Amz-Version-Id") != v2 {
		t.Errorf("PUT tagging version %q", resp.Header.Get("X-Amz-Version-Id"))
	}
	if info := objGetInfo(t, e, "bkt", "k", v1); info.Tags["old"] != "yes" || len(info.Tags) != 1 {
		t.Errorf("v1 tags changed: %v", info.Tags)
	}
	for _, tc := range []struct {
		name   string
		body   string
		hdr    http.Header
		status int
		code   string
	}{
		{"malformed", "<Tagging><TagSet>", nil, 400, "MalformedXML"},
		{"empty", "", nil, 400, "MissingRequestBodyError"},
		{"dup key", `<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`, nil, 400, "InvalidTag"},
		{"long key", `<Tagging><TagSet><Tag><Key>` + strings.Repeat("k", 129) + `</Key><Value>1</Value></Tag></TagSet></Tagging>`, nil, 400, "InvalidTag"},
		{"bad md5", string(body), objHdr("Content-MD5", "AAAAAAAAAAAAAAAAAAAAAA=="), 400, "BadDigest"},
		{"too big", "<Tagging>" + strings.Repeat(" ", maxSubresourceBody) + "</Tagging>", nil, 400, "MaxMessageLengthExceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b []byte
			if tc.body != "" {
				b = []byte(tc.body)
			}
			objExpect(t, e.signed("PUT", "/bkt/k?tagging", b, tc.hdr), tc.status, tc.code)
		})
	}
	// Delete markers: latest => 404 NoSuchKey, addressed => 405.
	dm := e.signed("DELETE", "/bkt/k", nil, nil).Header.Get("X-Amz-Version-Id")
	resp = e.signed("GET", "/bkt/k?tagging", nil, nil)
	objExpect(t, resp, 404, "NoSuchKey")
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" {
		t.Errorf("delete marker header %v", resp.Header)
	}
	objExpect(t, e.signed("PUT", "/bkt/k?tagging&versionId="+dm, body, nil), 405, "MethodNotAllowed")
	objExpect(t, e.signed("DELETE", "/bkt/k?tagging&versionId=nope", nil, nil), 404, "NoSuchVersion")
	resp = e.signed("DELETE", "/bkt/k?tagging&versionId="+v1, nil, nil)
	if resp.StatusCode != 204 || resp.Header.Get("X-Amz-Version-Id") != v1 {
		t.Errorf("DELETE tagging v1: %d %v", resp.StatusCode, resp.Header)
	}
}

// --- ACL ---

func TestObjectACL(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	ctx := context.Background()
	e.putObject("bkt", "k", []byte("secret"), storage.ObjectMeta{})
	acl, err := e.client.GetObjectAcl(ctx, &awss3.GetObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("GetObjectAcl: %v", err)
	}
	owner := ownerFor("")
	if aws.ToString(acl.Owner.ID) != owner.ID || len(acl.Grants) != 1 || acl.Grants[0].Permission != types.PermissionFullControl ||
		acl.Grants[0].Grantee.Type != types.TypeCanonicalUser {
		t.Errorf("default ACL %+v", acl)
	}
	objExpect(t, e.anon("GET", "/bkt/k", nil, nil), 403, "AccessDenied")

	// x-amz-acl public-read makes the object readable anonymously.
	if _, err := e.client.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k"), ACL: types.ObjectCannedACLPublicRead}); err != nil {
		t.Fatalf("PutObjectAcl: %v", err)
	}
	acl, err = e.client.GetObjectAcl(ctx, &awss3.GetObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || len(acl.Grants) != 2 || aws.ToString(acl.Grants[1].Grantee.URI) != groupAllUsers || acl.Grants[1].Permission != types.PermissionRead {
		t.Fatalf("public ACL %v %+v", err, acl)
	}
	if r := e.anon("GET", "/bkt/k", nil, nil); r.StatusCode != 200 || string(r.Data) != "secret" {
		t.Errorf("anonymous read: %d", r.StatusCode)
	}

	// An AccessControlPolicy with only the owner grant makes it private again.
	_, err = e.client.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		AccessControlPolicy: &types.AccessControlPolicy{Owner: acl.Owner, Grants: acl.Grants[:1]}})
	if err != nil {
		t.Fatalf("PutObjectAcl XML: %v", err)
	}
	objExpect(t, e.anon("GET", "/bkt/k", nil, nil), 403, "AccessDenied")
	// Round trip of the public grant set.
	_, err = e.client.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		AccessControlPolicy: &types.AccessControlPolicy{Owner: acl.Owner, Grants: acl.Grants}})
	if err != nil || objGetInfo(t, e, "bkt", "k", "").ACL != aclPublicRead {
		t.Fatalf("PutObjectAcl public XML: %v", err)
	}
	// Grants to other users are not supported.
	_, err = e.client.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		AccessControlPolicy: &types.AccessControlPolicy{Owner: acl.Owner, Grants: []types.Grant{{
			Grantee: &types.Grantee{Type: types.TypeCanonicalUser, ID: aws.String("someone-else")}, Permission: types.PermissionRead}}}})
	objSDKErr(t, err, 501, "NotImplemented")
	_, err = e.client.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("missing"), ACL: types.ObjectCannedACLPrivate})
	objSDKErr(t, err, 404, "NoSuchKey")

	// Raw edge cases.
	objExpect(t, e.signed("PUT", "/bkt/k?acl", []byte("<AccessControlPolicy>"), nil), 400, "MalformedACLError")
	objExpect(t, e.signed("PUT", "/bkt/k?acl", nil, objHdr("X-Amz-Acl", "bogus")), 400, "InvalidArgument")
	objExpect(t, e.signed("PUT", "/bkt/k?acl", []byte("<x/>"), objHdr("X-Amz-Acl", "private")), 400, "UnexpectedContent")
	objExpect(t, e.signed("PUT", "/bkt/k?acl", nil, objHdr("X-Amz-Grant-Read", "id=x")), 501, "NotImplemented")
	objExpect(t, e.signed("PUT", "/bkt/k?acl", nil, nil), 400, "MissingRequestBodyError")
	bad := `<AccessControlPolicy><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>` +
		groupAllUsers + `</URI></Grantee><Permission>EVERYTHING</Permission></Grant></AccessControlList></AccessControlPolicy>`
	objExpect(t, e.signed("PUT", "/bkt/k?acl", []byte(bad), nil), 400, "MalformedACLError")

	// Ownership controls and public access block.
	e.putBucketConfig("bkt", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`)
	objExpect(t, e.signed("PUT", "/bkt/k?acl", nil, objHdr("X-Amz-Acl", "public-read-write")), 403, "AccessDenied")
	e.putBucketConfig("bkt", storage.ConfigOwnership, `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)
	objExpect(t, e.signed("PUT", "/bkt/k?acl", nil, objHdr("X-Amz-Acl", "private")), 400, "AccessControlListNotSupported")
	resp := e.signed("GET", "/bkt/k?acl", nil, nil)
	if resp.StatusCode != 200 || strings.Contains(string(resp.Data), "AllUsers") || !strings.Contains(string(resp.Data), `xsi:type="CanonicalUser"`) {
		t.Errorf("GetObjectAcl with ACLs disabled: %s", resp.Data)
	}
}

func TestCannedFromGrants(t *testing.T) {
	owner := ownerFor("alice")
	og := Grant{Grantee: Grantee{Type: "CanonicalUser", ID: owner.ID}, Permission: "FULL_CONTROL"}
	g := func(uri, perm string) Grant {
		return Grant{Grantee: Grantee{Type: "Group", URI: uri}, Permission: perm}
	}
	for _, tc := range []struct {
		name   string
		grants []Grant
		want   string
		code   string
	}{
		{"none", nil, "", ""},
		{"owner", []Grant{og}, "", ""},
		{"public-read", []Grant{og, g(groupAllUsers, "READ")}, aclPublicRead, ""},
		{"public-read-write", []Grant{og, g(groupAllUsers, "READ"), g(groupAllUsers, "WRITE")}, aclPublicReadWrite, ""},
		{"authenticated-read", []Grant{og, g(groupAuthenticatedUsers, "READ")}, aclAuthenticatedRead, ""},
		{"untyped group", []Grant{{Grantee: Grantee{URI: groupAllUsers}, Permission: "READ"}}, aclPublicRead, ""},
		{"write only", []Grant{g(groupAllUsers, "WRITE")}, "", "NotImplemented"},
		{"mixed groups", []Grant{g(groupAllUsers, "READ"), g(groupAuthenticatedUsers, "READ")}, "", "NotImplemented"},
		{"read acp", []Grant{g(groupAllUsers, "READ_ACP")}, "", "NotImplemented"},
		{"log delivery", []Grant{g(groupLogDelivery, "WRITE")}, "", "NotImplemented"},
		{"other user", []Grant{{Grantee: Grantee{Type: "CanonicalUser", ID: "x"}, Permission: "READ"}}, "", "NotImplemented"},
		{"email", []Grant{{Grantee: Grantee{Type: "AmazonCustomerByEmail", EmailAddress: "a@b"}, Permission: "READ"}}, "", "NotImplemented"},
		{"bad perm", []Grant{g(groupAllUsers, "ALL")}, "", "MalformedACLError"},
		{"bad type", []Grant{{Grantee: Grantee{Type: "Robot"}, Permission: "READ"}}, "", "MalformedACLError"},
	} {
		got, err := cannedFromGrants(&AccessControlPolicy{Grants: tc.grants}, owner.ID)
		if got != tc.want || codeOf(err) != tc.code {
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
}

// --- Retention and legal hold ---

func TestObjectRetentionSDK(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	objLockBucket(t, e, "lock")
	e.createBucket("plain")
	ctx := context.Background()
	e.putObject("plain", "k", []byte("x"), storage.ObjectMeta{})
	_, err := e.client.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Bucket: aws.String("plain"), Key: aws.String("k")})
	objSDKErr(t, err, 400, "InvalidRequest")
	_, err = e.client.PutObjectLegalHold(ctx, &awss3.PutObjectLegalHoldInput{Bucket: aws.String("plain"), Key: aws.String("k"),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOn}})
	objSDKErr(t, err, 400, "InvalidRequest")

	v := e.putObject("lock", "k", []byte("x"), storage.ObjectMeta{}).VersionID
	_, err = e.client.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Bucket: aws.String("lock"), Key: aws.String("k")})
	objSDKErr(t, err, 404, "NoSuchObjectLockConfiguration")

	in2h := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	in1h := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	in3h := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Millisecond)
	put := func(mode types.ObjectLockRetentionMode, until *time.Time, bypass bool, cl *awss3.Client) error {
		if cl == nil {
			cl = e.client
		}
		in := &awss3.PutObjectRetentionInput{Bucket: aws.String("lock"), Key: aws.String("k"), VersionId: aws.String(v),
			Retention: &types.ObjectLockRetention{Mode: mode, RetainUntilDate: until}}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		_, err := cl.PutObjectRetention(ctx, in)
		return err
	}
	if err := put(types.ObjectLockRetentionModeGovernance, &in2h, false, nil); err != nil {
		t.Fatalf("PutObjectRetention: %v", err)
	}
	ret, err := e.client.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Bucket: aws.String("lock"), Key: aws.String("k")})
	if err != nil || ret.Retention.Mode != types.ObjectLockRetentionModeGovernance || !ret.Retention.RetainUntilDate.Equal(in2h) {
		t.Fatalf("GetObjectRetention: %v %+v", err, ret)
	}
	// Shortening governance retention needs a bypass.
	objSDKErr(t, put(types.ObjectLockRetentionModeGovernance, &in1h, false, nil), 403, "AccessDenied")
	e.putPolicy("ret", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObjectRetention","s3:GetObjectRetention"],"Resource":"arn:aws:s3:::lock/*"}]}`)
	u := e.clientFor(e.createUser("retainer", "ret"))
	objSDKErr(t, put(types.ObjectLockRetentionModeGovernance, &in1h, true, u), 403, "AccessDenied")
	if err := put(types.ObjectLockRetentionModeGovernance, &in3h, false, u); err != nil {
		t.Errorf("extending governance: %v", err)
	}
	if err := put(types.ObjectLockRetentionModeGovernance, &in1h, true, nil); err != nil {
		t.Errorf("root bypass shortening: %v", err)
	}
	// Switch to compliance (allowed with bypass from governance), which can
	// then only be extended.
	if err := put(types.ObjectLockRetentionModeCompliance, &in2h, true, nil); err != nil {
		t.Fatalf("to compliance: %v", err)
	}
	objSDKErr(t, put(types.ObjectLockRetentionModeCompliance, &in1h, true, nil), 403, "AccessDenied")
	objSDKErr(t, put(types.ObjectLockRetentionModeGovernance, &in3h, true, nil), 403, "AccessDenied")
	if err := put(types.ObjectLockRetentionModeCompliance, &in3h, false, nil); err != nil {
		t.Errorf("extending compliance: %v", err)
	}
	// Removing compliance retention is impossible.
	objExpect(t, e.signed("PUT", "/lock/k?retention", []byte(`<Retention/>`), objHdr("X-Amz-Bypass-Governance-Retention", "true")), 403, "AccessDenied")

	// Validation.
	past := time.Now().Add(-time.Hour)
	objSDKErr(t, put(types.ObjectLockRetentionModeCompliance, &past, false, nil), 400, "InvalidArgument")
	objExpect(t, e.signed("PUT", "/lock/k?retention", []byte(`<Retention><Mode>FOREVER</Mode><RetainUntilDate>2100-01-01T00:00:00Z</RetainUntilDate></Retention>`), nil), 400, "MalformedXML")
	objExpect(t, e.signed("PUT", "/lock/k?retention", []byte(`<Retention><Mode>COMPLIANCE</Mode></Retention>`), nil), 400, "MalformedXML")
	objExpect(t, e.signed("PUT", "/lock/k?retention", []byte(`<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>tomorrow</RetainUntilDate></Retention>`), nil), 400, "InvalidArgument")

	// Governance retention can be removed with a bypass.
	v2 := e.putObject("lock", "g", []byte("x"), storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: in2h}).VersionID
	objExpect(t, e.signed("PUT", "/lock/g?retention", []byte(`<Retention/>`), nil), 403, "AccessDenied")
	objExpect(t, e.signed("PUT", "/lock/g?retention", []byte(`<Retention/>`), objHdr("X-Amz-Bypass-Governance-Retention", "true")), 200, "")
	if info := objGetInfo(t, e, "lock", "g", v2); info.RetentionMode != "" || !info.RetainUntil.IsZero() {
		t.Errorf("retention not removed: %+v", info.ObjectMeta)
	}
	// Raw response shape.
	e.putObject("lock", "r", []byte("x"), storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: time.Date(2100, 1, 2, 3, 4, 5, 0, time.UTC)})
	resp := e.signed("GET", "/lock/r?retention", nil, nil)
	if want := `<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>COMPLIANCE</Mode><RetainUntilDate>2100-01-02T03:04:05.000Z</RetainUntilDate></Retention>`; !strings.Contains(string(resp.Data), want) {
		t.Errorf("retention body %s", resp.Data)
	}
}

func TestObjectLegalHoldSDK(t *testing.T) {
	e := newTestEnv(t)
	objLockBucket(t, e, "lock")
	ctx := context.Background()
	e.putObject("lock", "k", []byte("x"), storage.ObjectMeta{})
	lh, err := e.client.GetObjectLegalHold(ctx, &awss3.GetObjectLegalHoldInput{Bucket: aws.String("lock"), Key: aws.String("k")})
	if err != nil || lh.LegalHold.Status != types.ObjectLockLegalHoldStatusOff {
		t.Fatalf("GetObjectLegalHold: %v %+v", err, lh)
	}
	if _, err := e.client.PutObjectLegalHold(ctx, &awss3.PutObjectLegalHoldInput{Bucket: aws.String("lock"), Key: aws.String("k"),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOn}}); err != nil {
		t.Fatalf("PutObjectLegalHold: %v", err)
	}
	lh, err = e.client.GetObjectLegalHold(ctx, &awss3.GetObjectLegalHoldInput{Bucket: aws.String("lock"), Key: aws.String("k")})
	if err != nil || lh.LegalHold.Status != types.ObjectLockLegalHoldStatusOn {
		t.Fatalf("GetObjectLegalHold ON: %v %+v", err, lh)
	}
	info := objGetInfo(t, e, "lock", "k", "")
	objExpect(t, e.signed("DELETE", "/lock/k?versionId="+info.VersionID, nil, objHdr("X-Amz-Bypass-Governance-Retention", "true")), 403, "AccessDenied")
	objExpect(t, e.signed("PUT", "/lock/k?legal-hold", []byte(`<LegalHold><Status>MAYBE</Status></LegalHold>`), nil), 400, "MalformedXML")
	objExpect(t, e.signed("PUT", "/lock/k?legal-hold", []byte(`<LegalHold><Status>OFF</Status></LegalHold>`), nil), 200, "")
	if r := e.signed("DELETE", "/lock/k?versionId="+info.VersionID, nil, nil); r.StatusCode != 204 {
		t.Errorf("delete after releasing hold: %d %s", r.StatusCode, r.Data)
	}
}

// --- GetObjectAttributes ---

func TestObjectAttributesSDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	data := []byte("attributes!")
	put, err := e.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader(string(data)),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256, StorageClass: types.StorageClassOnezoneIa})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.client.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesChecksum,
			types.ObjectAttributesObjectSize, types.ObjectAttributesStorageClass, types.ObjectAttributesObjectParts}})
	if err != nil {
		t.Fatalf("GetObjectAttributes: %v", err)
	}
	if aws.ToString(out.ETag) != strings.Trim(aws.ToString(put.ETag), `"`) || aws.ToInt64(out.ObjectSize) != int64(len(data)) ||
		out.StorageClass != types.StorageClassOnezoneIa || out.ObjectParts != nil || out.LastModified == nil {
		t.Errorf("attributes %+v", out)
	}
	if out.Checksum == nil || aws.ToString(out.Checksum.ChecksumSHA256) != computeChecksum(ChecksumSHA256, data) || out.Checksum.ChecksumType != types.ChecksumTypeFullObject {
		t.Errorf("checksum %+v", out.Checksum)
	}
	// Only the requested attributes are returned.
	out, err = e.client.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectSize}})
	if err != nil || out.ETag != nil || out.Checksum != nil || aws.ToInt64(out.ObjectSize) != int64(len(data)) {
		t.Errorf("size only: %v %+v", err, out)
	}

	// A multipart object with paged parts.
	sizes := []int64{4, 4, 3}
	parts := []map[string]string{{ChecksumCRC32: "AAAAAQ=="}, {ChecksumCRC32: "AAAAAg=="}, {ChecksumCRC32: "AAAAAw=="}}
	e.putObject("bkt", "mp", data, storage.ObjectMeta{ETag: "0123456789abcdef0123456789abcdef-3", PartSizes: sizes, PartChecksums: parts,
		Checksums: map[string]string{ChecksumCRC32: "AAAAAA==-3"}, ChecksumType: ChecksumTypeComposite})
	out, err = e.client.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("mp"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}, MaxParts: aws.Int32(2)})
	if err != nil {
		t.Fatal(err)
	}
	op := out.ObjectParts
	if op == nil || aws.ToInt32(op.TotalPartsCount) != 3 || !aws.ToBool(op.IsTruncated) || aws.ToString(op.NextPartNumberMarker) != "2" ||
		aws.ToInt32(op.MaxParts) != 2 || len(op.Parts) != 2 || aws.ToInt64(op.Parts[1].Size) != 4 || aws.ToString(op.Parts[1].ChecksumCRC32) != "AAAAAg==" {
		t.Fatalf("parts page 1: %+v", op)
	}
	out, err = e.client.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("mp"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}, PartNumberMarker: aws.String("2")})
	if err != nil || len(out.ObjectParts.Parts) != 1 || aws.ToInt32(out.ObjectParts.Parts[0].PartNumber) != 3 || aws.ToBool(out.ObjectParts.IsTruncated) {
		t.Fatalf("parts page 2: %v %+v", err, out.ObjectParts)
	}

	_, err = e.client.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("missing"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag}})
	objSDKErr(t, err, 404, "NoSuchKey")
	objExpect(t, e.signed("GET", "/bkt/k?attributes", nil, nil), 400, "InvalidArgument")
	objExpect(t, e.signed("GET", "/bkt/k?attributes", nil, objHdr("X-Amz-Object-Attributes", "ETag,Color")), 400, "InvalidArgument")
	objExpect(t, e.signed("GET", "/bkt/k?attributes", nil, objHdr("X-Amz-Object-Attributes", "ETag", "X-Amz-Max-Parts", "-1")), 400, "InvalidArgument")

	// Raw shape and headers in a versioned bucket, including delete markers.
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	v := e.putObject("bkt", "vk", data, storage.ObjectMeta{}).VersionID
	resp := e.signed("GET", "/bkt/vk?attributes", nil, objHdr("X-Amz-Object-Attributes", "ETag, ObjectSize"))
	objExpect(t, resp, 200, "")
	var doc struct {
		XMLName    xml.Name
		ETag       string
		ObjectSize int64
	}
	if err := xml.Unmarshal(resp.Data, &doc); err != nil || doc.XMLName.Local != "GetObjectAttributesResponse" || doc.ETag != md5Hex(data) || doc.ObjectSize != int64(len(data)) {
		t.Errorf("raw attributes %v %s", err, resp.Data)
	}
	if resp.Header.Get("X-Amz-Version-Id") != v || resp.Header.Get("Last-Modified") == "" {
		t.Errorf("headers %v", resp.Header)
	}
	dm := e.signed("DELETE", "/bkt/vk", nil, nil).Header.Get("X-Amz-Version-Id")
	resp = e.signed("GET", "/bkt/vk?attributes", nil, objHdr("X-Amz-Object-Attributes", "ETag"))
	objExpect(t, resp, 404, "NoSuchKey")
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("X-Amz-Version-Id") != dm {
		t.Errorf("DM headers %v", resp.Header)
	}
	objExpect(t, e.signed("GET", "/bkt/vk?attributes&versionId="+dm, nil, objHdr("X-Amz-Object-Attributes", "ETag")), 405, "MethodNotAllowed")
}

func TestParseObjectAttributes(t *testing.T) {
	got, err := parseObjectAttributes(http.Header{"X-Amz-Object-Attributes": {"etag, Checksum", "OBJECTSIZE"}})
	if err != nil || !got["ETag"] || !got["Checksum"] || !got["ObjectSize"] || len(got) != 3 {
		t.Errorf("%v %v", got, err)
	}
	if _, err := parseObjectAttributes(http.Header{}); !objIsCode(err, s3err.ErrInvalidArgument) {
		t.Errorf("missing: %v", err)
	}
	if _, err := parseObjectAttributes(http.Header{"X-Amz-Object-Attributes": {"Size"}}); !objIsCode(err, s3err.ErrInvalidArgument) {
		t.Errorf("unknown: %v", err)
	}
}

// objIsCode reports whether err is an S3 error with target's code.
func objIsCode(err error, target *s3err.Error) bool { return err != nil && codeOf(err) == target.Code }

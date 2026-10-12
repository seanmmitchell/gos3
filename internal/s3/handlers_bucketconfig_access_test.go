package s3

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- Bucket ACL ---

// bcGrants summarizes ACL grants as "<grantee>:<permission>" strings.
func bcGrants(grants []types.Grant) []string {
	var out []string
	for _, g := range grants {
		who := aws.ToString(g.Grantee.URI)
		if who == "" {
			who = "user:" + aws.ToString(g.Grantee.DisplayName)
		} else {
			who = who[strings.LastIndexByte(who, '/')+1:]
		}
		out = append(out, who+":"+string(g.Permission))
	}
	return out
}

func TestBucketAclSDK(t *testing.T) {
	e := bcEnv(t)
	requireAuthz(t, e)
	c := e.client
	getACL := func() string {
		t.Helper()
		out, err := c.GetBucketAcl(bcCtx, &awss3.GetBucketAclInput{Bucket: aws.String("cfg")})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.Owner.DisplayName) != "root" || aws.ToString(out.Owner.ID) != ownerFor("").ID {
			t.Fatalf("owner: %+v", out.Owner)
		}
		return strings.Join(bcGrants(out.Grants), ",")
	}
	if got := getACL(); got != "user:root:FULL_CONTROL" {
		t.Fatalf("default ACL: %s", got)
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 403, "AccessDenied")

	for _, tc := range []struct {
		acl  types.BucketCannedACL
		want string
	}{
		{types.BucketCannedACLPublicRead, "user:root:FULL_CONTROL,AllUsers:READ"},
		{types.BucketCannedACLPublicReadWrite, "user:root:FULL_CONTROL,AllUsers:READ,AllUsers:WRITE"},
		{types.BucketCannedACLAuthenticatedRead, "user:root:FULL_CONTROL,AuthenticatedUsers:READ"},
		{types.BucketCannedACLPrivate, "user:root:FULL_CONTROL"},
	} {
		if _, err := c.PutBucketAcl(bcCtx, &awss3.PutBucketAclInput{Bucket: aws.String("cfg"), ACL: tc.acl}); err != nil {
			t.Fatalf("%s: %v", tc.acl, err)
		}
		if got := getACL(); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.acl, got, tc.want)
		}
	}
	if _, err := e.backend.GetBucketConfig(bcCtx, "cfg", storage.ConfigACL); err == nil {
		t.Fatal("private ACL should remove the stored ACL")
	}

	// public-read takes effect for anonymous listing immediately.
	if _, err := c.PutBucketAcl(bcCtx, &awss3.PutBucketAclInput{Bucket: aws.String("cfg"), ACL: types.BucketCannedACLPublicRead}); err != nil {
		t.Fatal(err)
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 200, "")
	resp := e.signed(http.MethodGet, "/cfg?acl", nil, nil)
	bcNS(t, resp, "AccessControlPolicy")
	if !bytes.Contains(resp.Data, []byte(`xsi:type="Group"`)) {
		t.Fatalf("grantee type missing: %s", resp.Data)
	}

	// AccessControlPolicy body through the SDK.
	owner := ownerFor("")
	_, err := c.PutBucketAcl(bcCtx, &awss3.PutBucketAclInput{Bucket: aws.String("cfg"), AccessControlPolicy: &types.AccessControlPolicy{
		Owner: &types.Owner{ID: aws.String(owner.ID)},
		Grants: []types.Grant{
			{Grantee: &types.Grantee{Type: types.TypeCanonicalUser, ID: aws.String(owner.ID)}, Permission: types.PermissionFullControl},
			{Grantee: &types.Grantee{Type: types.TypeGroup, URI: aws.String(groupLogDelivery)}, Permission: types.PermissionWrite},
			{Grantee: &types.Grantee{Type: types.TypeGroup, URI: aws.String(groupLogDelivery)}, Permission: types.PermissionReadAcp},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := getACL(); got != "user:root:FULL_CONTROL,LogDelivery:WRITE,LogDelivery:READ_ACP" {
		t.Fatalf("log-delivery-write: %s", got)
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 403, "AccessDenied")
}

func TestBucketAclValidation(t *testing.T) {
	e := bcEnv(t)
	owner := ownerFor("")
	acp := func(grants ...string) []byte {
		return []byte(`<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>` + owner.ID +
			`</ID></Owner><AccessControlList>` + strings.Join(grants, "") + `</AccessControlList></AccessControlPolicy>`)
	}
	group := func(uri, perm string) string {
		return `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>` + uri +
			`</URI></Grantee><Permission>` + perm + `</Permission></Grant>`
	}
	user := func(id, perm string) string {
		return `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>` + id +
			`</ID></Grantee><Permission>` + perm + `</Permission></Grant>`
	}
	for _, tc := range []struct {
		name   string
		hdr    http.Header
		body   []byte
		status int
		code   string
		stored string // expected stored canned ACL ("" = none) when status is 200
	}{
		{"canned", bcHdr("x-amz-acl", "public-read"), nil, 200, "", "public-read"},
		{"canned log delivery", bcHdr("x-amz-acl", "log-delivery-write"), nil, 200, "", "log-delivery-write"},
		{"bad canned", bcHdr("x-amz-acl", "public"), nil, 400, "InvalidArgument", ""},
		{"canned with body", bcHdr("x-amz-acl", "private"), acp(), 400, "UnexpectedContent", ""},
		{"grant header", bcHdr("x-amz-grant-read", `uri="http://acs.amazonaws.com/groups/global/AllUsers"`), nil, 501, "NotImplemented", ""},
		{"no body", nil, nil, 400, "MissingRequestBodyError", ""},
		{"malformed", nil, []byte("<AccessControlPolicy><AccessControlList>"), 400, "MalformedACLError", ""},
		{"owner only", nil, acp(user(owner.ID, "FULL_CONTROL")), 200, "", ""},
		{"all users read", nil, acp(user(owner.ID, "FULL_CONTROL"), group(groupAllUsers, "READ")), 200, "", "public-read"},
		{"all users rw", nil, acp(group(groupAllUsers, "WRITE"), group(groupAllUsers, "READ")), 200, "", "public-read-write"},
		{"auth read", nil, acp(group(groupAuthenticatedUsers, "READ")), 200, "", "authenticated-read"},
		{"other user", nil, acp(user("abc", "READ")), 501, "NotImplemented", ""},
		{"all users write only", nil, acp(group(groupAllUsers, "WRITE")), 501, "NotImplemented", ""},
		{"all users full", nil, acp(group(groupAllUsers, "FULL_CONTROL")), 501, "NotImplemented", ""},
		{"bad permission", nil, acp(group(groupAllUsers, "EVERYTHING")), 400, "MalformedACLError", ""},
		{"bad group", nil, acp(group("http://example.com/group", "READ")), 400, "InvalidArgument", ""},
		{"email", nil, acp(`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="AmazonCustomerByEmail"><EmailAddress>a@b.c</EmailAddress></Grantee><Permission>READ</Permission></Grant>`), 501, "NotImplemented", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = e.backend.DeleteBucketConfig(bcCtx, "cfg", storage.ConfigACL)
			bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", tc.body, tc.hdr), tc.status, tc.code)
			if tc.status != 200 {
				return
			}
			raw, _ := e.backend.GetBucketConfig(bcCtx, "cfg", storage.ConfigACL)
			if string(raw) != tc.stored {
				t.Fatalf("stored %q, want %q", raw, tc.stored)
			}
		})
	}
	if resp := e.signed(http.MethodDelete, "/cfg?acl", nil, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE ?acl: %d", resp.StatusCode)
	}
}

func TestBucketAclOwnershipAndPublicAccessBlock(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	// Ownership BucketOwnerEnforced: ACLs disabled.
	if _, err := c.PutBucketOwnershipControls(bcCtx, &awss3.PutBucketOwnershipControlsInput{Bucket: aws.String("cfg"),
		OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced}}}}); err != nil {
		t.Fatal(err)
	}
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "public-read")), 400, "AccessControlListNotSupported")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "log-delivery-write")), 400, "AccessControlListNotSupported")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "private")), 200, "")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "bucket-owner-full-control")), 200, "")
	// A stored public ACL is reported as owner-only while ACLs are disabled.
	e.putBucketConfig("cfg", storage.ConfigACL, "public-read")
	out, err := c.GetBucketAcl(bcCtx, &awss3.GetBucketAclInput{Bucket: aws.String("cfg")})
	if err != nil || len(out.Grants) != 1 {
		t.Fatalf("ACL with BucketOwnerEnforced: %v %v", err, bcGrants(out.Grants))
	}
	_ = e.backend.DeleteBucketConfig(bcCtx, "cfg", storage.ConfigACL)
	if _, err := c.DeleteBucketOwnershipControls(bcCtx, &awss3.DeleteBucketOwnershipControlsInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}

	// BlockPublicAcls rejects public ACLs only.
	if _, err := c.PutPublicAccessBlock(bcCtx, &awss3.PutPublicAccessBlockInput{Bucket: aws.String("cfg"),
		PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true)}}); err != nil {
		t.Fatal(err)
	}
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "public-read")), 403, "AccessDenied")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "authenticated-read")), 403, "AccessDenied")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?acl", nil, bcHdr("x-amz-acl", "log-delivery-write")), 200, "")
}

// --- Ownership controls ---

func TestBucketOwnershipControlsSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketOwnershipControls(bcCtx, &awss3.GetBucketOwnershipControlsInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "OwnershipControlsNotFoundError")
	for _, o := range []types.ObjectOwnership{types.ObjectOwnershipObjectWriter, types.ObjectOwnershipBucketOwnerPreferred, types.ObjectOwnershipBucketOwnerEnforced} {
		if _, err := c.PutBucketOwnershipControls(bcCtx, &awss3.PutBucketOwnershipControlsInput{Bucket: aws.String("cfg"),
			OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: o}}}}); err != nil {
			t.Fatalf("%s: %v", o, err)
		}
		out, err := c.GetBucketOwnershipControls(bcCtx, &awss3.GetBucketOwnershipControlsInput{Bucket: aws.String("cfg")})
		if err != nil || len(out.OwnershipControls.Rules) != 1 || out.OwnershipControls.Rules[0].ObjectOwnership != o {
			t.Fatalf("%s: %v %+v", o, err, out)
		}
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?ownershipControls", nil, nil), "OwnershipControls")
	bcExpect(t, e.signed(http.MethodDelete, "/cfg?ownershipControls", nil, nil), 204, "")
	_, err = c.GetBucketOwnershipControls(bcCtx, &awss3.GetBucketOwnershipControlsInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "OwnershipControlsNotFoundError")

	// The document stored by CreateBucket (x-amz-object-ownership) is read back.
	if _, err := c.CreateBucket(bcCtx, &awss3.CreateBucketInput{Bucket: aws.String("owned"), ObjectOwnership: types.ObjectOwnershipObjectWriter}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketOwnershipControls(bcCtx, &awss3.GetBucketOwnershipControlsInput{Bucket: aws.String("owned")})
	if err != nil || out.OwnershipControls.Rules[0].ObjectOwnership != types.ObjectOwnershipObjectWriter {
		t.Fatalf("CreateBucket ownership: %v %+v", err, out)
	}
}

func TestBucketOwnershipControlsValidation(t *testing.T) {
	e := bcEnv(t)
	doc := func(rules ...string) []byte {
		var b strings.Builder
		b.WriteString("<OwnershipControls>")
		for _, r := range rules {
			b.WriteString("<Rule><ObjectOwnership>" + r + "</ObjectOwnership></Rule>")
		}
		b.WriteString("</OwnershipControls>")
		return []byte(b.String())
	}
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc(), nil), 400, "MalformedXML")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("ObjectWriter", "ObjectWriter"), nil), 400, "MalformedXML")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("Owner"), nil), 400, "MalformedXML")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("objectwriter"), nil), 400, "MalformedXML")

	// BucketOwnerEnforced is refused while the bucket ACL grants others access.
	for _, acl := range []string{"public-read", "authenticated-read", "log-delivery-write"} {
		e.putBucketConfig("cfg", storage.ConfigACL, acl)
		bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("BucketOwnerEnforced"), nil), 400, "InvalidBucketAclWithObjectOwnership")
	}
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("BucketOwnerPreferred"), nil), 200, "")
	e.putBucketConfig("cfg", storage.ConfigACL, "bucket-owner-full-control")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?ownershipControls", doc("BucketOwnerEnforced"), nil), 200, "")
}

// --- Public access block ---

func TestPublicAccessBlockSDK(t *testing.T) {
	e := bcEnv(t)
	requireAuthz(t, e)
	c := e.client
	_, err := c.GetPublicAccessBlock(bcCtx, &awss3.GetPublicAccessBlockInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchPublicAccessBlockConfiguration")

	// A public-read bucket is listable anonymously until IgnorePublicAcls.
	e.putBucketConfig("cfg", storage.ConfigACL, "public-read")
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 200, "")
	in := &types.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true),
		BlockPublicPolicy: aws.Bool(false), RestrictPublicBuckets: aws.Bool(true)}
	if _, err := c.PutPublicAccessBlock(bcCtx, &awss3.PutPublicAccessBlockInput{Bucket: aws.String("cfg"), PublicAccessBlockConfiguration: in}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetPublicAccessBlock(bcCtx, &awss3.GetPublicAccessBlockInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	p := out.PublicAccessBlockConfiguration
	if !aws.ToBool(p.BlockPublicAcls) || !aws.ToBool(p.IgnorePublicAcls) || aws.ToBool(p.BlockPublicPolicy) || !aws.ToBool(p.RestrictPublicBuckets) {
		t.Fatalf("PAB: %+v", p)
	}
	resp := e.signed(http.MethodGet, "/cfg?publicAccessBlock", nil, nil)
	bcNS(t, resp, "PublicAccessBlockConfiguration")
	for _, el := range []string{"<BlockPublicAcls>true</BlockPublicAcls>", "<BlockPublicPolicy>false</BlockPublicPolicy>"} {
		if !bytes.Contains(resp.Data, []byte(el)) {
			t.Fatalf("missing %s: %s", el, resp.Data)
		}
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 403, "AccessDenied")

	if _, err := c.DeletePublicAccessBlock(bcCtx, &awss3.DeletePublicAccessBlockInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	bcExpect(t, e.anon(http.MethodGet, "/cfg", nil, nil), 200, "")
	_, err = c.GetPublicAccessBlock(bcCtx, &awss3.GetPublicAccessBlockInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchPublicAccessBlockConfiguration")

	// Omitted fields are false; invalid booleans are malformed.
	bcExpect(t, e.signed(http.MethodPut, "/cfg?publicAccessBlock", []byte("<PublicAccessBlockConfiguration/>"), nil), 200, "")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?publicAccessBlock",
		[]byte("<PublicAccessBlockConfiguration><BlockPublicAcls>yes</BlockPublicAcls></PublicAccessBlockConfiguration>"), nil), 400, "MalformedXML")
}

// --- Object lock configuration ---

func TestObjectLockConfigurationSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetObjectLockConfiguration(bcCtx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "ObjectLockConfigurationNotFoundError")

	enable := &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
	_, err = c.PutObjectLockConfiguration(bcCtx, &awss3.PutObjectLockConfigurationInput{Bucket: aws.String("cfg"), ObjectLockConfiguration: enable})
	bcSDKErr(t, err, "InvalidBucketState")
	bcVersioned(t, e, "cfg")
	if _, err := c.PutObjectLockConfiguration(bcCtx, &awss3.PutObjectLockConfigurationInput{Bucket: aws.String("cfg"), ObjectLockConfiguration: enable}); err != nil {
		t.Fatal(err)
	}
	if bi, _ := e.backend.GetBucket(bcCtx, "cfg"); !bi.ObjectLockEnabled {
		t.Fatal("object lock not enabled in the backend")
	}
	out, err := c.GetObjectLockConfiguration(bcCtx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil || out.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled || out.ObjectLockConfiguration.Rule != nil {
		t.Fatalf("after enable: %v %+v", err, out.ObjectLockConfiguration)
	}

	// Default retention.
	for _, dr := range []*types.DefaultRetention{
		{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(3)},
		{Mode: types.ObjectLockRetentionModeCompliance, Years: aws.Int32(1)},
	} {
		if _, err := c.PutObjectLockConfiguration(bcCtx, &awss3.PutObjectLockConfigurationInput{Bucket: aws.String("cfg"),
			ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled,
				Rule: &types.ObjectLockRule{DefaultRetention: dr}}}); err != nil {
			t.Fatal(err)
		}
		out, err := c.GetObjectLockConfiguration(bcCtx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("cfg")})
		if err != nil {
			t.Fatal(err)
		}
		got := out.ObjectLockConfiguration.Rule.DefaultRetention
		if got.Mode != dr.Mode || aws.ToInt32(got.Days) != aws.ToInt32(dr.Days) || aws.ToInt32(got.Years) != aws.ToInt32(dr.Years) {
			t.Fatalf("retention: %+v, want %+v", got, dr)
		}
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?object-lock", nil, nil), "ObjectLockConfiguration")

	// Removing the rule keeps object lock enabled.
	if _, err := c.PutObjectLockConfiguration(bcCtx, &awss3.PutObjectLockConfigurationInput{Bucket: aws.String("cfg"), ObjectLockConfiguration: enable}); err != nil {
		t.Fatal(err)
	}
	out, err = c.GetObjectLockConfiguration(bcCtx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil || out.ObjectLockConfiguration.Rule != nil {
		t.Fatalf("after rule removal: %v %+v", err, out.ObjectLockConfiguration)
	}
	// Versioning can no longer be suspended.
	_, err = c.PutBucketVersioning(bcCtx, &awss3.PutBucketVersioningInput{Bucket: aws.String("cfg"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	bcSDKErr(t, err, "InvalidBucketState")

	// A bucket created with object lock reports it without a stored document.
	if _, err := c.CreateBucket(bcCtx, &awss3.CreateBucketInput{Bucket: aws.String("locked"), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	out, err = c.GetObjectLockConfiguration(bcCtx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("locked")})
	if err != nil || out.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		t.Fatalf("created locked: %v %+v", err, out)
	}
}

func TestObjectLockConfigurationValidation(t *testing.T) {
	e := bcEnv(t)
	bcVersioned(t, e, "cfg")
	doc := func(enabled, rule string) []byte {
		s := "<ObjectLockConfiguration>"
		if enabled != "" {
			s += "<ObjectLockEnabled>" + enabled + "</ObjectLockEnabled>"
		}
		return []byte(s + rule + "</ObjectLockConfiguration>")
	}
	dr := func(inner string) string { return "<Rule><DefaultRetention>" + inner + "</DefaultRetention></Rule>" }
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"missing enabled", doc("", ""), 400, "MalformedXML"},
		{"disabled", doc("Disabled", ""), 400, "MalformedXML"},
		{"empty rule", doc("Enabled", "<Rule/>"), 400, "MalformedXML"},
		{"bad mode", doc("Enabled", dr("<Mode>STRICT</Mode><Days>1</Days>")), 400, "MalformedXML"},
		{"no period", doc("Enabled", dr("<Mode>GOVERNANCE</Mode>")), 400, "MalformedXML"},
		{"both periods", doc("Enabled", dr("<Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years>")), 400, "MalformedXML"},
		{"zero days", doc("Enabled", dr("<Mode>GOVERNANCE</Mode><Days>0</Days>")), 400, "InvalidRetentionPeriod"},
		{"negative years", doc("Enabled", dr("<Mode>COMPLIANCE</Mode><Years>-1</Years>")), 400, "InvalidRetentionPeriod"},
		{"too many days", doc("Enabled", dr("<Mode>COMPLIANCE</Mode><Days>36501</Days>")), 400, "InvalidRetentionPeriod"},
		{"too many years", doc("Enabled", dr("<Mode>COMPLIANCE</Mode><Years>101</Years>")), 400, "InvalidRetentionPeriod"},
		{"max days", doc("Enabled", dr("<Mode>COMPLIANCE</Mode><Days>36500</Days>")), 200, ""},
		{"max years", doc("Enabled", dr("<Mode>GOVERNANCE</Mode><Years>100</Years>")), 200, ""},
		{"overflow", doc("Enabled", dr("<Mode>GOVERNANCE</Mode><Days>99999999999999999999</Days>")), 400, "MalformedXML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?object-lock", tc.body, nil), tc.status, tc.code)
		})
	}
	// A stored default retention is applied by PutObject (s3-object).
	raw, err := e.backend.GetBucketConfig(bcCtx, "cfg", storage.ConfigObjectLock)
	if err != nil || !bytes.Contains(raw, []byte("<Years>100</Years>")) {
		t.Fatalf("stored: %s %v", raw, err)
	}
}

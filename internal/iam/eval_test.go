package iam

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func mustPolicy(t testing.TB, s string) PolicyDocument {
	t.Helper()
	d, err := ParsePolicy([]byte(s))
	if err != nil {
		t.Fatalf("ParsePolicy: %v\n%s", err, s)
	}
	return *d
}

func mustBucketPolicy(t testing.TB, bucket, s string) *PolicyDocument {
	t.Helper()
	d, err := ParseBucketPolicy([]byte(s), bucket)
	if err != nil {
		t.Fatalf("ParseBucketPolicy: %v\n%s", err, s)
	}
	return d
}

// newEvalManager builds a manager with a set of users/policies used by the
// evaluation tests.
func newEvalManager(t testing.TB) *Manager {
	t.Helper()
	m, err := Open(Options{RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	policies := map[string]string{
		"denyput": `{"Statement":[{"Sid":"NoPut","Effect":"Deny","Action":"s3:PutObject","Resource":"arn:aws:s3:::locked/*"}]}`,
		"home": `{"Version":"2012-10-17","Statement":[
			{"Sid":"Own","Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::home/${aws:username}/*"},
			{"Sid":"List","Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::home","Condition":{"StringLike":{"s3:prefix":["${aws:username}/*","${aws:username}"]}}}]}`,
		"notaction":   `{"Statement":[{"Effect":"Allow","NotAction":["s3:Delete*","admin:*"],"Resource":"*"}]}`,
		"notresource": `{"Statement":[{"Effect":"Allow","Action":"s3:*","NotResource":["arn:aws:s3:::secret","arn:aws:s3:::secret/*"]}]}`,
		"denynotsecure": `{"Statement":[
			{"Effect":"Allow","Action":"s3:*","Resource":"*"},
			{"Sid":"TLS","Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`,
		"office":     `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::*/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","2001:db8::/32"]}}}]}`,
		"usersadmin": `{"Statement":[{"Effect":"Allow","Action":["admin:ListUsers","admin:Get*"],"Resource":"arn:gos3:admin:::*"}]}`,
		"qmark":      `{"Statement":[{"Effect":"Allow","Action":"s3:GetObjec?","Resource":"arn:aws:s3:::b/file-?.txt"}]}`,
	}
	for name, doc := range policies {
		if _, err := m.PutPolicy(ctx, name, "", mustPolicy(t, doc)); err != nil {
			t.Fatalf("PutPolicy %s: %v", name, err)
		}
	}
	users := map[string][]string{
		"alice":   {PolicyReadWrite},
		"bob":     {PolicyReadOnly},
		"carol":   {PolicyReadWrite, "denyput"},
		"dave":    {PolicyDiagnostics},
		"eve":     {"home"},
		"nact":    {"notaction"},
		"nres":    {"notresource"},
		"tls":     {"denynotsecure"},
		"office":  {"office"},
		"uadmin":  {"usersadmin"},
		"admin":   {PolicyAdmin},
		"writer":  {PolicyWriteOnly},
		"nobody":  {},
		"qm":      {"qmark"},
		"blocked": {PolicyAdmin},
	}
	for name, pols := range users {
		if _, err := m.CreateUser(ctx, UserSpec{Name: name, Policies: pols, Disabled: name == "blocked"}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func user(name string) auth.Identity {
	return auth.Identity{User: name, AccessKey: "AK" + strings.ToUpper(name)}
}

var (
	anon = auth.Anonymous
	root = auth.Identity{User: auth.RootUser, IsRoot: true, AccessKey: "ROOTKEY"}
)

type authzCase struct {
	name       string
	id         auth.Identity
	action     string
	bucket     string
	key        string
	bp         *PolicyDocument
	acl        string
	cond       map[string][]string
	want       bool
	wantDeny   bool
	wantReason string // substring
}

func runAuthzCases(t *testing.T, m *Manager, cases []authzCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := m.Authorize(context.Background(), AuthzRequest{
				Identity:     tc.id,
				Action:       tc.action,
				Bucket:       tc.bucket,
				Key:          tc.key,
				BucketPolicy: tc.bp,
				ACL:          tc.acl,
				Conditions:   tc.cond,
			})
			if d.Allowed != tc.want || d.ExplicitDeny != tc.wantDeny {
				t.Fatalf("decision = %+v, want allowed=%v explicitDeny=%v", d, tc.want, tc.wantDeny)
			}
			if d.Allowed && d.ExplicitDeny {
				t.Fatalf("allowed and explicit deny: %+v", d)
			}
			if tc.wantReason != "" && !strings.Contains(d.Reason, tc.wantReason) {
				t.Fatalf("reason %q does not contain %q", d.Reason, tc.wantReason)
			}
			if d.Reason == "" {
				t.Fatal("empty reason")
			}
		})
	}
}

func TestAuthorizeIdentityPolicies(t *testing.T) {
	m := newEvalManager(t)
	runAuthzCases(t, m, []authzCase{
		{name: "root always", id: root, action: "admin:UpdateConfig", want: true, wantReason: "root"},
		{name: "root despite bucket deny", id: root, action: "s3:GetObject", bucket: "b", key: "k",
			bp: mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`), want: true},
		{name: "readwrite get", id: user("alice"), action: "s3:GetObject", bucket: "b", key: "k", want: true, wantReason: "allowed by policy readwrite"},
		{name: "readwrite put", id: user("alice"), action: "s3:PutObject", bucket: "b", key: "a/b/c", want: true},
		{name: "readwrite list buckets", id: user("alice"), action: "s3:ListAllMyBuckets", want: true},
		{name: "readwrite bucket level", id: user("alice"), action: "s3:CreateBucket", bucket: "new", want: true},
		{name: "readwrite no admin", id: user("alice"), action: "admin:ListUsers", want: false, wantReason: "no matching allow"},
		{name: "action case insensitive", id: user("alice"), action: "S3:GETOBJECT", bucket: "b", key: "k", want: true},
		{name: "readonly get", id: user("bob"), action: "s3:GetObject", bucket: "b", key: "k", want: true},
		{name: "readonly list", id: user("bob"), action: "s3:ListBucket", bucket: "b", want: true},
		{name: "readonly list all", id: user("bob"), action: "s3:ListAllMyBuckets", want: true},
		{name: "readonly put denied", id: user("bob"), action: "s3:PutObject", bucket: "b", key: "k", want: false},
		{name: "readonly delete denied", id: user("bob"), action: "s3:DeleteObject", bucket: "b", key: "k", want: false},
		{name: "writeonly put", id: user("writer"), action: "s3:PutObject", bucket: "b", key: "k", want: true},
		{name: "writeonly abort", id: user("writer"), action: "s3:AbortMultipartUpload", bucket: "b", key: "k", want: true},
		{name: "writeonly list uploads", id: user("writer"), action: "s3:ListBucketMultipartUploads", bucket: "b", want: true},
		{name: "writeonly get denied", id: user("writer"), action: "s3:GetObject", bucket: "b", key: "k", want: false},
		{name: "deny overrides allow", id: user("carol"), action: "s3:PutObject", bucket: "locked", key: "x", want: false, wantDeny: true, wantReason: "explicit deny in policy denyput statement Sid=NoPut"},
		{name: "deny scoped", id: user("carol"), action: "s3:PutObject", bucket: "open", key: "x", want: true},
		{name: "deny other action", id: user("carol"), action: "s3:GetObject", bucket: "locked", key: "x", want: true},
		{name: "diagnostics info", id: user("dave"), action: "admin:ServerInfo", want: true},
		{name: "diagnostics stats", id: user("dave"), action: "admin:GetStats", want: true},
		{name: "diagnostics discovery", id: user("dave"), action: "admin:getdiscovery", want: true},
		{name: "diagnostics no users", id: user("dave"), action: "admin:ListUsers", want: false},
		{name: "diagnostics no s3", id: user("dave"), action: "s3:ListAllMyBuckets", want: false},
		{name: "admin s3", id: user("admin"), action: "s3:DeleteBucket", bucket: "b", want: true},
		{name: "admin admin", id: user("admin"), action: "admin:UpdateConfig", want: true},
		{name: "admin resource arn", id: user("uadmin"), action: "admin:ListUsers", want: true},
		{name: "admin resource arn get", id: user("uadmin"), action: "admin:GetUser", want: true},
		{name: "admin resource arn other", id: user("uadmin"), action: "admin:CreateUser", want: false},
		{name: "admin arn not s3", id: user("uadmin"), action: "s3:ListAllMyBuckets", want: false},
		{name: "no policies", id: user("nobody"), action: "s3:GetObject", bucket: "b", key: "k", want: false, wantReason: "no matching allow"},
		{name: "unknown user", id: user("ghost"), action: "s3:GetObject", bucket: "b", key: "k", want: false},
		{name: "disabled user", id: user("blocked"), action: "s3:GetObject", bucket: "b", key: "k", want: false, wantReason: "disabled"},
		{name: "missing action", id: user("admin"), action: "", want: false, wantReason: "missing action"},
		{name: "anonymous no identity policies", id: anon, action: "s3:GetObject", bucket: "b", key: "k", want: false},
		{name: "anonymous named user ignored", id: auth.Identity{IsAnonymous: true, User: "admin"}, action: "s3:GetObject", bucket: "b", key: "k", want: false},
		{name: "resolve by access key", id: auth.Identity{AccessKey: "unknown"}, action: "s3:GetObject", bucket: "b", key: "k", want: false},

		// NotAction / NotResource
		{name: "notaction allows get", id: user("nact"), action: "s3:GetObject", bucket: "b", key: "k", want: true},
		{name: "notaction excludes delete", id: user("nact"), action: "s3:DeleteObject", bucket: "b", key: "k", want: false},
		{name: "notaction excludes admin", id: user("nact"), action: "admin:ListUsers", want: false},
		{name: "notresource allows other", id: user("nres"), action: "s3:GetObject", bucket: "public", key: "k", want: true},
		{name: "notresource excludes object", id: user("nres"), action: "s3:GetObject", bucket: "secret", key: "k", want: false},
		{name: "notresource excludes bucket", id: user("nres"), action: "s3:ListBucket", bucket: "secret", want: false},
		{name: "notresource service level", id: user("nres"), action: "s3:ListAllMyBuckets", want: true},

		// ? wildcard
		{name: "qmark match", id: user("qm"), action: "s3:GetObject", bucket: "b", key: "file-1.txt", want: true},
		{name: "qmark no match", id: user("qm"), action: "s3:GetObject", bucket: "b", key: "file-10.txt", want: false},

		// Variables
		{name: "var own prefix", id: user("eve"), action: "s3:PutObject", bucket: "home", key: "eve/doc.txt", want: true},
		{name: "var other prefix", id: user("eve"), action: "s3:PutObject", bucket: "home", key: "alice/doc.txt", want: false},
		{name: "var list own", id: user("eve"), action: "s3:ListBucket", bucket: "home", cond: map[string][]string{"s3:prefix": {"eve/"}}, want: true},
		{name: "var list own exact", id: user("eve"), action: "s3:ListBucket", bucket: "home", cond: map[string][]string{"s3:prefix": {"eve"}}, want: true},
		{name: "var list other", id: user("eve"), action: "s3:ListBucket", bucket: "home", cond: map[string][]string{"s3:prefix": {"alice/"}}, want: false},
		{name: "var list no prefix", id: user("eve"), action: "s3:ListBucket", bucket: "home", want: false},
		{name: "var caller overrides username key", id: user("eve"), action: "s3:PutObject", bucket: "home", key: "eve/x",
			cond: map[string][]string{"aws:username": {"mallory"}}, want: false},

		// Conditions on identity policies
		{name: "tls deny when insecure", id: user("tls"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SecureTransport": {"false"}}, want: false, wantDeny: true, wantReason: "Sid=TLS"},
		{name: "tls allowed when secure", id: user("tls"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SecureTransport": {"true"}}, want: true},
		{name: "tls missing key no deny", id: user("tls"), action: "s3:GetObject", bucket: "b", key: "k", want: true},
		{name: "ip v4 in range", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SourceIp": {"10.1.2.3"}}, want: true},
		{name: "ip v4 mapped", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SourceIp": {"::ffff:10.1.2.3"}}, want: true},
		{name: "ip v6 in range", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SourceIp": {"2001:db8::1"}}, want: true},
		{name: "ip out of range", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"aws:SourceIp": {"192.168.0.1"}}, want: false},
		{name: "ip missing", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", want: false},
		{name: "ip key case insensitive", id: user("office"), action: "s3:GetObject", bucket: "b", key: "k", cond: map[string][]string{"AWS:SOURCEIP": {"10.0.0.1"}}, want: true},
	})
}

func TestAuthorizeBucketPolicyAndACL(t *testing.T) {
	m := newEvalManager(t)
	publicRead := mustBucketPolicy(t, "pub", `{"Version":"2012-10-17","Statement":[
		{"Sid":"PublicRead","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	shared := mustBucketPolicy(t, "shared", `{"Statement":[
		{"Sid":"Bob","Effect":"Allow","Principal":{"AWS":["bob"]},"Action":["s3:PutObject","s3:GetObject"],"Resource":"arn:aws:s3:::shared/*"},
		{"Sid":"Key","Effect":"Allow","Principal":{"AWS":"AKNOBODY"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"},
		{"Sid":"Name","Effect":"Allow","Principal":{"AWS":"writer"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/w/*"},
		{"Sid":"CanonicalNobody","Effect":"Allow","Principal":{"CanonicalUser":"nobody"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::shared"},
		{"Sid":"Root","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::1:root"},"Action":"s3:*","Resource":"arn:aws:s3:::shared/*"}]}`)
	denyAlice := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"NoAlice","Effect":"Deny","Principal":{"AWS":"alice"},"Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}]}`)
	notPrincipal := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"OnlyAlice","Effect":"Deny","NotPrincipal":{"AWS":"alice"},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
	ipRestricted := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"Net","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`)
	denyInsecureAll := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"TLS","Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}},
		{"Sid":"Read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	notActionBP := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"AllButDelete","Effect":"Allow","Principal":"*","NotAction":"s3:Delete*","NotResource":"arn:aws:s3:::b/private/*"}]}`)

	runAuthzCases(t, m, []authzCase{
		{name: "anon allowed by bucket policy", id: anon, action: "s3:GetObject", bucket: "pub", key: "img.png", bp: publicRead, want: true, wantReason: "allowed by bucket policy statement Sid=PublicRead"},
		{name: "anon other action", id: anon, action: "s3:PutObject", bucket: "pub", key: "img.png", bp: publicRead, want: false},
		{name: "anon bucket level", id: anon, action: "s3:ListBucket", bucket: "pub", bp: publicRead, want: false},
		{name: "user also allowed by star", id: user("nobody"), action: "s3:GetObject", bucket: "pub", key: "x", bp: publicRead, want: true},
		{name: "named user principal", id: user("bob"), action: "s3:PutObject", bucket: "shared", key: "x", bp: shared, want: true, wantReason: "Sid=Bob"},
		{name: "named user principal other user", id: user("eve"), action: "s3:PutObject", bucket: "shared", key: "x", bp: shared, want: false},
		{name: "access key principal", id: user("nobody"), action: "s3:GetObject", bucket: "shared", key: "x", bp: shared, want: true, wantReason: "Sid=Key"},
		{name: "name principal", id: user("writer"), action: "s3:GetObject", bucket: "shared", key: "w/x", bp: shared, want: true},
		{name: "name principal wrong resource", id: user("writer"), action: "s3:GetObject", bucket: "shared", key: "x", bp: shared, want: false},
		{name: "canonical user principal", id: user("nobody"), action: "s3:ListBucket", bucket: "shared", bp: shared, want: true},
		{name: "anon does not match named", id: anon, action: "s3:GetObject", bucket: "shared", key: "x", bp: shared, want: false},
		{name: "bucket deny overrides identity allow", id: user("alice"), action: "s3:DeleteObject", bucket: "b", key: "k", bp: denyAlice, want: false, wantDeny: true, wantReason: "explicit deny in bucket policy statement Sid=NoAlice"},
		{name: "bucket deny other user", id: user("admin"), action: "s3:DeleteObject", bucket: "b", key: "k", bp: denyAlice, want: true},
		{name: "notprincipal excludes alice", id: user("alice"), action: "s3:GetObject", bucket: "b", key: "k", bp: notPrincipal, want: true},
		{name: "notprincipal denies others", id: user("admin"), action: "s3:GetObject", bucket: "b", key: "k", bp: notPrincipal, want: false, wantDeny: true},
		{name: "notprincipal denies anon", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: notPrincipal, acl: "public-read", want: false, wantDeny: true},
		{name: "bp not ip outside", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: ipRestricted, cond: map[string][]string{"aws:SourceIp": {"8.8.8.8"}}, want: true},
		{name: "bp not ip inside", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: ipRestricted, cond: map[string][]string{"aws:SourceIp": {"10.9.9.9"}}, want: false},
		{name: "bp not ip missing", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: ipRestricted, want: true},
		{name: "bp deny insecure", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: denyInsecureAll, cond: map[string][]string{"aws:SecureTransport": {"false"}}, want: false, wantDeny: true},
		{name: "bp deny insecure user", id: user("admin"), action: "s3:PutObject", bucket: "b", key: "k", bp: denyInsecureAll, cond: map[string][]string{"aws:SecureTransport": {"false"}}, want: false, wantDeny: true},
		{name: "bp secure read", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: denyInsecureAll, cond: map[string][]string{"aws:SecureTransport": {"true"}}, want: true},
		{name: "bp notaction allows put", id: anon, action: "s3:PutObject", bucket: "b", key: "k", bp: notActionBP, want: true},
		{name: "bp notaction blocks delete", id: anon, action: "s3:DeleteObject", bucket: "b", key: "k", bp: notActionBP, want: false},
		{name: "bp notresource blocks private", id: anon, action: "s3:GetObject", bucket: "b", key: "private/k", bp: notActionBP, want: false},
		{name: "bp notresource bucket", id: anon, action: "s3:ListBucket", bucket: "b", bp: notActionBP, want: true},

		// Canned ACLs
		{name: "acl public-read get", id: anon, action: "s3:GetObject", bucket: "b", key: "k", acl: "public-read", want: true, wantReason: "canned ACL public-read"},
		{name: "acl public-read version", id: anon, action: "s3:GetObjectVersion", bucket: "b", key: "k", acl: "public-read", want: true},
		{name: "acl public-read list", id: anon, action: "s3:ListBucket", bucket: "b", acl: "public-read", want: true},
		{name: "acl public-read list versions", id: anon, action: "s3:ListBucketVersions", bucket: "b", acl: "public-read", want: true},
		{name: "acl public-read location", id: anon, action: "s3:GetBucketLocation", bucket: "b", acl: "public-read", want: true},
		{name: "acl public-read put denied", id: anon, action: "s3:PutObject", bucket: "b", key: "k", acl: "public-read", want: false},
		{name: "acl public-read acl denied", id: anon, action: "s3:GetObjectAcl", bucket: "b", key: "k", acl: "public-read", want: false},
		{name: "acl public-read-write put", id: anon, action: "s3:PutObject", bucket: "b", key: "k", acl: "public-read-write", want: true},
		{name: "acl public-read-write delete", id: anon, action: "s3:DeleteObject", bucket: "b", key: "k", acl: "public-read-write", want: true},
		{name: "acl public-read-write delete version", id: anon, action: "s3:DeleteObjectVersion", bucket: "b", key: "k", acl: "public-read-write", want: false},
		{name: "acl public-read-write mpu", id: anon, action: "s3:ListMultipartUploadParts", bucket: "b", key: "k", acl: "public-read-write", want: false},
		{name: "acl public-read-write uploads", id: anon, action: "s3:ListBucketMultipartUploads", bucket: "b", acl: "Public-Read-Write", want: true},
		{name: "acl public-read-write abort", id: anon, action: "s3:AbortMultipartUpload", bucket: "b", key: "k", acl: "public-read-write", want: false},
		{name: "acl public-read-write no policy put", id: anon, action: "s3:PutBucketPolicy", bucket: "b", acl: "public-read-write", want: false},
		{name: "acl authenticated anon", id: anon, action: "s3:GetObject", bucket: "b", key: "k", acl: "authenticated-read", want: false},
		{name: "acl authenticated user", id: user("nobody"), action: "s3:GetObject", bucket: "b", key: "k", acl: "authenticated-read", want: true},
		{name: "acl authenticated user write", id: user("nobody"), action: "s3:PutObject", bucket: "b", key: "k", acl: "authenticated-read", want: false},
		{name: "acl private", id: anon, action: "s3:GetObject", bucket: "b", key: "k", acl: "private", want: false},
		{name: "acl unknown", id: anon, action: "s3:GetObject", bucket: "b", key: "k", acl: "bucket-owner-read", want: false},
		{name: "acl does not override identity deny", id: user("carol"), action: "s3:PutObject", bucket: "locked", key: "k", acl: "public-read-write", want: false, wantDeny: true},
		{name: "acl does not override bucket deny", id: user("alice"), action: "s3:DeleteObject", bucket: "b", key: "k", bp: denyAlice, acl: "public-read-write", want: false, wantDeny: true},
		{name: "disabled user no acl", id: user("blocked"), action: "s3:GetObject", bucket: "b", key: "k", acl: "public-read", want: false},
	})
}

func TestAuthorizeBucketPolicyRootPrincipal(t *testing.T) {
	// Root is always allowed, even if the bucket policy denies it.
	m := newEvalManager(t)
	bp := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam::1:root"},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
	d := m.Authorize(context.Background(), AuthzRequest{Identity: root, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: bp})
	if !d.Allowed {
		t.Fatalf("root denied: %+v", d)
	}
	// In the narrow reading the account principal matches only the root
	// identity; in the broad one (Deny statements) every authenticated
	// identity.
	c := newEvalCtx(&AuthzRequest{Identity: root}, fixedNow)
	for _, broad := range []bool{false, true} {
		if !c.principalMatches(&Principal{AWS: StringOrSlice{"arn:aws:iam::123:root"}}, broad) {
			t.Fatal("root ARN did not match root")
		}
		if !c.principalMatches(&Principal{AWS: StringOrSlice{"123456789012"}}, broad) {
			t.Fatal("account id did not match root")
		}
	}
	c = newEvalCtx(&AuthzRequest{Identity: user("alice")}, fixedNow)
	if c.principalMatches(&Principal{AWS: StringOrSlice{"arn:aws:iam::123:root", "123456789012", "arn:aws:iam::1:user/bob", ""}}, false) {
		t.Fatal("root principals matched a user (narrow)")
	}
	for _, v := range []string{"arn:aws:iam::123:root", "123456789012"} {
		if !c.principalMatches(&Principal{AWS: StringOrSlice{v}}, true) {
			t.Fatalf("account principal %s did not match a user (broad)", v)
		}
	}
	anonCtx := newEvalCtx(&AuthzRequest{Identity: auth.Anonymous}, fixedNow)
	if anonCtx.principalMatches(&Principal{AWS: StringOrSlice{"arn:aws:iam::123:root", "123456789012"}}, true) {
		t.Fatal("account principal matched anonymous")
	}
	for _, arn := range []string{
		"arn:aws:iam::1:user/alice",
		"arn:aws:iam::123456789012:user/team/alice",
		"arn:aws-us-gov:iam::1:user/alice",
	} {
		if c.principalMatches(&Principal{AWS: StringOrSlice{arn}}, false) {
			t.Fatalf("unsupported user ARN matched: %s", arn)
		}
	}
	if c.principalMatches(&Principal{AWS: StringOrSlice{"arn:aws:iam::1:group/alice", "arn:aws:s3:::alice", "arn:short"}}, true) {
		t.Fatal("non-user ARNs matched")
	}
	if c.principalMatches(nil, true) {
		t.Fatal("nil principal matched")
	}
	if !c.principalMatches(&Principal{AWS: StringOrSlice{"*"}}, false) {
		t.Fatal("explicit * entry did not match")
	}
}

// TestAuthorizeAccountPrincipal: a Deny naming the account principal
// applies to every IAM user (as on AWS); an Allow naming it delegates to
// identity policies and grants users nothing by itself; NotPrincipal reads
// the account principal the way that grants less.
func TestAuthorizeAccountPrincipal(t *testing.T) {
	m := newEvalManager(t)
	ctx := context.Background()
	alice := user("alice")
	for _, acct := range []string{`"arn:aws:iam::123456789012:root"`, `"123456789012"`} {
		deny := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Deny","Principal":{"AWS":`+acct+`},"Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]},
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
		if d := m.Authorize(ctx, AuthzRequest{Identity: alice, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: deny}); d.Allowed || !d.ExplicitDeny {
			t.Fatalf("%s: Deny on account principal did not deny alice: %+v", acct, d)
		}
		if d := m.Authorize(ctx, AuthzRequest{Identity: auth.Anonymous, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: deny}); !d.Allowed {
			t.Fatalf("%s: account Deny applied to anonymous: %+v", acct, d)
		}
		allow := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":`+acct+`},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
		if d := m.Authorize(ctx, AuthzRequest{Identity: user("eve"), Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: allow}); d.Allowed {
			t.Fatalf("%s: Allow on account principal granted alice: %+v", acct, d)
		}
		// Allow NotPrincipal account: only anonymous callers get it.
		notAllow := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Allow","NotPrincipal":{"AWS":`+acct+`},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
		if d := m.Authorize(ctx, AuthzRequest{Identity: user("eve"), Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: notAllow}); d.Allowed {
			t.Fatalf("%s: Allow NotPrincipal account granted alice: %+v", acct, d)
		}
		if !IsPublicPolicy(notAllow) {
			t.Fatalf("%s: Allow NotPrincipal account not public", acct)
		}
		// Deny NotPrincipal account: an IAM user is not exempted by the
		// account entry alone, only by an entry naming the user.
		notDeny := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Deny","NotPrincipal":{"AWS":[`+acct+`]},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"},
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
		if d := m.Authorize(ctx, AuthzRequest{Identity: alice, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: notDeny}); d.Allowed {
			t.Fatalf("%s: Deny NotPrincipal account exempted alice: %+v", acct, d)
		}
		notDenyAlice := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Deny","NotPrincipal":{"AWS":[`+acct+`,"alice"]},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"},
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
		if d := m.Authorize(ctx, AuthzRequest{Identity: alice, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: notDenyAlice}); !d.Allowed {
			t.Fatalf("%s: Deny NotPrincipal [account, alice] denied alice: %+v", acct, d)
		}
	}
}

func TestAuthorizeServiceLevelResources(t *testing.T) {
	m := newEvalManager(t)
	ctx := context.Background()
	docs := map[string]string{
		"svc-star":     `{"Statement":[{"Effect":"Allow","Action":["s3:ListAllMyBuckets","admin:ServerInfo"],"Resource":"*"}]}`,
		"svc-s3arn":    `{"Statement":[{"Effect":"Allow","Action":["s3:ListAllMyBuckets","admin:ServerInfo"],"Resource":"arn:aws:s3:::*"}]}`,
		"svc-adminarn": `{"Statement":[{"Effect":"Allow","Action":["s3:ListAllMyBuckets","admin:ServerInfo"],"Resource":"arn:gos3:admin:::*"}]}`,
		"svc-bucket":   `{"Statement":[{"Effect":"Allow","Action":["s3:ListAllMyBuckets","admin:ServerInfo"],"Resource":"arn:aws:s3:::b*"}]}`,
		"svc-qmark":    `{"Statement":[{"Effect":"Allow","Action":["s3:ListAllMyBuckets"],"Resource":"arn:aws:s3:::?"}]}`,
	}
	for name, doc := range docs {
		if _, err := m.PutPolicy(ctx, name, "", mustPolicy(t, doc)); err != nil {
			t.Fatal(err)
		}
		if _, err := m.CreateUser(ctx, UserSpec{Name: name, Policies: []string{name}}); err != nil {
			t.Fatal(err)
		}
	}
	runAuthzCases(t, m, []authzCase{
		{name: "star s3", id: user("svc-star"), action: "s3:ListAllMyBuckets", want: true},
		{name: "star admin", id: user("svc-star"), action: "admin:ServerInfo", want: true},
		{name: "s3arn s3", id: user("svc-s3arn"), action: "s3:ListAllMyBuckets", want: true},
		{name: "s3arn admin", id: user("svc-s3arn"), action: "admin:ServerInfo", want: false},
		{name: "adminarn s3", id: user("svc-adminarn"), action: "s3:ListAllMyBuckets", want: false},
		{name: "adminarn admin", id: user("svc-adminarn"), action: "admin:ServerInfo", want: true},
		{name: "bucket pattern s3", id: user("svc-bucket"), action: "s3:ListAllMyBuckets", want: false},
		{name: "bucket pattern admin", id: user("svc-bucket"), action: "admin:ServerInfo", want: false},
		{name: "qmark does not cover service", id: user("svc-qmark"), action: "s3:ListAllMyBuckets", want: false},
	})
}

func TestAuthorizeDeniesOnlyAfterAllStatements(t *testing.T) {
	// An Allow in the first policy must not short-circuit a Deny that comes
	// later (identity or bucket policy).
	m := newEvalManager(t)
	ctx := context.Background()
	if _, err := m.PutPolicy(ctx, "latedeny", "", mustPolicy(t, `{"Statement":[{"Effect":"Deny","Action":"s3:GetObject","Resource":"*"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "late", Policies: []string{PolicyAdmin, "latedeny"}}); err != nil {
		t.Fatal(err)
	}
	d := m.Authorize(ctx, AuthzRequest{Identity: user("late"), Action: "s3:GetObject", Bucket: "b", Key: "k"})
	if d.Allowed || !d.ExplicitDeny {
		t.Fatalf("late deny ignored: %+v", d)
	}
	bp := mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"},{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	d = m.Authorize(ctx, AuthzRequest{Identity: anon, Action: "s3:PutObject", Bucket: "b", Key: "k", BucketPolicy: bp})
	if d.Allowed || !d.ExplicitDeny || !strings.Contains(d.Reason, "statement #2") {
		t.Fatalf("bucket late deny ignored: %+v", d)
	}
}

func TestAuthorizeIgnoresInvalidEffects(t *testing.T) {
	// Documents constructed in Go (bypassing parsing) with bogus fields must
	// not grant access.
	m := newEvalManager(t)
	bp := &PolicyDocument{Statement: []Statement{
		{Effect: "allow", Principal: &Principal{Any: true}, Action: StringOrSlice{"*"}, Resource: StringOrSlice{"*"}},
		{Effect: "Allow", Action: StringOrSlice{"*"}, Resource: StringOrSlice{"*"}},       // no principal
		{Effect: "Allow", Principal: &Principal{Any: true}, Resource: StringOrSlice{"*"}}, // no action
		{Effect: "Allow", Principal: &Principal{Any: true}, Action: StringOrSlice{"*"}},   // no resource
		{Effect: "Allow", Principal: &Principal{Any: true}, Action: StringOrSlice{"*"}, Resource: StringOrSlice{"*"}, // bad op
			Condition: map[string]map[string]StringOrSlice{"Bogus": {"aws:x": {"y"}}}},
	}}
	d := m.Authorize(context.Background(), AuthzRequest{Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: bp})
	if d.Allowed {
		t.Fatalf("bogus policy granted access: %+v", d)
	}
	// An unknown operator in a Deny statement fails closed (deny applies).
	bp = &PolicyDocument{Statement: []Statement{
		{Effect: "Deny", Principal: &Principal{Any: true}, Action: StringOrSlice{"*"}, Resource: StringOrSlice{"*"},
			Condition: map[string]map[string]StringOrSlice{"Bogus": {"aws:x": {"y"}}}},
	}}
	d = m.Authorize(context.Background(), AuthzRequest{Identity: user("admin"), Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: bp})
	if d.Allowed || !d.ExplicitDeny {
		t.Fatalf("deny with unknown operator ignored: %+v", d)
	}
}

func TestCannedACLAllows(t *testing.T) {
	if cannedACLAllows("", anon, "s3:GetObject") || cannedACLAllows("public-read", anon, "") {
		t.Fatal("empty ACL/action granted")
	}
	if !IsPublicACL("public-read") || !IsPublicACL("PUBLIC-READ-WRITE") || IsPublicACL("authenticated-read") || IsPublicACL("") {
		t.Fatal("IsPublicACL wrong")
	}
}

func TestARNHelpers(t *testing.T) {
	if BucketARN("b") != "arn:aws:s3:::b" || ObjectARN("b", "a/k") != "arn:aws:s3:::b/a/k" {
		t.Fatal("ARN helpers wrong")
	}
}

func BenchmarkAuthorize(b *testing.B) {
	m := newEvalManager(b)
	bp := mustBucketPolicy(b, "b", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*","Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`)
	req := AuthzRequest{Identity: user("carol"), Action: "s3:GetObject", Bucket: "b", Key: "some/key.txt", BucketPolicy: bp,
		Conditions: map[string][]string{"aws:SecureTransport": {"true"}}}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if !m.Authorize(ctx, req).Allowed {
			b.Fatal("denied")
		}
	}
}

// TestServiceAndFederatedPrincipals: policies ported from AWS with Service
// or Federated principals are accepted; those statements match no gos3
// identity (a NotPrincipal naming only them therefore applies to everyone).
func TestServiceAndFederatedPrincipals(t *testing.T) {
	m := newEvalManager(t)
	ctx := context.Background()
	bp := mustBucketPolicy(t, "b", `{"Version":"2012-10-17","Statement":[
		{"Sid":"OAC","Effect":"Allow","Principal":{"Service":"cloudfront.amazonaws.com"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"AWS:SourceArn":"arn:aws:cloudfront::1:distribution/X"}}},
		{"Sid":"Fed","Effect":"Allow","Principal":{"Federated":["cognito-identity.amazonaws.com","*.example"]},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"},
		{"Sid":"DenyAllButLogs","Effect":"Deny","NotPrincipal":{"Service":"logging.s3.amazonaws.com"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::b/logs/*"}]}`)
	for _, id := range []auth.Identity{anon, user("eve")} {
		if d := m.Authorize(ctx, AuthzRequest{Identity: id, Action: "s3:GetObject", Bucket: "b", Key: "k", BucketPolicy: bp}); d.Allowed {
			t.Fatalf("%+v: service/federated principal matched: %+v", id, d)
		}
		if d := m.Authorize(ctx, AuthzRequest{Identity: id, Action: "s3:PutObject", Bucket: "b", Key: "logs/x", BucketPolicy: bp}); !d.ExplicitDeny {
			t.Fatalf("%+v: Deny NotPrincipal Service did not apply: %+v", id, d)
		}
	}
	if IsPublicPolicy(bp) {
		t.Fatal("service/federated principals reported public")
	}
	if !IsPublicPolicy(mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Allow","NotPrincipal":{"Service":"x.amazonaws.com"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)) {
		t.Fatal("Allow NotPrincipal Service not public")
	}
}

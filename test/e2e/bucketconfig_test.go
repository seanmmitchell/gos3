package e2e

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestTagging(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newVersionedBucket(t)
			v1 := e.put(t, b, "k", []byte("1"), func(in *s3.PutObjectInput) { in.Tagging = aws.String("a=1") }).VersionId
			v2 := e.put(t, b, "k", []byte("2")).VersionId

			// Latest version has no tags; v1 keeps its own.
			g, err := c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "GetObjectTagging")
			if len(g.TagSet) != 0 || aws.ToString(g.VersionId) != aws.ToString(v2) {
				t.Errorf("latest tags %v version %q", tagMap(g.TagSet), aws.ToString(g.VersionId))
			}
			g, err = c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: v1})
			must(t, err, "GetObjectTagging v1")
			if !reflect.DeepEqual(tagMap(g.TagSet), map[string]string{"a": "1"}) {
				t.Errorf("v1 tags %v", tagMap(g.TagSet))
			}

			tags := []s3types.Tag{{Key: aws.String("env"), Value: aws.String("prod")}, {Key: aws.String("unicode ü"), Value: aws.String("välue/+-=.:_@")}}
			p, err := c.PutObjectTagging(ctx(t), &s3.PutObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k"), Tagging: &s3types.Tagging{TagSet: tags}})
			must(t, err, "PutObjectTagging")
			if aws.ToString(p.VersionId) != aws.ToString(v2) {
				t.Errorf("PutObjectTagging version %q", aws.ToString(p.VersionId))
			}
			g, err = c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "GetObjectTagging")
			if !reflect.DeepEqual(tagMap(g.TagSet), tagMap(tags)) {
				t.Errorf("tags %v", tagMap(g.TagSet))
			}
			h, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "HeadObject")
			if aws.ToString(h.ETag) == "" {
				t.Error("tagging changed object")
			}
			_, err = c.DeleteObjectTagging(ctx(t), &s3.DeleteObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "DeleteObjectTagging")
			g, err = c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "GetObjectTagging")
			if len(g.TagSet) != 0 {
				t.Errorf("tags after delete %v", tagMap(g.TagSet))
			}
			_, err = c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("missing")})
			wantErr(t, err, "NoSuchKey", http.StatusNotFound)
			_, err = c.PutObjectTagging(ctx(t), &s3.PutObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("k"),
				Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("aws:reserved"), Value: aws.String("x")}}}})
			wantErr(t, err, "InvalidTag", 400)

			// Bucket tagging.
			_, err = c.GetBucketTagging(ctx(t), &s3.GetBucketTaggingInput{Bucket: aws.String(b)})
			wantErr(t, err, "NoSuchTagSet", http.StatusNotFound)
			_, err = c.PutBucketTagging(ctx(t), &s3.PutBucketTaggingInput{Bucket: aws.String(b), Tagging: &s3types.Tagging{TagSet: tags}})
			must(t, err, "PutBucketTagging")
			bt, err := c.GetBucketTagging(ctx(t), &s3.GetBucketTaggingInput{Bucket: aws.String(b)})
			must(t, err, "GetBucketTagging")
			if !reflect.DeepEqual(tagMap(bt.TagSet), tagMap(tags)) {
				t.Errorf("bucket tags %v", tagMap(bt.TagSet))
			}
			_, err = c.DeleteBucketTagging(ctx(t), &s3.DeleteBucketTaggingInput{Bucket: aws.String(b)})
			must(t, err, "DeleteBucketTagging")
			_, err = c.GetBucketTagging(ctx(t), &s3.GetBucketTaggingInput{Bucket: aws.String(b)})
			wantErr(t, err, "NoSuchTagSet", http.StatusNotFound)
		})
	}
}

func TestACLs(t *testing.T) {
	e := fsEnv
	c := e.root()
	b := e.newBucket(t)
	e.put(t, b, "private", []byte("private"))
	e.put(t, b, "public", []byte("public"), func(in *s3.PutObjectInput) { in.ACL = s3types.ObjectCannedACLPublicRead })

	// Object ACL public-read: anonymous GET works for that object only.
	if r := e.anon(t, http.MethodGet, "/"+b+"/public", nil, nil); r.status != 200 || string(r.body) != "public" {
		t.Errorf("anonymous GET public-read object: %d %s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/private", nil, nil); r.status != 403 || r.code() != "AccessDenied" {
		t.Errorf("anonymous GET private object: %d %s", r.status, r.code())
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"?list-type=2", nil, nil); r.status != 403 {
		t.Errorf("anonymous list private bucket: %d", r.status)
	}
	acl, err := c.GetObjectAcl(ctx(t), &s3.GetObjectAclInput{Bucket: aws.String(b), Key: aws.String("public")})
	must(t, err, "GetObjectAcl")
	if !hasAllUsersGrant(acl.Grants, s3types.PermissionRead) || acl.Owner == nil || aws.ToString(acl.Owner.ID) == "" {
		t.Errorf("public object ACL grants %+v owner %+v", acl.Grants, acl.Owner)
	}
	acl, err = c.GetObjectAcl(ctx(t), &s3.GetObjectAclInput{Bucket: aws.String(b), Key: aws.String("private")})
	must(t, err, "GetObjectAcl private")
	if hasAllUsersGrant(acl.Grants, s3types.PermissionRead) || len(acl.Grants) != 1 || acl.Grants[0].Permission != s3types.PermissionFullControl {
		t.Errorf("private object ACL grants %+v", acl.Grants)
	}

	// PutObjectAcl toggles it.
	_, err = c.PutObjectAcl(ctx(t), &s3.PutObjectAclInput{Bucket: aws.String(b), Key: aws.String("private"), ACL: s3types.ObjectCannedACLPublicRead})
	must(t, err, "PutObjectAcl")
	if r := e.anon(t, http.MethodGet, "/"+b+"/private", nil, nil); r.status != 200 {
		t.Errorf("after PutObjectAcl public-read: %d", r.status)
	}
	_, err = c.PutObjectAcl(ctx(t), &s3.PutObjectAclInput{Bucket: aws.String(b), Key: aws.String("private"), ACL: s3types.ObjectCannedACLPrivate})
	must(t, err, "PutObjectAcl private")
	if r := e.anon(t, http.MethodGet, "/"+b+"/private", nil, nil); r.status != 403 {
		t.Errorf("after PutObjectAcl private: %d", r.status)
	}

	// Bucket ACL public-read allows anonymous listing (not object reads).
	_, err = c.PutBucketAcl(ctx(t), &s3.PutBucketAclInput{Bucket: aws.String(b), ACL: s3types.BucketCannedACLPublicRead})
	must(t, err, "PutBucketAcl")
	ba, err := c.GetBucketAcl(ctx(t), &s3.GetBucketAclInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketAcl")
	if !hasAllUsersGrant(ba.Grants, s3types.PermissionRead) {
		t.Errorf("bucket ACL grants %+v", ba.Grants)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"?list-type=2", nil, nil); r.status != 200 || !strings.Contains(string(r.body), "<Key>private</Key>") {
		t.Errorf("anonymous list public-read bucket: %d %s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/private", nil, nil); r.status != 403 {
		t.Errorf("bucket ACL must not grant object reads: %d", r.status)
	}
	if r := e.anon(t, http.MethodPut, "/"+b+"/anon-write", []byte("x"), nil); r.status != 403 {
		t.Errorf("anonymous PUT to public-read bucket: %d", r.status)
	}
	// public-read-write lets anonymous users write.
	_, err = c.PutBucketAcl(ctx(t), &s3.PutBucketAclInput{Bucket: aws.String(b), ACL: s3types.BucketCannedACLPublicReadWrite})
	must(t, err, "PutBucketAcl public-read-write")
	if r := e.anon(t, http.MethodPut, "/"+b+"/anon-write", []byte("x"), nil); r.status != 200 {
		t.Errorf("anonymous PUT to public-read-write bucket: %d %s", r.status, r.body)
	}
	_, err = c.PutBucketAcl(ctx(t), &s3.PutBucketAclInput{Bucket: aws.String(b), ACL: s3types.BucketCannedACLPrivate})
	must(t, err, "PutBucketAcl private")
	if r := e.anon(t, http.MethodGet, "/"+b+"?list-type=2", nil, nil); r.status != 403 {
		t.Errorf("anonymous list after private: %d", r.status)
	}

	// BucketOwnerEnforced disables ACLs.
	_, err = c.PutBucketOwnershipControls(ctx(t), &s3.PutBucketOwnershipControlsInput{Bucket: aws.String(b),
		OwnershipControls: &s3types.OwnershipControls{Rules: []s3types.OwnershipControlsRule{{ObjectOwnership: s3types.ObjectOwnershipBucketOwnerEnforced}}}})
	must(t, err, "PutBucketOwnershipControls")
	_, err = c.PutObjectAcl(ctx(t), &s3.PutObjectAclInput{Bucket: aws.String(b), Key: aws.String("private"), ACL: s3types.ObjectCannedACLPublicRead})
	wantErr(t, err, "AccessControlListNotSupported", 400)
	_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("acl"), Body: stringsReader("x"), ACL: s3types.ObjectCannedACLPublicRead})
	wantErr(t, err, "AccessControlListNotSupported", 400)
	// The public object's ACL no longer applies.
	if r := e.anon(t, http.MethodGet, "/"+b+"/public", nil, nil); r.status != 403 {
		t.Errorf("anonymous GET with ACLs disabled: %d", r.status)
	}
}

func hasAllUsersGrant(gs []s3types.Grant, p s3types.Permission) bool {
	for _, g := range gs {
		if g.Grantee != nil && g.Grantee.Type == s3types.TypeGroup && aws.ToString(g.Grantee.URI) == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == p {
			return true
		}
	}
	return false
}

func TestBucketPolicyAnonymous(t *testing.T) {
	e := fsEnv
	c := e.root()
	b := e.newBucket(t)
	e.put(t, b, "public/file", []byte("hello"))
	e.put(t, b, "public/secret", []byte("no"))
	e.put(t, b, "private/file", []byte("no"))

	_, err := c.GetBucketPolicy(ctx(t), &s3.GetBucketPolicyInput{Bucket: aws.String(b)})
	wantErr(t, err, "NoSuchBucketPolicy", http.StatusNotFound)

	policy := `{
	  "Version": "2012-10-17",
	  "Statement": [
	    {"Sid": "PublicRead", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::` + b + `/public/*"},
	    {"Sid": "PublicList", "Effect": "Allow", "Principal": {"AWS": "*"}, "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::` + b + `",
	     "Condition": {"StringLike": {"s3:prefix": "public/*"}}},
	    {"Sid": "DenySecret", "Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::` + b + `/public/secret"}
	  ]
	}`
	_, err = c.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(policy)})
	must(t, err, "PutBucketPolicy")
	gp, err := c.GetBucketPolicy(ctx(t), &s3.GetBucketPolicyInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketPolicy")
	var got, want any
	must(t, json.Unmarshal([]byte(aws.ToString(gp.Policy)), &got), "decode stored policy")
	must(t, json.Unmarshal([]byte(policy), &want), "decode policy")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored policy differs:\n%s", aws.ToString(gp.Policy))
	}
	st, err := c.GetBucketPolicyStatus(ctx(t), &s3.GetBucketPolicyStatusInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketPolicyStatus")
	if !aws.ToBool(st.PolicyStatus.IsPublic) {
		t.Errorf("policy not reported public")
	}

	if r := e.anon(t, http.MethodGet, "/"+b+"/public/file", nil, nil); r.status != 200 || string(r.body) != "hello" {
		t.Errorf("anonymous GET allowed object: %d %s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/public/secret", nil, nil); r.status != 403 {
		t.Errorf("explicit deny: %d", r.status)
	}
	// Explicit deny in a bucket policy also applies to IAM users with full
	// access (root is never denied).
	user := e.createUser(t, userName("polfull"), "readwrite")
	if r := e.signed(t, user, http.MethodGet, "/"+b+"/public/secret", nil, nil); r.status != 403 {
		t.Errorf("explicit deny for IAM user: %d", r.status)
	}
	if r := e.signed(t, user, http.MethodGet, "/"+b+"/private/file", nil, nil); r.status != 200 {
		t.Errorf("IAM user readwrite: %d", r.status)
	}
	if r := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/public/secret", nil, nil); r.status != 200 {
		t.Errorf("root denied by bucket policy: %d", r.status)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/private/file", nil, nil); r.status != 403 {
		t.Errorf("anonymous GET outside policy: %d", r.status)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"?list-type=2&prefix=public/", nil, nil); r.status != 200 || !strings.Contains(string(r.body), "public/file") {
		t.Errorf("anonymous list with allowed prefix: %d %s", r.status, r.body)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"?list-type=2&prefix=private/", nil, nil); r.status != 403 {
		t.Errorf("anonymous list with other prefix: %d", r.status)
	}
	if r := e.anon(t, http.MethodPut, "/"+b+"/public/new", []byte("x"), nil); r.status != 403 {
		t.Errorf("anonymous PUT: %d", r.status)
	}
	// Anonymous SDK client.
	anon := e.s3Client(rootCreds, func(o *s3.Options) { o.Credentials = aws.AnonymousCredentials{} })
	out, err := anon.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("public/file")})
	must(t, err, "anonymous SDK GetObject")
	readAll(t, out.Body)

	// Malformed policy.
	_, err = c.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(`{"Statement": "nope"}`)})
	wantErr(t, err, "MalformedPolicy", 400)
	_, err = c.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other-bucket/*"}]}`)})
	wantErr(t, err, "MalformedPolicy", 400)

	// Block public access ignores/refuses public policies.
	_, err = c.PutPublicAccessBlock(ctx(t), &s3.PutPublicAccessBlockInput{Bucket: aws.String(b), PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
		BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true)}})
	must(t, err, "PutPublicAccessBlock")
	pab, err := c.GetPublicAccessBlock(ctx(t), &s3.GetPublicAccessBlockInput{Bucket: aws.String(b)})
	must(t, err, "GetPublicAccessBlock")
	if c := pab.PublicAccessBlockConfiguration; !aws.ToBool(c.BlockPublicAcls) || !aws.ToBool(c.IgnorePublicAcls) || !aws.ToBool(c.BlockPublicPolicy) || !aws.ToBool(c.RestrictPublicBuckets) {
		t.Errorf("public access block %+v", c)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/public/file", nil, nil); r.status != 403 {
		t.Errorf("RestrictPublicBuckets: anonymous GET %d", r.status)
	}
	_, err = c.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(policy)})
	wantErr(t, err, "AccessDenied", http.StatusForbidden)
	_, err = c.DeletePublicAccessBlock(ctx(t), &s3.DeletePublicAccessBlockInput{Bucket: aws.String(b)})
	must(t, err, "DeletePublicAccessBlock")
	_, err = c.GetPublicAccessBlock(ctx(t), &s3.GetPublicAccessBlockInput{Bucket: aws.String(b)})
	wantErr(t, err, "NoSuchPublicAccessBlockConfiguration", http.StatusNotFound)
	if r := e.anon(t, http.MethodGet, "/"+b+"/public/file", nil, nil); r.status != 200 {
		t.Errorf("after removing the block: anonymous GET %d", r.status)
	}

	_, err = c.DeleteBucketPolicy(ctx(t), &s3.DeleteBucketPolicyInput{Bucket: aws.String(b)})
	must(t, err, "DeleteBucketPolicy")
	if r := e.anon(t, http.MethodGet, "/"+b+"/public/file", nil, nil); r.status != 403 {
		t.Errorf("after DeleteBucketPolicy: anonymous GET %d", r.status)
	}
}

func TestBucketCORS(t *testing.T) {
	e := fsEnv
	c := e.root()
	b := e.newBucket(t)
	e.put(t, b, "obj", []byte("cors"))
	_, err := c.GetBucketCors(ctx(t), &s3.GetBucketCorsInput{Bucket: aws.String(b)})
	wantErr(t, err, "NoSuchCORSConfiguration", http.StatusNotFound)

	rules := []s3types.CORSRule{
		{ID: aws.String("app"), AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET", "PUT", "HEAD"},
			AllowedHeaders: []string{"x-amz-*", "content-type"}, ExposeHeaders: []string{"ETag", "x-amz-version-id"}, MaxAgeSeconds: aws.Int32(600)},
		{AllowedOrigins: []string{"http://*.wild.test"}, AllowedMethods: []string{"GET"}},
	}
	_, err = c.PutBucketCors(ctx(t), &s3.PutBucketCorsInput{Bucket: aws.String(b), CORSConfiguration: &s3types.CORSConfiguration{CORSRules: rules}})
	must(t, err, "PutBucketCors")
	g, err := c.GetBucketCors(ctx(t), &s3.GetBucketCorsInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketCors")
	if len(g.CORSRules) != 2 || aws.ToString(g.CORSRules[0].ID) != "app" || !reflect.DeepEqual(g.CORSRules[0].AllowedMethods, rules[0].AllowedMethods) ||
		!reflect.DeepEqual(g.CORSRules[0].ExposeHeaders, rules[0].ExposeHeaders) || aws.ToInt32(g.CORSRules[0].MaxAgeSeconds) != 600 {
		t.Errorf("CORS round trip: %+v", g.CORSRules)
	}

	// Raw preflights.
	pre := func(origin, method, headers string) response {
		h := map[string]string{"Origin": origin, "Access-Control-Request-Method": method}
		if headers != "" {
			h["Access-Control-Request-Headers"] = headers
		}
		return e.anon(t, http.MethodOptions, "/"+b+"/obj", nil, h)
	}
	r := pre("https://app.example.com", "PUT", "Content-Type, X-Amz-Meta-Foo")
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		!strings.Contains(r.header.Get("Access-Control-Allow-Methods"), "PUT") || r.header.Get("Access-Control-Max-Age") != "600" ||
		!strings.Contains(strings.ToLower(r.header.Get("Access-Control-Allow-Headers")), "x-amz-meta-foo") {
		t.Errorf("allowed preflight: %d %v", r.status, r.header)
	}
	if !strings.Contains(r.header.Get("Vary"), "Origin") {
		t.Errorf("preflight Vary %q", r.header.Get("Vary"))
	}
	r = pre("https://evil.example.com", "GET", "")
	if r.status != 403 || r.code() != "AccessForbidden" || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed origin: %d %s %v", r.status, r.code(), r.header)
	}
	r = pre("https://app.example.com", "DELETE", "")
	if r.status != 403 {
		t.Errorf("disallowed method: %d", r.status)
	}
	r = pre("https://app.example.com", "PUT", "x-forbidden-header")
	if r.status != 403 {
		t.Errorf("disallowed header: %d", r.status)
	}
	r = pre("http://sub.wild.test", "GET", "")
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "http://sub.wild.test" {
		t.Errorf("wildcard origin: %d %v", r.status, r.header)
	}
	// Actual request carries CORS headers (even when anonymous and denied the
	// object it still gets them for a signed request).
	resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/obj", nil, map[string]string{"Origin": "https://app.example.com"})
	if resp.status != 200 || resp.header.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		!strings.Contains(resp.header.Get("Access-Control-Expose-Headers"), "ETag") {
		t.Errorf("actual CORS request: %d %v", resp.status, resp.header)
	}
	resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"/obj", nil, map[string]string{"Origin": "https://evil.example.com"})
	if resp.status != 200 || resp.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("actual request from other origin: %d %v", resp.status, resp.header)
	}

	_, err = c.PutBucketCors(ctx(t), &s3.PutBucketCorsInput{Bucket: aws.String(b), CORSConfiguration: &s3types.CORSConfiguration{
		CORSRules: []s3types.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"PATCH"}}}}})
	wantErr(t, err, "InvalidRequest", 400)
	_, err = c.DeleteBucketCors(ctx(t), &s3.DeleteBucketCorsInput{Bucket: aws.String(b)})
	must(t, err, "DeleteBucketCors")
	_, err = c.GetBucketCors(ctx(t), &s3.GetBucketCorsInput{Bucket: aws.String(b)})
	wantErr(t, err, "NoSuchCORSConfiguration", http.StatusNotFound)
	// Without a bucket configuration the server-wide CORS settings apply
	// (default: any origin, Access-Control-Allow-Origin "*").
	r = pre("https://app.example.com", "GET", "")
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "ETag") {
		t.Errorf("preflight with global CORS: %d %v", r.status, r.header)
	}
}

func TestBucketConfigRoundTrips(t *testing.T) {
	e := memEnv
	c := e.root()
	b := e.newBucket(t)
	target := e.newBucket(t)

	// Default encryption.
	_, err := c.PutBucketEncryption(ctx(t), &s3.PutBucketEncryptionInput{Bucket: aws.String(b), ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
		Rules: []s3types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256}, BucketKeyEnabled: aws.Bool(true)}}}})
	must(t, err, "PutBucketEncryption")
	enc, err := c.GetBucketEncryption(ctx(t), &s3.GetBucketEncryptionInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketEncryption")
	if r := enc.ServerSideEncryptionConfiguration.Rules; len(r) != 1 || r[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm != s3types.ServerSideEncryptionAes256 || !aws.ToBool(r[0].BucketKeyEnabled) {
		t.Errorf("encryption %+v", r)
	}
	put := e.put(t, b, "enc", []byte("x"))
	if put.ServerSideEncryption != s3types.ServerSideEncryptionAes256 {
		t.Errorf("default SSE not reported on PUT: %q", put.ServerSideEncryption)
	}
	_, err = c.DeleteBucketEncryption(ctx(t), &s3.DeleteBucketEncryptionInput{Bucket: aws.String(b)})
	must(t, err, "DeleteBucketEncryption")

	// Website.
	_, err = c.PutBucketWebsite(ctx(t), &s3.PutBucketWebsiteInput{Bucket: aws.String(b), WebsiteConfiguration: &s3types.WebsiteConfiguration{
		IndexDocument: &s3types.IndexDocument{Suffix: aws.String("index.html")}, ErrorDocument: &s3types.ErrorDocument{Key: aws.String("404.html")},
		RoutingRules: []s3types.RoutingRule{{Condition: &s3types.Condition{KeyPrefixEquals: aws.String("docs/")},
			Redirect: &s3types.Redirect{ReplaceKeyPrefixWith: aws.String("documents/")}}}}})
	must(t, err, "PutBucketWebsite")
	ws, err := c.GetBucketWebsite(ctx(t), &s3.GetBucketWebsiteInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketWebsite")
	if aws.ToString(ws.IndexDocument.Suffix) != "index.html" || aws.ToString(ws.ErrorDocument.Key) != "404.html" || len(ws.RoutingRules) != 1 ||
		aws.ToString(ws.RoutingRules[0].Redirect.ReplaceKeyPrefixWith) != "documents/" {
		t.Errorf("website %+v", ws)
	}
	_, err = c.DeleteBucketWebsite(ctx(t), &s3.DeleteBucketWebsiteInput{Bucket: aws.String(b)})
	must(t, err, "DeleteBucketWebsite")
	_, err = c.GetBucketWebsite(ctx(t), &s3.GetBucketWebsiteInput{Bucket: aws.String(b)})
	wantErr(t, err, "NoSuchWebsiteConfiguration", http.StatusNotFound)

	// Notification (stored only).
	nc, err := c.GetBucketNotificationConfiguration(ctx(t), &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketNotificationConfiguration empty")
	if len(nc.QueueConfigurations)+len(nc.TopicConfigurations)+len(nc.LambdaFunctionConfigurations) != 0 {
		t.Errorf("unexpected notification config %+v", nc)
	}
	_, err = c.PutBucketNotificationConfiguration(ctx(t), &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(b), NotificationConfiguration: &s3types.NotificationConfiguration{
		QueueConfigurations: []s3types.QueueConfiguration{{Id: aws.String("q"), QueueArn: aws.String("arn:aws:sqs:us-east-1:123456789012:queue"),
			Events: []s3types.Event{s3types.EventS3ObjectCreated}}}}})
	must(t, err, "PutBucketNotificationConfiguration")
	nc, err = c.GetBucketNotificationConfiguration(ctx(t), &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketNotificationConfiguration")
	if len(nc.QueueConfigurations) != 1 || aws.ToString(nc.QueueConfigurations[0].QueueArn) != "arn:aws:sqs:us-east-1:123456789012:queue" ||
		len(nc.QueueConfigurations[0].Events) != 1 || nc.QueueConfigurations[0].Events[0] != s3types.EventS3ObjectCreated {
		t.Errorf("notification %+v", nc.QueueConfigurations)
	}

	// Logging.
	lg, err := c.GetBucketLogging(ctx(t), &s3.GetBucketLoggingInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketLogging empty")
	if lg.LoggingEnabled != nil {
		t.Errorf("logging enabled by default")
	}
	_, err = c.PutBucketLogging(ctx(t), &s3.PutBucketLoggingInput{Bucket: aws.String(b), BucketLoggingStatus: &s3types.BucketLoggingStatus{
		LoggingEnabled: &s3types.LoggingEnabled{TargetBucket: aws.String(target), TargetPrefix: aws.String("logs/")}}})
	must(t, err, "PutBucketLogging")
	lg, err = c.GetBucketLogging(ctx(t), &s3.GetBucketLoggingInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketLogging")
	if lg.LoggingEnabled == nil || aws.ToString(lg.LoggingEnabled.TargetBucket) != target || aws.ToString(lg.LoggingEnabled.TargetPrefix) != "logs/" {
		t.Errorf("logging %+v", lg.LoggingEnabled)
	}
	_, err = c.PutBucketLogging(ctx(t), &s3.PutBucketLoggingInput{Bucket: aws.String(b), BucketLoggingStatus: &s3types.BucketLoggingStatus{
		LoggingEnabled: &s3types.LoggingEnabled{TargetBucket: aws.String("no-such-target-bucket"), TargetPrefix: aws.String("logs/")}}})
	wantErr(t, err, "InvalidTargetBucketForLogging", 400)

	// Ownership controls.
	_, err = c.GetBucketOwnershipControls(ctx(t), &s3.GetBucketOwnershipControlsInput{Bucket: aws.String(b)})
	wantErr(t, err, "OwnershipControlsNotFoundError", http.StatusNotFound)
	_, err = c.PutBucketOwnershipControls(ctx(t), &s3.PutBucketOwnershipControlsInput{Bucket: aws.String(b),
		OwnershipControls: &s3types.OwnershipControls{Rules: []s3types.OwnershipControlsRule{{ObjectOwnership: s3types.ObjectOwnershipObjectWriter}}}})
	must(t, err, "PutBucketOwnershipControls")
	oc, err := c.GetBucketOwnershipControls(ctx(t), &s3.GetBucketOwnershipControlsInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketOwnershipControls")
	if len(oc.OwnershipControls.Rules) != 1 || oc.OwnershipControls.Rules[0].ObjectOwnership != s3types.ObjectOwnershipObjectWriter {
		t.Errorf("ownership %+v", oc.OwnershipControls)
	}
	_, err = c.DeleteBucketOwnershipControls(ctx(t), &s3.DeleteBucketOwnershipControlsInput{Bucket: aws.String(b)})
	must(t, err, "DeleteBucketOwnershipControls")

	// Public access block round trip with mixed values.
	_, err = c.PutPublicAccessBlock(ctx(t), &s3.PutPublicAccessBlockInput{Bucket: aws.String(b), PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
		BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(false), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(false)}})
	must(t, err, "PutPublicAccessBlock")
	pab, err := c.GetPublicAccessBlock(ctx(t), &s3.GetPublicAccessBlockInput{Bucket: aws.String(b)})
	must(t, err, "GetPublicAccessBlock")
	if p := pab.PublicAccessBlockConfiguration; !aws.ToBool(p.BlockPublicAcls) || aws.ToBool(p.IgnorePublicAcls) || !aws.ToBool(p.BlockPublicPolicy) || aws.ToBool(p.RestrictPublicBuckets) {
		t.Errorf("public access block %+v", p)
	}
	_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("pub"), Body: stringsReader("x"), ACL: s3types.ObjectCannedACLPublicRead})
	wantErr(t, err, "AccessDenied", http.StatusForbidden)

	// Accelerate / request payment / replication basics.
	rp, err := c.GetBucketRequestPayment(ctx(t), &s3.GetBucketRequestPaymentInput{Bucket: aws.String(b)})
	must(t, err, "GetBucketRequestPayment")
	if rp.Payer != s3types.PayerBucketOwner {
		t.Errorf("payer %q", rp.Payer)
	}
	_, err = c.GetBucketReplication(ctx(t), &s3.GetBucketReplicationInput{Bucket: aws.String(b)})
	wantErr(t, err, "ReplicationConfigurationNotFoundError", http.StatusNotFound)
	// Unsupported subresource.
	if r := e.signed(t, rootCreds, http.MethodGet, "/"+b+"?analytics", nil, nil); r.status != 501 || r.code() != "NotImplemented" {
		t.Errorf("?analytics: %d %s", r.status, r.code())
	}
}

func TestLifecycleConfigAndExpiration(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			_, err := c.GetBucketLifecycleConfiguration(ctx(t), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(b)})
			wantErr(t, err, "NoSuchLifecycleConfiguration", http.StatusNotFound)

			for _, k := range []string{"tmp/a", "tmp/b", "tmp/tagged", "keep/c", "logs/old"} {
				e.put(t, b, k, []byte(k))
			}
			_, err = c.PutObjectTagging(ctx(t), &s3.PutObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("logs/old"),
				Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("expire"), Value: aws.String("yes")}}}})
			must(t, err, "PutObjectTagging")
			cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("tmp/upload")})
			must(t, err, "CreateMultipartUpload")

			past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			rules := []s3types.LifecycleRule{
				{ID: aws.String("expire-tmp"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("tmp/")},
					Expiration: &s3types.LifecycleExpiration{Date: aws.Time(past)}},
				{ID: aws.String("expire-tagged-logs"), Status: s3types.ExpirationStatusEnabled,
					Filter:     &s3types.LifecycleRuleFilter{Tag: &s3types.Tag{Key: aws.String("expire"), Value: aws.String("yes")}},
					Expiration: &s3types.LifecycleExpiration{Date: aws.Time(past)}},
				{ID: aws.String("disabled"), Status: s3types.ExpirationStatusDisabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("keep/")},
					Expiration: &s3types.LifecycleExpiration{Date: aws.Time(past)}},
				{ID: aws.String("abort-uploads"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("")},
					AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)}},
			}
			_, err = c.PutBucketLifecycleConfiguration(ctx(t), &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(b),
				LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules}})
			must(t, err, "PutBucketLifecycleConfiguration")
			g, err := c.GetBucketLifecycleConfiguration(ctx(t), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(b)})
			must(t, err, "GetBucketLifecycleConfiguration")
			if len(g.Rules) != 4 || aws.ToString(g.Rules[0].ID) != "expire-tmp" || !aws.ToTime(g.Rules[0].Expiration.Date).Equal(past) ||
				aws.ToString(g.Rules[1].Filter.Tag.Key) != "expire" || g.Rules[2].Status != s3types.ExpirationStatusDisabled ||
				aws.ToInt32(g.Rules[3].AbortIncompleteMultipartUpload.DaysAfterInitiation) != 1 {
				t.Errorf("lifecycle round trip: %+v", g.Rules)
			}
			// x-amz-expiration is reported for matching objects.
			h, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("tmp/a")})
			must(t, err, "HeadObject")
			if !strings.Contains(aws.ToString(h.Expiration), `rule-id="expire-tmp"`) || !strings.Contains(aws.ToString(h.Expiration), `expiry-date="Wed, 01 Jan 2020 00:00:00 GMT"`) {
				t.Errorf("x-amz-expiration = %q", aws.ToString(h.Expiration))
			}
			h, err = c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("keep/c")})
			must(t, err, "HeadObject")
			if h.Expiration != nil {
				t.Errorf("x-amz-expiration for an object of a disabled rule: %q", aws.ToString(h.Expiration))
			}
			// x-amz-abort-date for new uploads.
			cr2, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("other/upload")})
			must(t, err, "CreateMultipartUpload")
			if cr2.AbortDate == nil || aws.ToString(cr2.AbortRuleId) != "abort-uploads" || time.Until(*cr2.AbortDate) < 24*time.Hour {
				t.Errorf("abort date %v rule %q", cr2.AbortDate, aws.ToString(cr2.AbortRuleId))
			}

			// Invalid configurations.
			_, err = c.PutBucketLifecycleConfiguration(ctx(t), &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(b),
				LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: []s3types.LifecycleRule{{ID: aws.String("x"), Status: s3types.ExpirationStatusEnabled,
					Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("")}, Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(0)}}}}})
			wantErr(t, err, "InvalidArgument", 400)

			// Run one lifecycle pass through the server's hook.
			stats, err := e.srv.RunLifecycleOnce(ctx(t))
			must(t, err, "RunLifecycleOnce")
			if stats.Expired < 4 {
				t.Errorf("stats %+v: want at least 4 expired", stats)
			}
			l, err := c.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
			must(t, err, "ListObjectsV2")
			var left []string
			for _, o := range l.Contents {
				left = append(left, aws.ToString(o.Key))
			}
			if !reflect.DeepEqual(left, []string{"keep/c"}) {
				t.Errorf("objects after lifecycle pass: %v", left)
			}
			// The upload was initiated just now, so it is not aborted yet.
			_, err = c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String("tmp/upload"), UploadId: cr.UploadId})
			must(t, err, "recent upload kept")

			_, err = c.DeleteBucketLifecycle(ctx(t), &s3.DeleteBucketLifecycleInput{Bucket: aws.String(b)})
			must(t, err, "DeleteBucketLifecycle")
			_, err = c.GetBucketLifecycleConfiguration(ctx(t), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(b)})
			wantErr(t, err, "NoSuchLifecycleConfiguration", http.StatusNotFound)
		})
	}
}

// TestLifecycleVersioned checks expiration in a versioned bucket: current
// versions get a delete marker, noncurrent versions stay, and expired
// object delete markers are removed.
func TestLifecycleVersioned(t *testing.T) {
	e := fsEnv
	c := e.root()
	b := e.newVersionedBucket(t)
	v1 := e.put(t, b, "doc", []byte("1")).VersionId
	e.put(t, b, "doc", []byte("2"))
	// A lone delete marker (all versions removed) is an expired marker.
	lone := e.put(t, b, "lone", []byte("x")).VersionId
	_, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("lone")})
	must(t, err, "DeleteObject")
	_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("lone"), VersionId: lone})
	must(t, err, "DeleteObject version")

	_, err = c.PutBucketLifecycleConfiguration(ctx(t), &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(b),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: []s3types.LifecycleRule{
			{ID: aws.String("cur"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("doc")},
				Expiration: &s3types.LifecycleExpiration{Date: aws.Time(time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC))}},
			{ID: aws.String("markers"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("lone")},
				Expiration: &s3types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)}},
		}}})
	must(t, err, "PutBucketLifecycleConfiguration")
	stats, err := e.srv.RunLifecycleOnce(ctx(t))
	must(t, err, "RunLifecycleOnce")
	if stats.Expired != 1 || stats.DeleteMarkersRemoved != 1 {
		t.Errorf("stats %+v", stats)
	}
	vs, err := c.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b)})
	must(t, err, "ListObjectVersions")
	if len(vs.Versions) != 2 || len(vs.DeleteMarkers) != 1 || aws.ToString(vs.DeleteMarkers[0].Key) != "doc" || !aws.ToBool(vs.DeleteMarkers[0].IsLatest) {
		t.Errorf("after pass: %d versions, markers %+v", len(vs.Versions), vs.DeleteMarkers)
	}
	out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("doc"), VersionId: v1})
	must(t, err, "noncurrent version still readable")
	readAll(t, out.Body)
}

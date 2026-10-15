package iam

import (
	"context"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// evalCond evaluates a single-operator condition block against ctx.
func evalCond(id auth.Identity, op, key string, vals []string, ctx map[string][]string) bool {
	c := newEvalCtx(&AuthzRequest{Identity: id, Conditions: ctx}, fixedNow)
	return c.evalConditions(map[string]map[string]StringOrSlice{op: {key: vals}}, false)
}

func kv(k string, v ...string) map[string][]string { return map[string][]string{k: v} }

func TestConditionOperators(t *testing.T) {
	alice := user("alice")
	tests := []struct {
		name string
		op   string
		key  string
		vals []string
		ctx  map[string][]string
		want bool
	}{
		// String
		{"StringEquals match", "StringEquals", "s3:prefix", []string{"a", "b"}, kv("s3:prefix", "b"), true},
		{"StringEquals case", "StringEquals", "s3:prefix", []string{"A"}, kv("s3:prefix", "a"), false},
		{"StringEquals missing", "StringEquals", "s3:prefix", []string{"a"}, nil, false},
		{"StringEquals empty values = missing", "StringEquals", "s3:prefix", []string{"a"}, kv("s3:prefix"), false},
		{"StringNotEquals differs", "StringNotEquals", "s3:prefix", []string{"a", "b"}, kv("s3:prefix", "c"), true},
		{"StringNotEquals equal", "StringNotEquals", "s3:prefix", []string{"a", "b"}, kv("s3:prefix", "b"), false},
		{"StringNotEquals missing", "StringNotEquals", "s3:prefix", []string{"a"}, nil, true},
		{"StringEqualsIgnoreCase", "StringEqualsIgnoreCase", "s3:prefix", []string{"ABC"}, kv("s3:prefix", "abc"), true},
		{"StringNotEqualsIgnoreCase", "StringNotEqualsIgnoreCase", "s3:prefix", []string{"ABC"}, kv("s3:prefix", "abc"), false},
		{"StringNotEqualsIgnoreCase differs", "StringNotEqualsIgnoreCase", "s3:prefix", []string{"ABC"}, kv("s3:prefix", "abd"), true},
		{"StringLike", "StringLike", "s3:prefix", []string{"home/*"}, kv("s3:prefix", "home/x/y"), true},
		{"StringLike qmark", "StringLike", "s3:prefix", []string{"a?c"}, kv("s3:prefix", "abc"), true},
		{"StringLike no", "StringLike", "s3:prefix", []string{"home/*"}, kv("s3:prefix", "other"), false},
		{"StringNotLike", "StringNotLike", "s3:prefix", []string{"home/*"}, kv("s3:prefix", "other"), true},
		{"StringNotLike matches", "StringNotLike", "s3:prefix", []string{"home/*"}, kv("s3:prefix", "home/1"), false},
		{"StringLike variable", "StringLike", "s3:prefix", []string{"${aws:username}/*"}, kv("s3:prefix", "alice/x"), true},
		{"StringEquals variable", "StringEquals", "s3:prefix", []string{"${aws:username}"}, kv("s3:prefix", "alice"), true},
		{"StringEquals unresolved variable", "StringEquals", "s3:prefix", []string{"${aws:nothing}"}, kv("s3:prefix", ""), false},
		{"StringLike escaped star", "StringLike", "s3:prefix", []string{"a${*}"}, kv("s3:prefix", "ab"), false},
		{"StringLike escaped star literal", "StringLike", "s3:prefix", []string{"a${*}"}, kv("s3:prefix", "a*"), true},
		{"key case insensitive", "StringEquals", "S3:PREFIX", []string{"a"}, kv("s3:prefix", "a"), true},
		{"derived username", "StringEquals", "aws:username", []string{"alice"}, nil, true},
		{"derived userid", "StringEquals", "aws:userid", []string{"alice"}, nil, true},
		{"derived principaltype", "StringEquals", "aws:PrincipalType", []string{"User"}, nil, true},

		// Numeric
		{"NumericEquals", "NumericEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10.0"), true},
		{"NumericEquals no", "NumericEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "11"), false},
		{"NumericNotEquals", "NumericNotEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "11"), true},
		{"NumericNotEquals equal", "NumericNotEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10"), false},
		{"NumericLessThan", "NumericLessThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "9"), true},
		{"NumericLessThan eq", "NumericLessThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10"), false},
		{"NumericLessThanEquals", "NumericLessThanEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10"), true},
		{"NumericGreaterThan", "NumericGreaterThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "11"), true},
		{"NumericGreaterThan eq", "NumericGreaterThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10"), false},
		{"NumericGreaterThanEquals", "NumericGreaterThanEquals", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "10"), true},
		{"Numeric invalid ctx", "NumericLessThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "abc"), false},
		{"Numeric NaN ctx", "NumericLessThan", "s3:max-keys", []string{"10"}, kv("s3:max-keys", "NaN"), false},
		{"Numeric missing", "NumericLessThan", "s3:max-keys", []string{"10"}, nil, false},

		// Date
		{"DateEquals", "DateEquals", "aws:CurrentTime", []string{"2026-03-01T12:00:00Z"}, nil, true},
		{"DateEquals epoch", "DateEquals", "aws:EpochTime", []string{"2026-03-01T12:00:00Z"}, nil, true},
		{"DateNotEquals", "DateNotEquals", "aws:CurrentTime", []string{"2026-03-01T12:00:01Z"}, nil, true},
		{"DateLessThan", "DateLessThan", "aws:CurrentTime", []string{"2027-01-01T00:00:00Z"}, nil, true},
		{"DateLessThan past", "DateLessThan", "aws:CurrentTime", []string{"2020-01-01"}, nil, false},
		{"DateLessThanEquals", "DateLessThanEquals", "aws:CurrentTime", []string{"2026-03-01T12:00:00Z"}, nil, true},
		{"DateGreaterThan", "DateGreaterThan", "aws:CurrentTime", []string{"2020-01-01"}, nil, true},
		{"DateGreaterThan tz", "DateGreaterThan", "aws:CurrentTime", []string{"2026-03-01T13:00:00+02:00"}, nil, true},
		{"DateGreaterThanEquals", "DateGreaterThanEquals", "aws:CurrentTime", []string{"2026-03-01T12:00:00Z"}, nil, true},
		{"DateGreaterThanEquals future", "DateGreaterThanEquals", "aws:CurrentTime", []string{"2026-03-01T12:00:01Z"}, nil, false},
		{"Date explicit ctx", "DateLessThan", "aws:CurrentTime", []string{"2020-01-01"}, kv("aws:CurrentTime", "2019-12-31T23:59:59Z"), true},
		{"Date invalid ctx", "DateLessThan", "s3:x", []string{"2020-01-01"}, kv("s3:x", "never"), false},
		{"Date compact ctx", "DateGreaterThan", "s3:x", []string{"2030-01-01T00:00:00Z"}, kv("s3:x", "20990101T000000Z"), true},
		{"Date fraction offset ctx", "DateGreaterThan", "s3:x", []string{"2030-01-01T00:00:00Z"}, kv("s3:x", "2099-01-01T00:00:00.000+0000"), true},

		// Bool
		{"Bool true", "Bool", "aws:SecureTransport", []string{"true"}, kv("aws:SecureTransport", "true"), true},
		{"Bool case", "Bool", "aws:SecureTransport", []string{"TRUE"}, kv("aws:SecureTransport", "true"), true},
		{"Bool mismatch", "Bool", "aws:SecureTransport", []string{"true"}, kv("aws:SecureTransport", "false"), false},
		{"Bool missing", "Bool", "aws:SecureTransport", []string{"false"}, nil, false},

		// IP
		{"IpAddress v4", "IpAddress", "aws:SourceIp", []string{"192.168.0.0/16"}, kv("aws:SourceIp", "192.168.4.5"), true},
		{"IpAddress single", "IpAddress", "aws:SourceIp", []string{"192.168.4.5"}, kv("aws:SourceIp", "192.168.4.5"), true},
		{"IpAddress port", "IpAddress", "aws:SourceIp", []string{"192.168.0.0/16"}, kv("aws:SourceIp", "192.168.4.5:4433"), true},
		{"IpAddress unmasked cidr", "IpAddress", "aws:SourceIp", []string{"192.168.4.5/16"}, kv("aws:SourceIp", "192.168.9.9"), true},
		{"IpAddress v6", "IpAddress", "aws:SourceIp", []string{"2001:db8::/32"}, kv("aws:SourceIp", "2001:db8:1::5"), true},
		{"IpAddress v6 bracket port", "IpAddress", "aws:SourceIp", []string{"2001:db8::/32"}, kv("aws:SourceIp", "[2001:db8::5]:80"), true},
		{"IpAddress v6 vs v4", "IpAddress", "aws:SourceIp", []string{"2001:db8::/32"}, kv("aws:SourceIp", "10.0.0.1"), false},
		{"IpAddress mapped cidr", "IpAddress", "aws:SourceIp", []string{"::ffff:10.0.0.0/104"}, kv("aws:SourceIp", "10.1.1.1"), true},
		{"IpAddress zero", "IpAddress", "aws:SourceIp", []string{"0.0.0.0/0"}, kv("aws:SourceIp", "8.8.8.8"), true},
		{"IpAddress invalid ctx", "IpAddress", "aws:SourceIp", []string{"0.0.0.0/0"}, kv("aws:SourceIp", "garbage"), false},
		{"IpAddress out", "IpAddress", "aws:SourceIp", []string{"10.0.0.0/8"}, kv("aws:SourceIp", "11.0.0.1"), false},
		{"NotIpAddress out", "NotIpAddress", "aws:SourceIp", []string{"10.0.0.0/8"}, kv("aws:SourceIp", "11.0.0.1"), true},
		{"NotIpAddress in", "NotIpAddress", "aws:SourceIp", []string{"10.0.0.0/8", "11.0.0.0/8"}, kv("aws:SourceIp", "11.0.0.1"), false},
		{"NotIpAddress missing", "NotIpAddress", "aws:SourceIp", []string{"10.0.0.0/8"}, nil, true},

		// Null
		{"Null true absent", "Null", "s3:x-amz-acl", []string{"true"}, nil, true},
		{"Null true present", "Null", "s3:x-amz-acl", []string{"true"}, kv("s3:x-amz-acl", "private"), false},
		{"Null false present", "Null", "s3:x-amz-acl", []string{"false"}, kv("s3:x-amz-acl", "private"), true},
		{"Null false absent", "Null", "s3:x-amz-acl", []string{"false"}, nil, false},

		// ARN
		{"ArnLike", "ArnLike", "aws:SourceArn", []string{"arn:aws:s3:::bucket-*"}, kv("aws:SourceArn", "arn:aws:s3:::bucket-1"), true},
		{"ArnLike segment boundary", "ArnLike", "aws:SourceArn", []string{"arn:*:s3:::b"}, kv("aws:SourceArn", "arn:aws:x:s3:::b"), false},
		{"ArnLike resource may contain colons", "ArnLike", "aws:SourceArn", []string{"arn:aws:sns:*:*:topic*"}, kv("aws:SourceArn", "arn:aws:sns:us-east-1:1:topic:sub"), true},
		{"ArnEquals", "ArnEquals", "aws:SourceArn", []string{"arn:aws:s3:::b"}, kv("aws:SourceArn", "arn:aws:s3:::b"), true},
		{"ArnLike not arn", "ArnLike", "aws:SourceArn", []string{"arn:aws:s3:::b"}, kv("aws:SourceArn", "b"), false},
		{"ArnNotEquals", "ArnNotEquals", "aws:SourceArn", []string{"arn:aws:s3:::b"}, kv("aws:SourceArn", "arn:aws:s3:::c"), true},
		{"ArnNotLike", "ArnNotLike", "aws:SourceArn", []string{"arn:aws:s3:::*"}, kv("aws:SourceArn", "arn:aws:s3:::c"), false},
		{"ArnLike variable", "ArnLike", "aws:SourceArn", []string{"arn:aws:iam::*:user/${aws:username}"}, kv("aws:SourceArn", "arn:aws:iam::1:user/alice"), true},

		// Binary
		{"BinaryEquals base64 ctx", "BinaryEquals", "s3:x", []string{"aGVsbG8="}, kv("s3:x", "aGVsbG8="), true},
		{"BinaryEquals raw ctx", "BinaryEquals", "s3:x", []string{"aGVsbG8="}, kv("s3:x", "hello"), true},
		{"BinaryEquals no", "BinaryEquals", "s3:x", []string{"aGVsbG8="}, kv("s3:x", "bye"), false},

		// IfExists
		{"IfExists missing", "StringEqualsIfExists", "s3:x-amz-acl", []string{"private"}, nil, true},
		{"IfExists present match", "StringEqualsIfExists", "s3:x-amz-acl", []string{"private"}, kv("s3:x-amz-acl", "private"), true},
		{"IfExists present mismatch", "StringEqualsIfExists", "s3:x-amz-acl", []string{"private"}, kv("s3:x-amz-acl", "public-read"), false},
		{"NumericIfExists missing", "NumericLessThanIfExists", "s3:max-keys", []string{"5"}, nil, true},
		{"IpAddressIfExists missing", "IpAddressIfExists", "aws:SourceIp", []string{"10.0.0.0/8"}, nil, true},
		{"BoolIfExists mismatch", "BoolIfExists", "aws:SecureTransport", []string{"false"}, kv("aws:SecureTransport", "true"), false},
		{"operator case insensitive", "stringequalsIFEXISTS", "s3:x", []string{"a"}, nil, true},

		// ForAnyValue / ForAllValues
		{"ForAnyValue match", "ForAnyValue:StringEquals", "aws:TagKeys", []string{"env", "team"}, kv("aws:TagKeys", "x", "team"), true},
		{"ForAnyValue none", "ForAnyValue:StringEquals", "aws:TagKeys", []string{"env"}, kv("aws:TagKeys", "x", "y"), false},
		{"ForAnyValue missing", "ForAnyValue:StringEquals", "aws:TagKeys", []string{"env"}, nil, false},
		{"ForAnyValue negated", "ForAnyValue:StringNotEquals", "aws:TagKeys", []string{"env"}, kv("aws:TagKeys", "env", "x"), true},
		{"ForAnyValue negated all equal", "ForAnyValue:StringNotEquals", "aws:TagKeys", []string{"env"}, kv("aws:TagKeys", "env"), false},
		{"ForAllValues subset", "ForAllValues:StringEquals", "aws:TagKeys", []string{"env", "team"}, kv("aws:TagKeys", "env", "team"), true},
		{"ForAllValues not subset", "ForAllValues:StringEquals", "aws:TagKeys", []string{"env"}, kv("aws:TagKeys", "env", "team"), false},
		{"ForAllValues missing", "ForAllValues:StringEquals", "aws:TagKeys", []string{"env"}, nil, true},
		{"ForAllValues like", "ForAllValues:StringLike", "aws:TagKeys", []string{"app-*"}, kv("aws:TagKeys", "app-1", "app-2"), true},
		{"ForAllValues negated", "ForAllValues:StringNotEquals", "aws:TagKeys", []string{"secret"}, kv("aws:TagKeys", "a", "b"), true},
		{"ForAllValues negated hit", "ForAllValues:StringNotEquals", "aws:TagKeys", []string{"secret"}, kv("aws:TagKeys", "a", "secret"), false},
		{"ForAnyValue IfExists missing", "ForAnyValue:StringEqualsIfExists", "aws:TagKeys", []string{"a"}, nil, true},
		{"multi-valued plain any", "StringEquals", "aws:TagKeys", []string{"b"}, kv("aws:TagKeys", "a", "b"), true},
		{"multi-valued plain negated", "StringNotEquals", "aws:TagKeys", []string{"b"}, kv("aws:TagKeys", "a", "b"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalCond(alice, tt.op, tt.key, tt.vals, tt.ctx); got != tt.want {
				t.Fatalf("%s %s %v with %v = %v, want %v", tt.op, tt.key, tt.vals, tt.ctx, got, tt.want)
			}
		})
	}
}

func TestConditionsAreANDed(t *testing.T) {
	cond := map[string]map[string]StringOrSlice{
		"StringEquals": {"s3:prefix": {"a"}, "s3:delimiter": {"/"}},
		"IpAddress":    {"aws:SourceIp": {"10.0.0.0/8"}},
	}
	full := map[string][]string{"s3:prefix": {"a"}, "s3:delimiter": {"/"}, "aws:SourceIp": {"10.0.0.1"}}
	c := newEvalCtx(&AuthzRequest{Identity: user("x"), Conditions: full}, fixedNow)
	if !c.evalConditions(cond, false) {
		t.Fatal("all satisfied but false")
	}
	for drop := range full {
		partial := map[string][]string{}
		for k, v := range full {
			if k != drop {
				partial[k] = v
			}
		}
		c := newEvalCtx(&AuthzRequest{Identity: user("x"), Conditions: partial}, fixedNow)
		if c.evalConditions(cond, false) {
			t.Fatalf("satisfied without %s", drop)
		}
	}
}

func TestAnonymousDerivedKeys(t *testing.T) {
	if evalCond(anon, "StringLike", "aws:username", []string{"*"}, nil) {
		t.Fatal("anonymous has a username")
	}
	if !evalCond(anon, "Null", "aws:username", []string{"true"}, nil) {
		t.Fatal("anonymous username not null")
	}
	if !evalCond(anon, "StringEquals", "aws:PrincipalType", []string{"Anonymous"}, nil) {
		t.Fatal("anonymous principal type")
	}
	// A resource variable that cannot be resolved never matches.
	m := newEvalManager(t)
	bp := mustBucketPolicy(t, "home", `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::home/${aws:username}/*"}]}`)
	d := m.Authorize(context.Background(), AuthzRequest{Identity: anon, Action: "s3:GetObject", Bucket: "home", Key: "/x", BucketPolicy: bp})
	if d.Allowed {
		t.Fatalf("unresolved variable matched: %+v", d)
	}
	d = m.Authorize(context.Background(), AuthzRequest{Identity: user("eve"), Action: "s3:GetObject", Bucket: "home", Key: "eve/x", BucketPolicy: bp})
	if !d.Allowed {
		t.Fatalf("resolved variable did not match: %+v", d)
	}
}

func TestParseCondOp(t *testing.T) {
	valid := []string{"StringEquals", "ForAnyValue:StringLike", "ForAllValues:NumericLessThanIfExists", "Null", "ArnNotLikeIfExists", "binaryequals"}
	for _, v := range valid {
		if _, ok := parseCondOp(v); !ok {
			t.Errorf("parseCondOp(%q) failed", v)
		}
	}
	invalid := []string{"", "IfExists", "NullIfExists", "ForAnyValue:", "ForSomeValues:StringEquals", "StringEqual", "StringEqualsIfExistsIfExists"}
	for _, v := range invalid {
		if _, ok := parseCondOp(v); ok {
			t.Errorf("parseCondOp(%q) succeeded", v)
		}
	}
}

// IAM condition key names, including tag-qualified keys, are case-insensitive:
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_condition.html
func TestTagConditionKeysCaseInsensitive(t *testing.T) {
	alice := user("alice")
	envTags := map[string][]string{
		"s3:ExistingObjectTag/Env": {"prod"},
		"s3:ExistingObjectTag/env": {"dev"},
	}
	tests := []struct {
		name string
		op   string
		key  string
		vals []string
		ctx  map[string][]string
		want bool
	}{
		{"exact tag key", "StringEquals", "s3:ExistingObjectTag/Public", []string{"yes"}, kv("s3:ExistingObjectTag/Public", "yes"), true},
		{"tag key case differs", "StringEquals", "s3:ExistingObjectTag/Public", []string{"yes"}, kv("s3:ExistingObjectTag/public", "yes"), true},
		{"tag value remains sensitive", "StringEquals", "s3:ExistingObjectTag/Public", []string{"YES"}, kv("s3:ExistingObjectTag/public", "yes"), false},
		{"tag key case differs negated", "StringNotEquals", "s3:ExistingObjectTag/Public", []string{"no"}, kv("s3:ExistingObjectTag/public", "no"), false},
		{"tag key case differs null", "Null", "s3:ExistingObjectTag/Public", []string{"true"}, kv("s3:ExistingObjectTag/public", "yes"), false},
		{"prefix case insensitive", "StringEquals", "S3:existingObjectTag/Public", []string{"yes"}, kv("s3:ExistingObjectTag/Public", "yes"), true},
		{"prefix case insensitive tag case differs", "StringEquals", "S3:EXISTINGOBJECTTAG/PUBLIC", []string{"yes"}, kv("s3:ExistingObjectTag/Public", "yes"), true},
		{"request object tag", "StringEquals", "s3:RequestObjectTag/Team", []string{"a"}, kv("s3:RequestObjectTag/team", "a"), true},
		{"request object tag prefix", "StringEquals", "s3:requestobjecttag/Team", []string{"a"}, kv("s3:RequestObjectTag/Team", "a"), true},
		{"aws request tag", "StringEquals", "aws:RequestTag/Team", []string{"a"}, kv("aws:RequestTag/team", "a"), true},
		{"aws resource tag", "StringEquals", "AWS:resourcetag/Team", []string{"a"}, kv("aws:ResourceTag/Team", "a"), true},
		{"Env prod", "StringEquals", "s3:ExistingObjectTag/Env", []string{"prod"}, envTags, true},
		{"Env dev", "StringEquals", "s3:ExistingObjectTag/Env", []string{"dev"}, envTags, true},
		{"env dev", "StringEquals", "s3:ExistingObjectTag/env", []string{"dev"}, envTags, true},
		{"env prod", "StringEquals", "s3:ExistingObjectTag/env", []string{"prod"}, envTags, true},
		{"ENV prod", "StringEquals", "s3:ExistingObjectTag/ENV", []string{"prod"}, envTags, true},
		{"ENV dev", "StringEquals", "s3:ExistingObjectTag/ENV", []string{"dev"}, envTags, true},
		{"ENV present", "Null", "s3:ExistingObjectTag/ENV", []string{"true"}, envTags, false},
		{"Env prefix case merged", "StringEquals", "S3:ExistingObjectTag/Env", []string{"dev"}, envTags, true},
		{"Env StringNotEquals dev", "StringNotEquals", "s3:existingobjecttag/Env", []string{"dev"}, envTags, false},
		{"Env all values differ", "ForAllValues:StringEquals", "s3:ExistingObjectTag/Env", []string{"prod"}, envTags, false},
		{"variable tag key case differs", "StringEquals", "s3:prefix", []string{"${s3:ExistingObjectTag/public}"}, map[string][]string{"s3:prefix": {"yes"}, "s3:ExistingObjectTag/Public": {"yes"}}, true},
		{"variable prefix case insensitive", "StringEquals", "s3:prefix", []string{"${S3:existingobjecttag/Public}"}, map[string][]string{"s3:prefix": {"yes"}, "s3:ExistingObjectTag/Public": {"yes"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalCond(alice, tt.op, tt.key, tt.vals, tt.ctx); got != tt.want {
				t.Fatalf("%s %s %v with %v = %v, want %v", tt.op, tt.key, tt.vals, tt.ctx, got, tt.want)
			}
		})
	}

	// The same condition must match regardless of the stored tag's case.
	m := newEvalManager(t)
	bp := mustBucketPolicy(t, "b", `{"Statement":[{"Sid":"Tagged","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/Public":"yes"}}}]}`)
	runAuthzCases(t, m, []authzCase{
		{name: "tag Public=yes", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: kv("s3:ExistingObjectTag/Public", "yes"), want: true, wantReason: "Sid=Tagged"},
		{name: "tag public=yes", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: kv("s3:ExistingObjectTag/public", "yes"), want: true, wantReason: "Sid=Tagged"},
		{name: "tag PUBLIC=yes", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: kv("s3:ExistingObjectTag/PUBLIC", "yes"), want: true, wantReason: "Sid=Tagged"},
	})
}

func TestMixedCaseTagDeny(t *testing.T) {
	m := newEvalManager(t)
	bp := mustBucketPolicy(t, "b", `{"Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/Classification":"private"}}}]}`)
	runAuthzCases(t, m, []authzCase{
		{name: "lowercase tag still denies", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: kv("s3:ExistingObjectTag/classification", "private"), want: false, wantDeny: true},
		{name: "exact tag cannot hide lowercase denial", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: map[string][]string{
			"s3:ExistingObjectTag/Classification": {"public"},
			"s3:ExistingObjectTag/classification": {"private"},
		}, want: false, wantDeny: true},
		{name: "public value still allows", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: bp, cond: kv("s3:ExistingObjectTag/classification", "public"), want: true},
	})
}

// A policy variable that cannot be resolved is effectively null, as AWS
// documents in "Policy variables with no value"
// (https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_variables.html#policy-vars-no-value):
// positive operators do not match it and negated operators do.
func TestUnresolvedPolicyVariables(t *testing.T) {
	alice := user("alice")
	owner := kv("s3:ExistingObjectTag/owner", "alice")
	arn := kv("aws:SourceArn", "arn:aws:iam::1:user/alice")
	const tagKey = "s3:ExistingObjectTag/owner"
	tests := []struct {
		name string
		id   auth.Identity
		op   string
		key  string
		vals []string
		ctx  map[string][]string
		want bool
	}{
		// Anonymous callers have no aws:username.
		{"StringEquals", anon, "StringEquals", tagKey, []string{"${aws:username}"}, owner, false},
		{"StringNotEquals", anon, "StringNotEquals", tagKey, []string{"${aws:username}"}, owner, true},
		{"StringEqualsIgnoreCase", anon, "StringEqualsIgnoreCase", tagKey, []string{"${aws:username}"}, owner, false},
		{"StringNotEqualsIgnoreCase", anon, "StringNotEqualsIgnoreCase", tagKey, []string{"${aws:username}"}, owner, true},
		{"StringLike", anon, "StringLike", tagKey, []string{"${aws:username}*"}, owner, false},
		{"StringNotLike", anon, "StringNotLike", tagKey, []string{"${aws:username}*"}, owner, true},
		{"StringEqualsIfExists", anon, "StringEqualsIfExists", tagKey, []string{"${aws:username}"}, owner, false},
		{"StringNotEqualsIfExists", anon, "StringNotEqualsIfExists", tagKey, []string{"${aws:username}"}, owner, true},
		{"ForAnyValue:StringEquals", anon, "ForAnyValue:StringEquals", tagKey, []string{"${aws:username}"}, owner, false},
		{"ForAnyValue:StringNotEquals", anon, "ForAnyValue:StringNotEquals", tagKey, []string{"${aws:username}"}, owner, true},
		{"ForAllValues:StringEquals", anon, "ForAllValues:StringEquals", tagKey, []string{"${aws:username}"}, owner, false},
		{"ForAllValues:StringNotEquals", anon, "ForAllValues:StringNotEquals", tagKey, []string{"${aws:username}"}, owner, true},
		{"ArnLike", anon, "ArnLike", "aws:SourceArn", []string{"arn:aws:iam::*:user/${aws:username}"}, arn, false},
		{"ArnNotLike", anon, "ArnNotLike", "aws:SourceArn", []string{"arn:aws:iam::*:user/${aws:username}"}, arn, true},
		// Other values of the same operator still count.
		{"StringNotEquals other value equal", anon, "StringNotEquals", tagKey, []string{"${aws:username}", "alice"}, owner, false},
		{"StringEquals other value equal", anon, "StringEquals", tagKey, []string{"${aws:username}", "alice"}, owner, true},
		// A default value resolves the variable.
		{"default StringNotEquals equal", anon, "StringNotEquals", tagKey, []string{"${aws:username, 'alice'}"}, owner, false},
		{"default StringNotEquals differs", anon, "StringNotEquals", tagKey, []string{"${aws:username, 'bob'}"}, owner, true},
		{"default StringEquals", anon, "StringEquals", tagKey, []string{"${aws:username, 'alice'}"}, owner, true},
		// A resolved variable compares as usual.
		{"resolved StringNotEquals equal", alice, "StringNotEquals", tagKey, []string{"${aws:username}"}, owner, false},
		{"resolved StringEquals equal", alice, "StringEquals", tagKey, []string{"${aws:username}"}, owner, true},
		{"resolved StringNotEquals differs", user("bob"), "StringNotEquals", tagKey, []string{"${aws:username}"}, owner, true},
		{"resolved default unused", alice, "StringNotEquals", tagKey, []string{"${aws:username, 'bob'}"}, owner, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalCond(tt.id, tt.op, tt.key, tt.vals, tt.ctx); got != tt.want {
				t.Fatalf("%s %s %v with %v = %v, want %v", tt.op, tt.key, tt.vals, tt.ctx, got, tt.want)
			}
		})
	}

	m := newEvalManager(t)
	ownerNobody := kv(tagKey, "nobody")
	// AWS's own example: a Deny with StringNotEquals against a variable
	// denies callers for whom the variable has no value.
	denyOthers := mustBucketPolicy(t, "b", `{"Version":"2012-10-17","Statement":[
		{"Sid":"Read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Sid":"OwnerOnly","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringNotEquals":{"s3:ExistingObjectTag/owner":"${aws:username}"}}}]}`)
	// The same test in an Allow grants callers without a username, as on
	// AWS; such a policy counts as public.
	allowOthers := mustBucketPolicy(t, "b", `{"Version":"2012-10-17","Statement":[
		{"Sid":"NotOwner","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringNotEquals":{"s3:ExistingObjectTag/owner":"${aws:username}"}}}]}`)
	// Guarding the variable with Null keeps anonymous callers out.
	allowOthersGuarded := mustBucketPolicy(t, "b", `{"Version":"2012-10-17","Statement":[
		{"Sid":"NotOwner","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringNotEquals":{"s3:ExistingObjectTag/owner":"${aws:username}"},"Null":{"aws:username":"false"}}}]}`)
	runAuthzCases(t, m, []authzCase{
		{name: "deny anon", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: denyOthers, cond: ownerNobody, want: false, wantDeny: true, wantReason: "Sid=OwnerOnly"},
		{name: "deny other user", id: user("eve"), action: "s3:GetObject", bucket: "b", key: "k", bp: denyOthers, cond: ownerNobody, want: false, wantDeny: true},
		{name: "deny spares owner", id: user("nobody"), action: "s3:GetObject", bucket: "b", key: "k", bp: denyOthers, cond: ownerNobody, want: true, wantReason: "Sid=Read"},
		{name: "allow anon", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthers, cond: ownerNobody, want: true, wantReason: "Sid=NotOwner"},
		{name: "allow other user", id: user("eve"), action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthers, cond: ownerNobody, want: true},
		{name: "allow not owner", id: user("nobody"), action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthers, cond: ownerNobody, want: false},
		{name: "guarded anon", id: anon, action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthersGuarded, cond: ownerNobody, want: false},
		{name: "guarded other user", id: user("eve"), action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthersGuarded, cond: ownerNobody, want: true},
		{name: "guarded not owner", id: user("nobody"), action: "s3:GetObject", bucket: "b", key: "k", bp: allowOthersGuarded, cond: ownerNobody, want: false},
	})
	if !IsPublicPolicy(allowOthers) {
		t.Fatal("Allow for everyone with a negated variable condition is not public")
	}
}

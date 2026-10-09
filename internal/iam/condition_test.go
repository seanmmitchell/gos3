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

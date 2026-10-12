package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// awsPostPolicy is the policy of the AWS "Example: Browser-Based Upload
// using HTTP POST (Using AWS Signature Version 4)" documentation.
const awsPostPolicy = "eyAiZXhwaXJhdGlvbiI6ICIyMDE1LTEyLTMwVDEyOjAwOjAwLjAwMFoiLA0KICAiY29uZGl0aW9ucyI6IFsNCiAgICB7ImJ1Y2tldCI6ICJzaWd2NGV4YW1wbGVidWNrZXQifSwNCiAgICBbInN0YXJ0cy13aXRoIiwgIiRrZXkiLCAidXNlci91c2VyMS8iXSwNCiAgICB7ImFjbCI6ICJwdWJsaWMtcmVhZCJ9LA0KICAgIHsic3VjY2Vzc19hY3Rpb25fcmVkaXJlY3QiOiAiaHR0cDovL3NpZ3Y0ZXhhbXBsZWJ1Y2tldC5zMy5hbWF6b25hd3MuY29tL3N1Y2Nlc3NmdWxfdXBsb2FkLmh0bWwifSwNCiAgICBbInN0YXJ0cy13aXRoIiwgIiRDb250ZW50LVR5cGUiLCAiaW1hZ2UvIl0sDQogICAgeyJ4LWFtei1tZXRhLXV1aWQiOiAiMTQzNjUxMjM2NTEyNzQifSwNCiAgICB7IngtYW16LXNlcnZlci1zaWRlLWVuY3J5cHRpb24iOiAiQUVTMjU2In0sDQogICAgWyJzdGFydHMtd2l0aCIsICIkeC1hbXotbWV0YS10YWciLCAiIl0sDQoNCiAgICB7IngtYW16LWNyZWRlbnRpYWwiOiAiQUtJQUlPU0ZPRE5ON0VYQU1QTEUvMjAxNTEyMjkvdXMtZWFzdC0xL3MzL2F3czRfcmVxdWVzdCJ9LA0KICAgIHsieC1hbXotYWxnb3JpdGhtIjogIkFXUzQtSE1BQy1TSEEyNTYifSwNCiAgICB7IngtYW16LWRhdGUiOiAiMjAxNTEyMjlUMDAwMDAwWiIgfQ0KICBdDQp9"

func awsPostForm() PostPolicyForm {
	return PostPolicyForm{
		"key":                          "user/user1/${filename}",
		"acl":                          "public-read",
		"success_action_redirect":      "http://sigv4examplebucket.s3.amazonaws.com/successful_upload.html",
		"content-type":                 "image/jpeg",
		"x-amz-meta-uuid":              "14365123651274",
		"x-amz-server-side-encryption": "AES256",
		"x-amz-credential":             "AKIAIOSFODNN7EXAMPLE/20151229/us-east-1/s3/aws4_request",
		"x-amz-algorithm":              "AWS4-HMAC-SHA256",
		"x-amz-date":                   "20151229T000000Z",
		"x-amz-meta-tag":               "",
		"policy":                       awsPostPolicy,
		"x-amz-signature":              "8afdbf4008c03f22c2cd3cdb72e4afbb1f6a588f3255ac628749a66d7f09699e",
		"bucket":                       "sigv4examplebucket",
	}
}

func TestAWSDocumentedPostPolicy(t *testing.T) {
	v := newTestVerifier(time.Date(2015, 12, 29, 12, 0, 0, 0, time.UTC))
	form := awsPostForm()
	id, p, err := v.VerifyPostPolicy(form)
	if err != nil {
		t.Fatal(err)
	}
	if id.AccessKey != testAK || id.IsAnonymous {
		t.Fatalf("identity %+v", id)
	}
	if !p.Expiration.Equal(time.Date(2015, 12, 30, 12, 0, 0, 0, time.UTC)) || len(p.Conditions) != 11 {
		t.Fatalf("policy %+v", p)
	}
	if err := CheckPostPolicy(p, form, 1234); err != nil {
		t.Fatalf("CheckPostPolicy: %v", err)
	}

	// Expired.
	late := newTestVerifier(time.Date(2015, 12, 30, 12, 0, 0, 0, time.UTC))
	if _, _, err := late.VerifyPostPolicy(form); errCode(err) != "AccessDenied" {
		t.Fatalf("expired: %v", err)
	}
	// Tampered policy.
	bad := awsPostForm()
	bad["policy"] = strings.Replace(awsPostPolicy, "eyAi", "eyBi", 1)
	if _, _, err := v.VerifyPostPolicy(bad); errCode(err) != "SignatureDoesNotMatch" {
		t.Fatalf("tampered: %v", err)
	}
	// Mixed-case field names from the HTML form are accepted.
	mixed := PostPolicyForm{}
	for k, val := range awsPostForm() {
		mixed[strings.ToUpper(k[:1])+k[1:]] = val
	}
	if _, p2, err := v.VerifyPostPolicy(mixed); err != nil || CheckPostPolicy(p2, mixed, 1) != nil {
		t.Fatalf("mixed case: %v", err)
	}
}

func TestCheckPostPolicyConditions(t *testing.T) {
	v := newTestVerifier(time.Date(2015, 12, 29, 12, 0, 0, 0, time.UTC))
	_, p, err := v.VerifyPostPolicy(awsPostForm())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(f PostPolicyForm)
		size int64
		code string
	}{
		{"ok", nil, 10, ""},
		{"unknown size skips range", nil, -1, ""},
		{"wrong bucket", func(f PostPolicyForm) { f["bucket"] = "other" }, 10, "AccessDenied"},
		{"missing bucket", func(f PostPolicyForm) { delete(f, "bucket") }, 10, "AccessDenied"},
		{"key prefix mismatch", func(f PostPolicyForm) { f["key"] = "user/user2/x" }, 10, "AccessDenied"},
		{"acl mismatch", func(f PostPolicyForm) { f["acl"] = "private" }, 10, "AccessDenied"},
		{"acl case differs", func(f PostPolicyForm) { f["acl"] = "Public-Read" }, 10, "AccessDenied"},
		{"content type mismatch", func(f PostPolicyForm) { f["content-type"] = "text/plain" }, 10, "AccessDenied"},
		{"content type list ok", func(f PostPolicyForm) { f["content-type"] = "image/png, image/gif" }, 10, ""},
		{"content type list bad", func(f PostPolicyForm) { f["content-type"] = "image/png,text/html" }, 10, "AccessDenied"},
		{"empty starts-with any", func(f PostPolicyForm) { f["x-amz-meta-tag"] = "anything" }, 10, ""},
		{"extra field", func(f PostPolicyForm) { f["x-amz-meta-other"] = "x" }, 10, "AccessDenied"},
		{"extra storage class", func(f PostPolicyForm) { f["x-amz-storage-class"] = "STANDARD" }, 10, "AccessDenied"},
		{"x-ignore field ok", func(f PostPolicyForm) { f["x-ignore-foo"] = "x" }, 10, ""},
		{"file field ok", func(f PostPolicyForm) { f["file"] = "x" }, 10, ""},
		{"meta mismatch", func(f PostPolicyForm) { f["x-amz-meta-uuid"] = "1" }, 10, "AccessDenied"},
		{"missing eq field", func(f PostPolicyForm) { delete(f, "x-amz-meta-uuid") }, 10, "AccessDenied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := awsPostForm()
			if tc.edit != nil {
				tc.edit(f)
			}
			if err := CheckPostPolicy(p, f, tc.size); errCode(err) != tc.code {
				t.Fatalf("got %v, want %q", err, tc.code)
			}
		})
	}
	if err := CheckPostPolicy(nil, PostPolicyForm{"anything": "x"}, 5); err != nil {
		t.Fatalf("nil policy: %v", err)
	}
}

func TestSignPostPolicyRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	v := newTestVerifier(now)
	conds := []any{
		map[string]string{"bucket": "photos"},
		[]any{"starts-with", "$key", "uploads/"},
		[]any{"content-length-range", 10, 100},
		[]any{"eq", "$success_action_status", "201"},
		[]any{"starts-with", "$Content-Type", ""},
	}
	fields, err := SignPostPolicy(Credentials{testAK, testSK}, "us-east-1", now, now.Add(time.Hour), conds)
	if err != nil {
		t.Fatal(err)
	}
	form := PostPolicyForm{"bucket": "photos", "key": "uploads/a.jpg", "success_action_status": "201", "content-type": "image/jpeg"}
	for k, val := range fields {
		form[k] = val
	}
	id, p, err := v.VerifyPostPolicy(form)
	if err != nil || id.User != "user-"+testAK {
		t.Fatalf("verify: %v %+v", err, id)
	}
	for _, tc := range []struct {
		size int64
		code string
	}{{10, ""}, {100, ""}, {50, ""}, {9, "EntityTooSmall"}, {101, "EntityTooLarge"}} {
		if err := CheckPostPolicy(p, form, tc.size); errCode(err) != tc.code {
			t.Errorf("size %d: got %v want %q", tc.size, err, tc.code)
		}
	}
	form["success_action_status"] = "200"
	if err := CheckPostPolicy(p, form, 50); errCode(err) != "AccessDenied" {
		t.Errorf("status mismatch: %v", err)
	}
	// Signed for another key.
	other := PostPolicyForm{}
	for k, val := range form {
		other[k] = val
	}
	fields2, _ := SignPostPolicy(Credentials{"other", "wrong"}, "us-east-1", now, now.Add(time.Hour), conds)
	for k, val := range fields2 {
		other[k] = val
	}
	if _, _, err := v.VerifyPostPolicy(other); errCode(err) != "SignatureDoesNotMatch" {
		t.Errorf("wrong secret: %v", err)
	}
	fields3, _ := SignPostPolicy(Credentials{"nobody", "x"}, "us-east-1", now, now.Add(time.Hour), conds)
	for k, val := range fields3 {
		other[k] = val
	}
	if _, _, err := v.VerifyPostPolicy(other); errCode(err) != "InvalidAccessKeyId" {
		t.Errorf("unknown key: %v", err)
	}
}

func policyB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestVerifyPostPolicyErrors(t *testing.T) {
	now := time.Now().UTC()
	v := newTestVerifier(now)
	strict := newTestVerifier(now, func(o *Options) { o.StrictRegion = true })
	sign := func(policyJSON string, mod func(PostPolicyForm)) PostPolicyForm {
		date := now.Format("20060102")
		f := PostPolicyForm{
			"policy":           policyB64(policyJSON),
			"x-amz-algorithm":  "AWS4-HMAC-SHA256",
			"x-amz-credential": testAK + "/" + date + "/us-east-1/s3/aws4_request",
			"x-amz-date":       now.Format("20060102T150405Z"),
		}
		if mod != nil {
			mod(f)
		}
		cred := strings.Split(f["x-amz-credential"], "/")
		f["x-amz-signature"] = strings.Repeat("0", 64)
		if len(cred) == 5 {
			f["x-amz-signature"] = PostPolicySignature(testSK, cred[1], cred[2], f["policy"])
		}
		return f
	}
	exp := now.Add(time.Hour).Format(time.RFC3339)
	valid := `{"expiration":"` + exp + `","conditions":[]}`
	tests := []struct {
		name string
		v    *Verifier
		form PostPolicyForm
		code string
		anon bool
	}{
		{"valid", v, sign(valid, nil), "", false},
		{"anonymous", v, PostPolicyForm{"key": "k"}, "", true},
		{"sigv2", v, PostPolicyForm{"awsaccesskeyid": testAK, "signature": "x", "policy": "e30="}, "AccessDenied", false},
		{"missing policy", v, func() PostPolicyForm { f := sign(valid, nil); delete(f, "policy"); return f }(), "InvalidArgument", false},
		{"missing signature", v, func() PostPolicyForm { f := sign(valid, nil); delete(f, "x-amz-signature"); return f }(), "InvalidArgument", false},
		{"missing credential", v, func() PostPolicyForm { f := sign(valid, nil); delete(f, "x-amz-credential"); return f }(), "InvalidArgument", false},
		{"missing date", v, func() PostPolicyForm { f := sign(valid, nil); delete(f, "x-amz-date"); return f }(), "InvalidArgument", false},
		{"bad algorithm", v, sign(valid, func(f PostPolicyForm) { f["x-amz-algorithm"] = "AWS4-HMAC-SHA512" }), "InvalidArgument", false},
		{"bad date", v, sign(valid, func(f PostPolicyForm) { f["x-amz-date"] = "now" }), "InvalidArgument", false},
		{"date mismatch", v, sign(valid, func(f PostPolicyForm) {
			f["x-amz-credential"] = testAK + "/19990101/us-east-1/s3/aws4_request"
		}), "InvalidArgument", false},
		{"bad credential", v, sign(valid, func(f PostPolicyForm) { f["x-amz-credential"] = "garbage" }), "InvalidArgument", false},
		{"wrong service", v, sign(valid, func(f PostPolicyForm) {
			f["x-amz-credential"] = testAK + "/" + now.Format("20060102") + "/us-east-1/ec2/aws4_request"
		}), "InvalidArgument", false},
		{"strict region", strict, sign(valid, func(f PostPolicyForm) {
			f["x-amz-credential"] = testAK + "/" + now.Format("20060102") + "/eu-west-1/s3/aws4_request"
		}), "InvalidArgument", false},
		{"security token", v, sign(valid, func(f PostPolicyForm) { f["x-amz-security-token"] = "x" }), "InvalidToken", false},
		{"bad base64", v, sign(valid, func(f PostPolicyForm) { f["policy"] = "!!!" }), "InvalidPolicyDocument", false},
		{"base64 with newlines", v, sign(valid, func(f PostPolicyForm) {
			f["policy"] = f["policy"][:10] + "\r\n" + f["policy"][10:]
		}), "", false},
		{"bad json", v, sign(`{"expiration":`, nil), "InvalidPolicyDocument", false},
		{"trailing json", v, sign(valid+`{}`, nil), "InvalidPolicyDocument", false},
		{"missing expiration", v, sign(`{"conditions":[]}`, nil), "InvalidPolicyDocument", false},
		{"bad expiration", v, sign(`{"expiration":"tomorrow","conditions":[]}`, nil), "InvalidPolicyDocument", false},
		{"expired", v, sign(`{"expiration":"`+now.Add(-time.Second).Format(time.RFC3339Nano)+`","conditions":[]}`, nil), "AccessDenied", false},
		{"missing conditions", v, sign(`{"expiration":"`+exp+`"}`, nil), "InvalidPolicyDocument", false},
		{"condition not array/object", v, sign(`{"expiration":"`+exp+`","conditions":["x"]}`, nil), "InvalidPolicyDocument", false},
		{"condition wrong arity", v, sign(`{"expiration":"`+exp+`","conditions":[["eq","$key"]]}`, nil), "InvalidPolicyDocument", false},
		{"condition unknown op", v, sign(`{"expiration":"`+exp+`","conditions":[["regex","$key","x"]]}`, nil), "InvalidPolicyDocument", false},
		{"condition field without $", v, sign(`{"expiration":"`+exp+`","conditions":[["eq","key","x"]]}`, nil), "InvalidPolicyDocument", false},
		{"condition value object", v, sign(`{"expiration":"`+exp+`","conditions":[{"key":{"a":1}}]}`, nil), "InvalidPolicyDocument", false},
		{"empty object condition", v, sign(`{"expiration":"`+exp+`","conditions":[{}]}`, nil), "InvalidPolicyDocument", false},
		{"range inverted", v, sign(`{"expiration":"`+exp+`","conditions":[["content-length-range",10,1]]}`, nil), "InvalidPolicyDocument", false},
		{"range negative", v, sign(`{"expiration":"`+exp+`","conditions":[["content-length-range",-1,1]]}`, nil), "InvalidPolicyDocument", false},
		{"range strings ok", v, sign(`{"expiration":"`+exp+`","conditions":[["content-length-range","1","10"]]}`, nil), "", false},
		{"range fraction", v, sign(`{"expiration":"`+exp+`","conditions":[["content-length-range",1.5,10]]}`, nil), "InvalidPolicyDocument", false},
		{"huge policy", v, sign(`{"expiration":"`+exp+`","conditions":[],"pad":"`+strings.Repeat("x", maxPolicySize)+`"}`, nil), "InvalidPolicyDocument", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, p, err := tc.v.VerifyPostPolicy(tc.form)
			if errCode(err) != tc.code {
				t.Fatalf("got %v, want %q", err, tc.code)
			}
			if err == nil && tc.anon != id.IsAnonymous {
				t.Fatalf("anonymous = %v", id.IsAnonymous)
			}
			if tc.anon && p != nil {
				t.Fatal("anonymous upload returned a policy")
			}
		})
	}
}

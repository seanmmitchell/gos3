package iam

import (
	"errors"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

func TestParsePolicy(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string // substring; "" = valid
	}{
		{"minimal", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`, ""},
		{"v2012", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:Get*","admin:*"],"Resource":["arn:aws:s3:::b/*","arn:gos3:admin:::*"]}]}`, ""},
		{"v2008", `{"Version":"2008-10-17","Statement":{"Effect":"Deny","NotAction":"s3:Delete*","NotResource":"arn:aws:s3:::b"}}`, ""},
		{"star action", `{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`, ""},
		{"question wildcard", `{"Statement":[{"Effect":"Allow","Action":"s3:Get?bject","Resource":"*"}]}`, ""},
		{"case insensitive svc", `{"Statement":[{"Effect":"Allow","Action":"S3:getobject","Resource":"*"}]}`, ""},
		{"conditions", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{
			"StringLike":{"s3:prefix":["home/${aws:username}/*"]},
			"ForAnyValue:StringEquals":{"aws:TagKeys":["a"]},
			"NumericLessThanEqualsIfExists":{"s3:max-keys":"10"},
			"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"},
			"Bool":{"aws:SecureTransport":true},
			"IpAddress":{"aws:SourceIp":["10.0.0.0/8","::1"]},
			"Null":{"s3:x-amz-acl":"true"},
			"ArnLike":{"aws:SourceArn":"arn:aws:s3:::*"},
			"BinaryEquals":{"s3:x":"aGVsbG8="}}}]}`, ""},
		{"bad version", `{"Version":"2020-01-01","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`, "version"},
		{"not json", `nope`, "first byte"},
		{"empty", ``, "first byte"},
		{"array", `[]`, "first byte"},
		{"invalid json", `{"Statement":`, "invalid Json"},
		{"trailing garbage", `{"Statement":[]} x`, "invalid Json"},
		{"unknown top field", `{"Statement":[],"Foo":1}`, "Unknown field Foo"},
		{"missing statement", `{"Version":"2012-10-17"}`, "Missing required field Statement"},
		{"empty statements", `{"Statement":[]}`, "Missing required field Statement"},
		{"statement string", `{"Statement":"x"}`, "Statement must be"},
		{"statement elem not object", `{"Statement":["x"]}`, "must be an object"},
		{"unknown stmt field", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Foo":1}]}`, "Unknown field Foo"},
		{"lowercase field", `{"Statement":[{"effect":"Allow","Action":"s3:*","Resource":"*"}]}`, "Unknown field effect"},
		{"principal", `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"*"}]}`, "prohibited field Principal"},
		{"notprincipal", `{"Statement":[{"Effect":"Allow","NotPrincipal":"*","Action":"s3:*","Resource":"*"}]}`, "prohibited field NotPrincipal"},
		{"missing effect", `{"Statement":[{"Action":"s3:*","Resource":"*"}]}`, "Missing required field Effect"},
		{"bad effect", `{"Statement":[{"Effect":"allow","Action":"s3:*","Resource":"*"}]}`, "Invalid effect"},
		{"missing action", `{"Statement":[{"Effect":"Allow","Resource":"*"}]}`, "Missing required field Action"},
		{"empty action", `{"Statement":[{"Effect":"Allow","Action":[],"Resource":"*"}]}`, "Missing required field Action"},
		{"both action", `{"Statement":[{"Effect":"Allow","Action":"s3:*","NotAction":"s3:Get*","Resource":"*"}]}`, "both Action and NotAction"},
		{"missing resource", `{"Statement":[{"Effect":"Allow","Action":"s3:*"}]}`, "Missing required field Resource"},
		{"both resource", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","NotResource":"*"}]}`, "both Resource and NotResource"},
		{"bad action svc", `{"Statement":[{"Effect":"Allow","Action":"iam:*","Resource":"*"}]}`, "invalid action"},
		{"bad action chars", `{"Statement":[{"Effect":"Allow","Action":"s3:Get Object","Resource":"*"}]}`, "invalid action"},
		{"bad action empty name", `{"Statement":[{"Effect":"Allow","Action":"s3:","Resource":"*"}]}`, "invalid action"},
		{"bad action no colon", `{"Statement":[{"Effect":"Allow","Action":"GetObject","Resource":"*"}]}`, "invalid action"},
		{"bad resource", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"bucket/*"}]}`, "invalid resource"},
		{"short arn", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3"}]}`, "invalid resource"},
		{"action number", `{"Statement":[{"Effect":"Allow","Action":5,"Resource":"*"}]}`, "invalid action"},
		{"action object", `{"Statement":[{"Effect":"Allow","Action":{},"Resource":"*"}]}`, "Invalid value for field Action"},
		{"dup sid", `{"Statement":[{"Sid":"a","Effect":"Allow","Action":"s3:*","Resource":"*"},{"Sid":"a","Effect":"Deny","Action":"s3:*","Resource":"*"}]}`, "unique"},
		{"unknown operator", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"StringFoo":{"s3:prefix":"a"}}}]}`, "Invalid Condition type"},
		{"null ifexists", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"NullIfExists":{"s3:prefix":"true"}}}]}`, "Invalid Condition type"},
		{"bad numeric", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"NumericEquals":{"s3:max-keys":"ten"}}}]}`, "Invalid Condition value"},
		{"bad date", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"DateLessThan":{"aws:CurrentTime":"yesterday"}}}]}`, "Invalid Condition value"},
		{"bad bool", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"yes"}}}]}`, "Invalid Condition value"},
		{"bad ip", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/33"}}}]}`, "Invalid Condition value"},
		{"bad base64", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"BinaryEquals":{"s3:x":"!!"}}}]}`, "Invalid Condition value"},
		{"bad cond key", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{"prefix":"a"}}}]}`, "invalid condition key"},
		{"empty cond values", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{"s3:prefix":[]}}}]}`, "at least one value"},
		{"empty cond block", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{}}}]}`, "at least one condition key"},
		{"cond not object", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":[]}]}`, "Invalid value for field Condition"},
		{"too big", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Sid":"` + strings.Repeat("a", MaxPolicySize) + `"}]}`, "maximum allowed document size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParsePolicy([]byte(tt.doc))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if doc == nil || len(doc.Statement) == 0 {
					t.Fatal("empty document")
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("error %v does not wrap ErrInvalidPolicy", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseBucketPolicy(t *testing.T) {
	stmt := func(s string) string { return `{"Version":"2012-10-17","Statement":[` + s + `]}` }
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"public read", stmt(`{"Sid":"P","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}`), ""},
		{"aws star", stmt(`{"Effect":"Allow","Principal":{"AWS":"*"},"Action":["s3:ListBucket"],"Resource":"arn:aws:s3:::photos"}`), ""},
		{"users and arns", stmt(`{"Effect":"Allow","Principal":{"AWS":["alice","arn:aws:iam::123456789012:user/bob","arn:aws:iam::123456789012:root","123456789012","AKIAEXAMPLE"]},"Action":"s3:*","Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}`), ""},
		{"user path arn", stmt(`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::1:user/dev/team/bob"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), ""},
		{"canonical", stmt(`{"Effect":"Allow","Principal":{"CanonicalUser":"alice"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), ""},
		{"wildcard bucket", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::pho*/*"}`), ""},
		{"wildcard all buckets", stmt(`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::*"}`), ""},
		{"not principal", stmt(`{"Effect":"Deny","NotPrincipal":{"AWS":"alice"},"Action":"s3:*","NotResource":"arn:aws:s3:::photos/public/*"}`), ""},
		{"missing principal", stmt(`{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Missing required field Principal"},
		{"both principals", stmt(`{"Effect":"Allow","Principal":"*","NotPrincipal":"*","Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "both Principal and NotPrincipal"},
		{"bad principal string", stmt(`{"Effect":"Allow","Principal":"alice","Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"service principal", stmt(`{"Effect":"Allow","Principal":{"Service":"logging.s3.amazonaws.com"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"empty principal", stmt(`{"Effect":"Allow","Principal":{},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"null principal", stmt(`{"Effect":"Allow","Principal":null,"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"bad principal chars", stmt(`{"Effect":"Allow","Principal":{"AWS":"al ice"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"bad principal arn", stmt(`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::1:role/x"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"bad principal arn svc", stmt(`{"Effect":"Allow","Principal":{"AWS":"arn:aws:s3:::photos"},"Action":"s3:*","Resource":"arn:aws:s3:::photos/*"}`), "Invalid principal in policy"},
		{"other bucket", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::other/*"}`), "Policy has invalid resource"},
		{"bare star resource", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"*"}`), "Policy has invalid resource"},
		{"bucket prefix only", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::"}`), "Policy has invalid resource"},
		{"not resource other bucket", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*","NotResource":"arn:aws:s3:::other"}`), "Policy has invalid resource"},
		{"admin action", stmt(`{"Effect":"Allow","Principal":"*","Action":"admin:*","Resource":"arn:aws:s3:::photos"}`), "Policy has invalid action"},
		{"missing resource", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:*"}`), "Missing required field Resource"},
		{"not json", `x`, "first byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseBucketPolicy([]byte(tt.doc), "photos")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if doc == nil {
					t.Fatal("nil doc")
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !errors.Is(err, s3err.ErrMalformedPolicy) {
				t.Fatalf("error %v is not MalformedPolicy", err)
			}
			if e := s3err.As(err); !strings.Contains(e.Message, tt.wantErr) {
				t.Fatalf("message %q does not contain %q", e.Message, tt.wantErr)
			}
		})
	}
}

func TestValidateBucketPolicyIdentityParity(t *testing.T) {
	// A bucket policy is not a valid identity policy and vice versa.
	bp := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	if _, err := ParsePolicy([]byte(bp)); err == nil {
		t.Fatal("identity parser accepted a principal")
	}
	ip := `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	if _, err := ParseBucketPolicy([]byte(ip), "b"); err == nil {
		t.Fatal("bucket parser accepted a statement without principal")
	}
}

func FuzzParsePolicy(f *testing.F) {
	f.Add([]byte(`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`))
	f.Add([]byte(`{"Statement":{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"1.2.3.4/32"}}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if doc, err := ParsePolicy(data); err == nil && doc == nil {
			t.Fatal("nil doc without error")
		}
		if doc, err := ParseBucketPolicy(data, "b"); err == nil {
			_ = IsPublicPolicy(doc)
		}
	})
}

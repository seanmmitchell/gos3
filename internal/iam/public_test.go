package iam

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestIsPublicPolicy(t *testing.T) {
	stmt := func(s string) string { return `{"Statement":[` + s + `]}` }
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"public read", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"aws star", stmt(`{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"named user", stmt(`{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), false},
		{"deny star", stmt(`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}`), false},
		{"source ip restricted", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`), false},
		{"source ip any", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["0.0.0.0/0"]}}}`), true},
		{"source ip mixed any", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","::/0"]}}}`), true},
		// Ranges are judged by what the evaluator matches, together.
		{"source ip split halves", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["0.0.0.0/1","128.0.0.0/1","::/1","8000::/1"]}}}`), true},
		{"source ip 4in6 any", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"::ffff:0.0.0.0/96"}}}`), true},
		{"source ip 4in6 narrow", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"::ffff:10.0.0.0/104"}}}`), false},
		{"source ip wide v4", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"0.0.0.0/4"}}}`), true},
		{"source ip wide v6", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"2000::/3"}}}`), true},
		{"source ip private ranges", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","100.64.0.0/10","fd00::/8","2001:db8::/32","203.0.113.7"]}}}`), true},
		{"source ip private v4 + v6 site", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","100.64.0.0/10","2001:db8::/32","2001:db8:1::/48","203.0.113.7"]}}}`), false},
		{"not ip is public", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`), true},
		{"vpce", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"aws:SourceVpce":"vpce-1"}}}`), false},
		{"vpce wildcard", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringLike":{"aws:SourceVpce":"vpce-*"}}}`), true},
		{"username case", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"AWS:USERNAME":"alice"}}}`), false},
		{"username ifexists", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEqualsIfExists":{"aws:username":"alice"}}}`), true},
		{"non restricting cond", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"Bool":{"aws:SecureTransport":"true"}}}`), true},
		{"not principal allow", stmt(`{"Effect":"Allow","NotPrincipal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"not principal star", stmt(`{"Effect":"Allow","NotPrincipal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), false},
		{"second statement public", stmt(`{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:*","Resource":"arn:aws:s3:::b/*"},{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::b"}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustBucketPolicy(t, "b", tt.doc)
			if got := IsPublicPolicy(doc); got != tt.want {
				t.Fatalf("IsPublicPolicy = %v, want %v", got, tt.want)
			}
		})
	}
	if IsPublicPolicy(nil) {
		t.Fatal("nil policy public")
	}
	if !IsPublicPolicy(&PolicyDocument{Statement: []Statement{{Effect: "Allow", Principal: &Principal{AWS: StringOrSlice{"*"}}}}}) {
		t.Fatal("constructed star principal not public")
	}
}

// TestIsPublicPolicyRound2 covers ForAllValues conditions (vacuously true
// for keys anonymous callers lack) and aws:SourceIp ranges split across
// statements.
func TestIsPublicPolicyRound2(t *testing.T) {
	allow := func(cond string) string {
		return `{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":` + cond + `}`
	}
	var split []string
	for i := 0; i < 64; i++ {
		split = append(split, allow(fmt.Sprintf(`{"IpAddress":{"aws:SourceIp":"%d.0.0.0/6"}}`, i*4)))
	}
	var sameNet []string
	for i := 0; i < 10; i++ {
		sameNet = append(sameNet, `{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/p`+strconv.Itoa(i)+`/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","10.1.0.0/16"]}}}`)
	}
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"forallvalues username", allow(`{"ForAllValues:StringEquals":{"aws:username":"alice"}}`), true},
		{"forallvalues vpc", allow(`{"ForAllValues:StringEquals":{"aws:SourceVpc":"vpc-1"}}`), true},
		{"forallvalues sourceip", allow(`{"ForAllValues:IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`), true},
		{"foranyvalue username", allow(`{"ForAnyValue:StringEquals":{"aws:username":"alice"}}`), false},
		{"foranyvalue negated", allow(`{"ForAnyValue:StringNotEquals":{"aws:username":"alice"}}`), true},
		{"forallvalues plus restricting", allow(`{"ForAllValues:StringEquals":{"aws:username":"alice"},"StringEquals":{"aws:SourceVpce":"vpce-1"}}`), false},
		{"split across statements", strings.Join(split, ","), true},
		{"split across statements, half", strings.Join(split[:2], ","), true},
		{"repeated narrow range", strings.Join(sameNet, ","), false},
		{"two statements, two /8", allow(`{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`) + "," + allow(`{"IpAddress":{"aws:SourceIp":"11.0.0.0/8"}}`), false},
		{"five /8 across statements", allow(`{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","11.0.0.0/8"]}}`) + "," + allow(`{"IpAddress":{"aws:SourceIp":["12.0.0.0/8","13.0.0.0/8"]}}`) + "," + allow(`{"IpAddress":{"aws:SourceIp":"14.0.0.0/8"}}`), true},
		{"ip plus other restriction not counted", allow(`{"IpAddress":{"aws:SourceIp":"0.0.0.0/1"},"StringEquals":{"aws:username":"alice"}}`), false},
		{"narrowest of two ip conditions", allow(`{"IpAddress":{"aws:SourceIp":"0.0.0.0/1"},"ForAnyValue:IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustBucketPolicy(t, "b", `{"Statement":[`+tt.doc+`]}`)
			if got := IsPublicPolicy(doc); got != tt.want {
				t.Fatalf("IsPublicPolicy = %v, want %v", got, tt.want)
			}
		})
	}
}

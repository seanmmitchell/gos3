package iam

import "testing"

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

package storage

import (
	"slices"
	"strings"
	"testing"
)

func TestResticS3Validate(t *testing.T) {
	valid := func() *ResticS3 {
		return &ResticS3{Endpoint: "https://s3.example.test", Bucket: "archives", Prefix: "gos3/a", Region: "us-east-1", AccessKey: "a", SecretKey: "s"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (*ResticS3)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"http://10.0.0.5:9000", "https://[::1]:9000/", "https://s3_internal.lan"} {
		s := valid()
		s.Endpoint = ok
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, tc := range []struct {
		field string
		mod   func(*ResticS3)
	}{
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://:443" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test:99999" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test:0" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test:" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test/?" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test#x" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://u:p@s3.example.test" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://s3.example.test/bucket" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "ftp://s3.example.test" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://bad host" }},
		{"endpoint", func(s *ResticS3) { s.Endpoint = "https://" + strings.Repeat("a", 2050) }},
		{"bucket", func(s *ResticS3) { s.Bucket = "ab" }},
		{"bucket", func(s *ResticS3) { s.Bucket = "a/b/c" }},
		{"bucket", func(s *ResticS3) { s.Bucket = "-archives" }},
		{"prefix", func(s *ResticS3) { s.Prefix = "a/../b" }},
		{"prefix", func(s *ResticS3) { s.Prefix = "a\nb" }},
		{"region", func(s *ResticS3) { s.Region = "US East" }},
		{"region", func(s *ResticS3) { s.Region = strings.Repeat("a", 70) }},
		{"key", func(s *ResticS3) { s.SecretKey = "" }},
	} {
		s := valid()
		tc.mod(s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%+v: error %v does not name %s", *s, err, tc.field)
		}
	}
}

func TestParseEncryptionProvider(t *testing.T) {
	for raw, want := range map[string]string{`{"provider":"AES256"}`: EncryptionAES256, `{"provider":"restic"}`: EncryptionRestic} {
		if got, err := ParseEncryptionProvider([]byte(raw)); err != nil || got != want {
			t.Errorf("%s: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{`{"provider":"aws:kms"}`, `{}`, `not json`} {
		if _, err := ParseEncryptionProvider([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestIntersectEncryptionProviders(t *testing.T) {
	both := []string{EncryptionAES256, EncryptionRestic}
	if got := IntersectEncryptionProviders(both, []string{EncryptionAES256}, both); !slices.Equal(got, []string{EncryptionAES256}) {
		t.Fatal(got)
	}
	if got := IntersectEncryptionProviders(both); !slices.Equal(got, both) {
		t.Fatal(got)
	}
	if IntersectEncryptionProviders() != nil {
		t.Fatal("no lists")
	}
}

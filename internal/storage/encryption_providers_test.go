package storage

import (
	"slices"
	"testing"
)

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

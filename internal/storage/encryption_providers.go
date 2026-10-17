package storage

import (
	"context"
	"slices"
)

// EncryptionProvidersBackend is implemented by backends that can tell
// which SSE-S3 implementations (EncryptionAES256, EncryptionRestic) a
// bucket can use right now: restic needs the restic executable on the
// server and a backing that can hold restic repositories for every copy of
// the bucket. The admin
// API reports the list so the web UI offers only usable providers, and
// refuses to select another one.
type EncryptionProvidersBackend interface {
	// EncryptionProviders returns the usable providers of bucket, in
	// preference order (EncryptionAES256 first).
	EncryptionProviders(ctx context.Context, bucket string) ([]string, error)
}

// ValidEncryptionProvider reports whether p names a known SSE-S3
// implementation.
func ValidEncryptionProvider(p string) bool {
	return p == EncryptionAES256 || p == EncryptionRestic
}

// IntersectEncryptionProviders returns the providers present in every
// list, in the order of the first. With no lists it returns nil.
func IntersectEncryptionProviders(lists ...[]string) []string {
	if len(lists) == 0 {
		return nil
	}
	out := slices.Clone(lists[0])
	for _, l := range lists[1:] {
		out = slices.DeleteFunc(out, func(p string) bool { return !slices.Contains(l, p) })
	}
	return out
}

package copier

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Comparison helpers shared with the multi router's replication.

// SameVersion reports whether a and b are the same stored version: the
// same ID, delete-marker flag, size, modification time, ETag and
// encryption. Metadata is compared separately (MetaEqual).
func SameVersion(a, b storage.ObjectInfo) bool {
	return a.Encryption == b.Encryption && SameContent(a, b)
}

// SameContent is SameVersion ignoring how the versions are encrypted: a
// copy encrypted differently from its source (or decrypted) holds the same
// content.
func SameContent(a, b storage.ObjectInfo) bool {
	return a.VersionID == b.VersionID && a.DeleteMarker == b.DeleteMarker && a.Size == b.Size &&
		a.LastModified.Equal(b.LastModified) && a.ETag == b.ETag
}

// MetaEqual compares object metadata, treating nil and empty maps and
// slices as equal and comparing times by instant.
func MetaEqual(a, b storage.ObjectMeta) bool {
	return reflect.DeepEqual(NormMeta(a), NormMeta(b))
}

// NormMeta returns a copy of m in the canonical form MetaEqual compares:
// empty maps and slices as nil, the retention date in UTC.
func NormMeta(m storage.ObjectMeta) storage.ObjectMeta {
	m = CloneMeta(m)
	norm := func(mm map[string]string) map[string]string {
		if len(mm) == 0 {
			return nil
		}
		return mm
	}
	m.UserMeta, m.Tags, m.Checksums = norm(m.UserMeta), norm(m.Tags), norm(m.Checksums)
	if len(m.PartSizes) == 0 {
		m.PartSizes = nil
	}
	if len(m.PartChecksums) == 0 {
		m.PartChecksums = nil
	}
	for i, pc := range m.PartChecksums {
		m.PartChecksums[i] = norm(pc)
	}
	if m.RetainUntil.IsZero() {
		m.RetainUntil = time.Time{}
	} else {
		m.RetainUntil = time.Unix(0, m.RetainUntil.UnixNano()).UTC()
	}
	return m
}

// CloneMeta returns a deep copy of m.
func CloneMeta(m storage.ObjectMeta) storage.ObjectMeta {
	m.UserMeta = maps.Clone(m.UserMeta)
	m.Tags = maps.Clone(m.Tags)
	m.Checksums = maps.Clone(m.Checksums)
	m.PartSizes = slices.Clone(m.PartSizes)
	if m.PartChecksums != nil {
		pc := make([]map[string]string, len(m.PartChecksums))
		for i, c := range m.PartChecksums {
			pc[i] = maps.Clone(c)
		}
		m.PartChecksums = pc
	}
	return m
}

// KeyVersions returns the versions and delete markers of bucket/key on b,
// newest first.
func KeyVersions(ctx context.Context, b storage.Backend, bucket, key string) ([]storage.ObjectInfo, error) {
	var out []storage.ObjectInfo
	err := b.WalkVersions(ctx, bucket, key, "", "", func(oi storage.ObjectInfo) error {
		if oi.Key != key {
			return storage.ErrStopWalk
		}
		out = append(out, oi)
		return nil
	})
	return out, err
}

// keyLatest returns the latest version of bucket/key on b as a walk over
// objects reports it: nothing when the key has no versions or its latest
// version is a delete marker.
func keyLatest(ctx context.Context, b storage.Backend, bucket, key string) ([]storage.ObjectInfo, error) {
	var out []storage.ObjectInfo
	err := b.WalkObjects(ctx, bucket, key, "", func(oi storage.ObjectInfo) error {
		if oi.Key == key {
			out = append(out, oi)
		}
		return storage.ErrStopWalk
	})
	return out, err
}

// listKey lists bucket/key on b as a walk over versions (versions) or over
// objects reports it.
func listKey(ctx context.Context, b storage.Backend, bucket, key string, versions bool) ([]storage.ObjectInfo, error) {
	if versions {
		return KeyVersions(ctx, b, bucket, key)
	}
	return keyLatest(ctx, b, bucket, key)
}

// current returns the key's current version from a listing of either kind
// (nil when there is none or it is a delete marker).
func current(vs []storage.ObjectInfo) *storage.ObjectInfo {
	if len(vs) == 0 || vs[0].DeleteMarker {
		return nil
	}
	return &vs[0]
}

// oldestFirst returns a reversed copy of a newest-first listing.
func oldestFirst(vs []storage.ObjectInfo) []storage.ObjectInfo {
	out := slices.Clone(vs)
	slices.Reverse(out)
	return out
}

// sameListing compares two versions by the fields every listing reports
// exactly as reads do: ID, delete-marker flag, size and modification time.
// ETags are left out: a listing of a native bucket may report another ETag
// than a read of the same version. Conditional deletes and updates use it
// to check that the version they act on is still the one a listing
// reported.
func sameListing(a, b storage.ObjectInfo) bool {
	return a.VersionID == b.VersionID && a.DeleteMarker == b.DeleteMarker && a.Size == b.Size &&
		a.LastModified.Equal(b.LastModified)
}

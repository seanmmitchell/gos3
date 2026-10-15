package s3client_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// lockedSetup returns a client and the bucket "locked", created with
// object lock (and so versioning) enabled.
func lockedSetup(t *testing.T) *s3client.Client {
	t.Helper()
	_, c := setup(t)
	if err := c.CreateBucketWith(t.Context(), "locked", s3client.CreateBucketOptions{ObjectLock: true}); err != nil {
		t.Fatalf("CreateBucketWith object lock: %v", err)
	}
	return c
}

func TestObjectRetention(t *testing.T) {
	c := lockedSetup(t)
	ctx := t.Context()
	v1 := putString(t, c, "locked", "k", "one")

	// No retention yet: the zero Retention, no error.
	if r, err := c.GetObjectRetention(ctx, "locked", "k", v1.VersionID); err != nil || r != (s3client.Retention{}) {
		t.Fatalf("GetObjectRetention without retention: %+v, %v", r, err)
	}

	// RetainUntil is rounded up to the millisecond.
	base := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	want := s3client.Retention{Mode: "GOVERNANCE", RetainUntil: base.Add(2 * time.Millisecond)}
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID,
		s3client.Retention{Mode: "GOVERNANCE", RetainUntil: base.Add(time.Millisecond + time.Nanosecond)}, false); err != nil {
		t.Fatalf("PutObjectRetention: %v", err)
	}
	r, err := c.GetObjectRetention(ctx, "locked", "k", v1.VersionID)
	if err != nil || r.Mode != want.Mode || !r.RetainUntil.Equal(want.RetainUntil) {
		t.Fatalf("GetObjectRetention = %+v, %v; want %+v", r, err, want)
	}
	// The current version is addressed without a version ID.
	if r, err := c.GetObjectRetention(ctx, "locked", "k", ""); err != nil || r.Mode != "GOVERNANCE" {
		t.Errorf("GetObjectRetention of the current version: %+v, %v", r, err)
	}

	// The version cannot be deleted, nor its retention shortened, without
	// bypassing governance.
	if _, err := c.DeleteObjectWith(ctx, "locked", "k", s3client.DeleteOptions{VersionID: v1.VersionID}); !s3client.IsAccessDenied(err) {
		t.Fatalf("DELETE of a retained version: %v, want 403", err)
	}
	shorter := s3client.Retention{Mode: "GOVERNANCE", RetainUntil: base.Add(-30 * time.Minute)}
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID, shorter, false); !s3client.IsAccessDenied(err) {
		t.Fatalf("shortening without bypass: %v, want 403", err)
	}
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID, shorter, true); err != nil {
		t.Fatalf("shortening with bypass: %v", err)
	}
	if r, err := c.GetObjectRetention(ctx, "locked", "k", v1.VersionID); err != nil || !r.RetainUntil.Equal(shorter.RetainUntil) {
		t.Fatalf("after shortening: %+v, %v", r, err)
	}
	// The zero Retention removes it (with bypass, as it is active).
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID, s3client.Retention{}, false); !s3client.IsAccessDenied(err) {
		t.Fatalf("removing without bypass: %v, want 403", err)
	}
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID, s3client.Retention{}, true); err != nil {
		t.Fatalf("removing with bypass: %v", err)
	}
	if r, err := c.GetObjectRetention(ctx, "locked", "k", v1.VersionID); err != nil || r != (s3client.Retention{}) {
		t.Fatalf("after removing: %+v, %v", r, err)
	}

	// Deleting a retained version works with the bypass header.
	if err := c.PutObjectRetention(ctx, "locked", "k", v1.VersionID, want, false); err != nil {
		t.Fatal(err)
	}
	res, err := c.DeleteObjectWith(ctx, "locked", "k", s3client.DeleteOptions{VersionID: v1.VersionID, BypassGovernance: true})
	if err != nil || res.VersionID != v1.VersionID {
		t.Fatalf("DELETE with bypass: %+v, %v", res, err)
	}
	// So does DeleteVersions.
	v2 := putString(t, c, "locked", "k", "two")
	if err := c.PutObjectRetention(ctx, "locked", "k", v2.VersionID, want, false); err != nil {
		t.Fatal(err)
	}
	ids := []s3client.ObjectID{{Key: "k", VersionID: v2.VersionID}}
	if _, err := c.DeleteVersions(ctx, "locked", ids, false); !s3client.IsAccessDenied(err) {
		t.Fatalf("DeleteVersions of a retained version: %v, want AccessDenied", err)
	}
	if results, err := c.DeleteVersions(ctx, "locked", ids, true); err != nil || results[0].VersionID != v2.VersionID {
		t.Fatalf("DeleteVersions with bypass: %+v, %v", results, err)
	}

	// Retention of a missing key is an error.
	if _, err := c.GetObjectRetention(ctx, "locked", "missing", ""); !s3client.IsNotFound(err) {
		t.Errorf("GetObjectRetention of a missing key: %v", err)
	}
}

func TestObjectLegalHold(t *testing.T) {
	c := lockedSetup(t)
	ctx := t.Context()
	v1 := putString(t, c, "locked", "k", "one")
	if on, err := c.GetObjectLegalHold(ctx, "locked", "k", v1.VersionID); err != nil || on {
		t.Fatalf("GetObjectLegalHold of a new version: %v, %v", on, err)
	}
	if err := c.PutObjectLegalHold(ctx, "locked", "k", v1.VersionID, true); err != nil {
		t.Fatalf("PutObjectLegalHold: %v", err)
	}
	if on, err := c.GetObjectLegalHold(ctx, "locked", "k", v1.VersionID); err != nil || !on {
		t.Fatalf("GetObjectLegalHold: %v, %v", on, err)
	}
	if _, err := c.DeleteObjectWith(ctx, "locked", "k", s3client.DeleteOptions{VersionID: v1.VersionID, BypassGovernance: true}); !s3client.IsAccessDenied(err) {
		t.Fatalf("DELETE of a held version: %v, want 403", err)
	}
	if err := c.PutObjectLegalHold(ctx, "locked", "k", v1.VersionID, false); err != nil {
		t.Fatal(err)
	}
	if on, err := c.GetObjectLegalHold(ctx, "locked", "k", ""); err != nil || on {
		t.Fatalf("GetObjectLegalHold after release: %v, %v", on, err)
	}
	if _, err := c.DeleteObjectWith(ctx, "locked", "k", s3client.DeleteOptions{VersionID: v1.VersionID}); err != nil {
		t.Fatalf("DELETE after release: %v", err)
	}
}

func TestObjectLockConfiguration(t *testing.T) {
	c := lockedSetup(t)
	ctx := t.Context()
	if cfg, found, err := c.GetObjectLockConfiguration(ctx, "bkt"); err != nil || found || cfg != (s3client.ObjectLockConfig{}) {
		t.Fatalf("GetObjectLockConfiguration without object lock: %+v, %v, %v", cfg, found, err)
	}
	cfg, found, err := c.GetObjectLockConfiguration(ctx, "locked")
	if err != nil || !found || cfg != (s3client.ObjectLockConfig{Enabled: true}) {
		t.Fatalf("GetObjectLockConfiguration of a new lock bucket: %+v, %v, %v", cfg, found, err)
	}
	for _, want := range []s3client.ObjectLockConfig{
		{Enabled: true, Mode: "GOVERNANCE", Days: 3},
		{Enabled: true, Mode: "COMPLIANCE", Years: 1},
		{Enabled: true},
	} {
		if err := c.PutObjectLockConfiguration(ctx, "locked", want); err != nil {
			t.Fatalf("PutObjectLockConfiguration %+v: %v", want, err)
		}
		if got, found, err := c.GetObjectLockConfiguration(ctx, "locked"); err != nil || !found || got != want {
			t.Errorf("GetObjectLockConfiguration = %+v, %v, %v; want %+v", got, found, err, want)
		}
	}
	if err := c.PutObjectLockConfiguration(ctx, "locked", s3client.ObjectLockConfig{}); err == nil {
		t.Error("PutObjectLockConfiguration disabling object lock succeeded")
	}

	// Enabling object lock on an existing versioned bucket.
	if err := c.PutObjectLockConfiguration(ctx, "bkt", s3client.ObjectLockConfig{Enabled: true}); err == nil {
		t.Error("PutObjectLockConfiguration on an unversioned bucket succeeded")
	}
	if err := c.PutBucketVersioning(ctx, "bkt", "Enabled"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutObjectLockConfiguration(ctx, "bkt", s3client.ObjectLockConfig{Enabled: true, Mode: "GOVERNANCE", Days: 1}); err != nil {
		t.Fatalf("enabling object lock on a versioned bucket: %v", err)
	}
	if _, found, err := c.GetObjectLockConfiguration(ctx, "bkt"); err != nil || !found {
		t.Errorf("GetObjectLockConfiguration after enabling: %v, %v", found, err)
	}
}

func TestObjectTagging(t *testing.T) {
	c := versionedSetup(t)
	ctx := t.Context()
	v1 := putString(t, c, "bkt", "k", "one")
	putString(t, c, "bkt", "k", "two")

	tags := map[string]string{"a": "1", "k 2": "v+2 = x/y@z:_.-", "ünï": "日本"}
	if err := c.PutObjectTagging(ctx, "bkt", "k", v1.VersionID, tags); err != nil {
		t.Fatalf("PutObjectTagging: %v", err)
	}
	if got, err := c.GetObjectTagging(ctx, "bkt", "k", v1.VersionID); err != nil || !maps.Equal(got, tags) {
		t.Errorf("GetObjectTagging v1 = %v, %v; want %v", got, err, tags)
	}
	if got, err := c.GetObjectTagging(ctx, "bkt", "k", ""); err != nil || got == nil || len(got) != 0 {
		t.Errorf("GetObjectTagging of the current version = %#v, %v; want an empty map", got, err)
	}
	if err := c.DeleteObjectTagging(ctx, "bkt", "k", v1.VersionID); err != nil {
		t.Fatalf("DeleteObjectTagging: %v", err)
	}
	if got, err := c.GetObjectTagging(ctx, "bkt", "k", v1.VersionID); err != nil || len(got) != 0 {
		t.Errorf("GetObjectTagging after delete = %v, %v", got, err)
	}

	// EncodeTagging builds an x-amz-tagging header the service decodes.
	_, err := c.Put(ctx, "bkt", "tagged", strings.NewReader("x"), 1, s3client.PutOptions{
		Header: http.Header{"X-Amz-Tagging": {s3client.EncodeTagging(tags)}},
	})
	if err != nil {
		t.Fatalf("Put with x-amz-tagging %q: %v", s3client.EncodeTagging(tags), err)
	}
	if got, err := c.GetObjectTagging(ctx, "bkt", "tagged", ""); err != nil || !maps.Equal(got, tags) {
		t.Errorf("tags from x-amz-tagging = %v, %v; want %v", got, err, tags)
	}
}

func TestEncodeTagging(t *testing.T) {
	for _, tc := range []struct {
		tags map[string]string
		want string
	}{
		{nil, ""},
		{map[string]string{"b": "2", "a": "x y"}, "a=x%20y&b=2"},
		{map[string]string{"k+&=": "v/%", "empty": ""}, "empty=&k%2B%26%3D=v%2F%25"},
	} {
		if got := s3client.EncodeTagging(tc.tags); got != tc.want {
			t.Errorf("EncodeTagging(%v) = %q, want %q", tc.tags, got, tc.want)
		}
	}
}

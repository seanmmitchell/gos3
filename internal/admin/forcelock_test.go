package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// putLocked stores a version of bucket/key with the given object lock state.
func (e *testEnv) putLocked(bucket, key, mode string, until time.Time, hold bool) storage.ObjectInfo {
	e.t.Helper()
	ctx := context.Background()
	w, err := e.backend.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		e.t.Fatalf("NewObjectWriter: %v", err)
	}
	if _, err := io.WriteString(w, "locked"); err != nil {
		e.t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "etag", RetentionMode: mode, RetainUntil: until, LegalHold: hold}, storage.CommitOptions{})
	if err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	return info
}

func (e *testEnv) versionCount(bucket string) int {
	e.t.Helper()
	n := 0
	if err := e.backend.WalkVersions(context.Background(), bucket, "", "", "", func(storage.ObjectInfo) error { n++; return nil }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestForceDeleteHonoursObjectLock is the regression test for force delete
// destroying versions under COMPLIANCE retention or legal hold: such a
// bucket is refused before anything is deleted, whoever asks.
func TestForceDeleteHonoursObjectLock(t *testing.T) {
	e := newEnv(t)
	lockOpts := storage.CreateBucketOptions{ObjectLockEnabled: true}
	future := e.clock.Now().Add(24 * time.Hour)

	e.createBucket("compliance", lockOpts)
	e.putObject("compliance", "plain", "x")
	e.putLocked("compliance", "worm", "COMPLIANCE", future, false)
	msg := e.root("DELETE", "/buckets/compliance?force=true", nil).expectError(t, http.StatusConflict, codeConflict)
	if !strings.Contains(msg, "COMPLIANCE") || !strings.Contains(msg, `"worm"`) {
		t.Errorf("message %q", msg)
	}
	// Bypassing governance does not help, and nothing was deleted.
	e.root("DELETE", "/buckets/compliance?force=true&bypassGovernanceRetention=true", nil).expectError(t, http.StatusConflict, codeConflict)
	if n := e.versionCount("compliance"); n != 2 {
		t.Errorf("%d versions left, want 2 (nothing deleted)", n)
	}

	e.createBucket("held", lockOpts)
	e.putLocked("held", "evidence", "", time.Time{}, true)
	e.putLocked("held", "expired", "COMPLIANCE", e.clock.Now().Add(-time.Hour), false)
	e.root("DELETE", "/buckets/held?force=true&bypassGovernanceRetention=true", nil).expectError(t, http.StatusConflict, codeConflict)
	if n := e.versionCount("held"); n != 2 {
		t.Errorf("%d versions left, want 2", n)
	}

	// Once retention has expired (and without holds) the bucket goes.
	e.clock.Advance(48 * time.Hour)
	e.root("DELETE", "/buckets/compliance?force=true", nil).expectStatus(t, http.StatusNoContent)
	if _, err := e.backend.GetBucket(context.Background(), "compliance"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("bucket still exists: %v", err)
	}
}

// TestForceDeleteGovernanceBypass checks that GOVERNANCE retention needs an
// explicit bypass and s3:BypassGovernanceRetention on the objects, beyond
// admin:ForceDeleteBucket.
func TestForceDeleteGovernanceBypass(t *testing.T) {
	e := newEnv(t)
	future := e.clock.Now().Add(time.Hour)
	setup := func(name string) {
		e.createBucket(name, storage.CreateBucketOptions{ObjectLockEnabled: true})
		e.putObject(name, "plain", "x")
		e.putLocked(name, "gov", "GOVERNANCE", future, false)
	}

	// A bucket-scoped delegate with only admin:ForceDeleteBucket.
	e.putPolicy("bucket-admin", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:ForceDeleteBucket"}, "Resource": "arn:gos3:admin:::bucket/team-*",
	})
	delegate := e.addUser("delegate", "bucket-admin")
	setup("team-a")
	e.do(&delegate, "DELETE", Prefix+"/buckets/team-a?force=true", nil).expectError(t, http.StatusConflict, codeConflict)
	e.do(&delegate, "DELETE", Prefix+"/buckets/team-a?force=true&bypassGovernanceRetention=true", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	if n := e.versionCount("team-a"); n != 2 {
		t.Errorf("%d versions left, want 2", n)
	}
	e.do(&delegate, "DELETE", Prefix+"/buckets/team-a?bypassGovernanceRetention=true", nil).expectError(t, http.StatusBadRequest, codeInvalidRequest)

	// With s3:BypassGovernanceRetention on the bucket's objects it works.
	e.putPolicy("bucket-admin-bypass", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:ForceDeleteBucket"}, "Resource": "arn:gos3:admin:::bucket/team-*",
	}, map[string]any{
		"Effect": "Allow", "Action": []string{"s3:BypassGovernanceRetention"}, "Resource": "arn:aws:s3:::team-a/*",
	})
	bypasser := e.addUser("bypasser", "bucket-admin-bypass")
	e.do(&bypasser, "DELETE", Prefix+"/buckets/team-a?force=true", nil).expectError(t, http.StatusConflict, codeConflict)
	e.do(&bypasser, "DELETE", Prefix+"/buckets/team-a?force=true&bypassGovernanceRetention=true", nil).expectStatus(t, http.StatusNoContent)

	// Root (and the built-in admin policy, s3:*) may bypass, when asked to.
	setup("team-b")
	e.root("DELETE", "/buckets/team-b?force=true", nil).expectError(t, http.StatusConflict, codeConflict)
	e.root("DELETE", "/buckets/team-b?force=true&bypassGovernanceRetention=true", nil).expectStatus(t, http.StatusNoContent)
}

// TestForceDeleteLockAppliedDuringDelete checks the per-version check that
// runs atomically with each delete: a version that became locked after the
// pre-scan is not deleted.
func TestForceDeleteLockAppliedDuringDelete(t *testing.T) {
	e := newEnv(t)
	e.createBucket("race", storage.CreateBucketOptions{ObjectLockEnabled: true})
	e.putObject("race", "plain", "x")
	rc := &reqCtx{r: httptest.NewRequest("DELETE", "/", nil), ctx: context.Background(), id: auth.Identity{IsRoot: true}}
	guard, err := e.api.lockGuard(rc, storage.BucketInfo{Name: "race", ObjectLockEnabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	e.putLocked("race", "late", "COMPLIANCE", e.clock.Now().Add(time.Hour), false)
	err = e.api.emptyBucket(context.Background(), "race", guard)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("emptyBucket = %v, want 409", err)
	}
	if _, err := e.backend.GetObjectInfo(context.Background(), "race", "late", ""); err != nil {
		t.Errorf("locked version deleted: %v", err)
	}
}

package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
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

// bypassRecorder records, for every delete-like call reaching the backend,
// whether its context carried an approved GOVERNANCE bypass.
type bypassRecorder struct {
	*memory.Backend
	mu    sync.Mutex
	calls []string // "<op> <key> bypass=<bool>"
}

func (b *bypassRecorder) record(op, key string, ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf("%s %s bypass=%v", op, key, storage.GovernanceBypass(ctx)))
}

func (b *bypassRecorder) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	b.record("DeleteObject", key, ctx)
	return b.Backend.DeleteObject(ctx, bucket, key, versionID)
}

func (b *bypassRecorder) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	b.record("DeleteObjectIf", key, ctx)
	return b.Backend.DeleteObjectIf(ctx, bucket, key, versionID, check)
}

func (b *bypassRecorder) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	b.record("AbortMultipartUpload", key, ctx)
	return b.Backend.AbortMultipartUpload(ctx, bucket, key, uploadID)
}

func (b *bypassRecorder) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	if storage.GovernanceBypass(ctx) {
		b.record("DeleteBucketConfig", key, ctx)
	}
	return b.Backend.DeleteBucketConfig(ctx, bucket, key)
}

func (b *bypassRecorder) DeleteBucket(ctx context.Context, bucket string) error {
	b.record("DeleteBucket", bucket, ctx)
	return b.Backend.DeleteBucket(ctx, bucket)
}

func (b *bypassRecorder) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.calls
	b.calls = nil
	slices.Sort(out)
	return out
}

// TestForceDeleteGovernanceBypassContext checks that an approved GOVERNANCE
// bypass reaches the backend (storage.WithGovernanceBypass) with the deletes
// of the approved objects' versions only, so that a backend enforcing
// retention itself (a native bucket's remote service) honours it, and that
// it is never set otherwise.
func TestForceDeleteGovernanceBypassContext(t *testing.T) {
	var rec *bypassRecorder
	e := newEnv(t, func(d *Deps) {
		rec = &bypassRecorder{Backend: d.Backend.(*memory.Backend)}
		d.Backend = rec
	})
	future := e.clock.Now().Add(time.Hour)
	setup := func(name string) {
		e.createBucket(name, storage.CreateBucketOptions{ObjectLockEnabled: true})
		e.putObject(name, "plain", "x")
		e.putLocked(name, "gov", "GOVERNANCE", future, false)
		e.putLocked(name, "gov", "GOVERNANCE", future, false)
		e.putLocked(name, "old", "GOVERNANCE", e.clock.Now().Add(-time.Hour), false)
		if _, err := e.backend.CreateMultipartUpload(context.Background(), name, "gov", storage.ObjectMeta{}, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Approved: only the versions of "gov" (unexpired GOVERNANCE) carry it.
	setup("approved")
	e.root("DELETE", "/buckets/approved?force=true&bypassGovernanceRetention=true", nil).expectStatus(t, http.StatusNoContent)
	want := []string{
		"AbortMultipartUpload gov bypass=false",
		"DeleteBucket approved bypass=false",
		"DeleteObjectIf gov bypass=true", "DeleteObjectIf gov bypass=true",
		"DeleteObjectIf old bypass=false", "DeleteObjectIf plain bypass=false",
	}
	if got := rec.take(); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant %q", got, want)
	}

	// A bypass asked for without GOVERNANCE retention in force is not set.
	e.createBucket("unlocked", storage.CreateBucketOptions{})
	e.putObject("unlocked", "plain", "x")
	e.root("DELETE", "/buckets/unlocked?force=true&bypassGovernanceRetention=true", nil).expectStatus(t, http.StatusNoContent)
	if got := rec.take(); slices.ContainsFunc(got, func(c string) bool { return strings.HasSuffix(c, "bypass=true") }) {
		t.Errorf("calls = %q", got)
	}

	// Not asked for or not allowed: refused, nothing deleted with a bypass.
	setup("refused")
	e.root("DELETE", "/buckets/refused?force=true", nil).expectError(t, http.StatusConflict, codeConflict)
	e.putPolicy("force-only", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:ForceDeleteBucket"}, "Resource": "arn:gos3:admin:::bucket/*",
	})
	u := e.addUser("force-only", "force-only")
	e.do(&u, "DELETE", Prefix+"/buckets/refused?force=true&bypassGovernanceRetention=true", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("calls = %q", got)
	}
}

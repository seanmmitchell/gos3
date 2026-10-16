package multi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// lockedBucket creates the versioned bucket "vault" on a and replicates it
// active-passive to b, in sync.
func lockedBucket(t *testing.T) (*Router, storage.Backend, storage.Backend) {
	t.Helper()
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, "vault", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "vault", ModeActivePassive, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "vault", StateInSync)
	return r, bk[0].Backend, bk[1].Backend
}

func hasVersion(t *testing.T, b storage.Backend, key, id string) bool {
	t.Helper()
	_, err := b.GetObjectInfo(context.Background(), "vault", key, id)
	if err != nil && !errors.Is(err, storage.ErrVersionNotFound) && !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// A forced promote of a lagging replica must not make the full sync of
// the old primary delete its object-locked versions; they are copied to
// the new primary instead (H-06).
func TestForcedPromoteKeepsLockedVersions(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	put(t, r, "vault", "k", "replicated", storage.ObjectMeta{})
	waitSame(t, "vault", a, b)

	// Acknowledged on the primary, not replicated yet: one version under
	// COMPLIANCE retention, one under a legal hold, one unlocked.
	retained := put(t, a, "vault", "k", "retained", storage.ObjectMeta{
		RetentionMode: "COMPLIANCE", RetainUntil: time.Now().Add(time.Hour),
	})
	held := put(t, a, "vault", "held", "held", storage.ObjectMeta{LegalHold: true})
	plain := put(t, a, "vault", "plain", "plain", storage.ObjectMeta{})

	if _, err := r.Promote(ctx, "vault", "b", true); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "vault", StateInSync)
	waitSame(t, "vault", a, b)
	for _, v := range []storage.ObjectInfo{retained, held} {
		if !hasVersion(t, a, v.Key, v.VersionID) || !hasVersion(t, b, v.Key, v.VersionID) {
			t.Fatalf("locked version %s/%s lost after a forced promote", v.Key, v.VersionID)
		}
	}
	if got := read(t, r, "vault", "k", ""); got != "retained" {
		t.Fatalf("current version after the restore = %q", got)
	}
	// An unlocked version the new primary never received is discarded,
	// as before.
	if hasVersion(t, a, "plain", plain.VersionID) {
		t.Fatal("unlocked version of the old primary kept")
	}
}

// When the destination's locked null version and the source's null version
// differ, syncKey restoring a diverged copy leaves the destination alone.
func TestSyncKeyLockedNullConflict(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	if err := r.SetBucketVersioning(ctx, "vault", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	waitSame(t, "vault", a, b)
	put(t, a, "vault", "k", "primary", storage.ObjectMeta{})
	locked := put(t, b, "vault", "k", "locked", storage.ObjectMeta{LegalHold: true})
	top := r.topology.Load()
	err := r.syncKey(ctx, top.backings["a"], top.backings["b"], "vault", "k", true, nil)
	if !errors.Is(err, errLockedConflict) {
		t.Fatalf("syncKey = %v, want errLockedConflict", err)
	}
	if got := read(t, b, "vault", "k", locked.VersionID); got != "locked" {
		t.Fatalf("locked null version replaced: %q", got)
	}
}

// Dropping the old primary right after a forced promote purges its copy,
// but not a locked version the new primary lacks: that one is restored to
// the primary first (H-06).
func TestPurgeRestoresLockedVersions(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	locked := put(t, a, "vault", "only-a", "locked", storage.ObjectMeta{
		RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour),
	})
	if _, err := r.Promote(ctx, "vault", "b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "vault", ModeSingle, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the copy on a to be purged", func() bool {
		_, err := a.GetBucket(ctx, "vault")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
	if got := read(t, b, "vault", "only-a", locked.VersionID); got != "locked" {
		t.Fatalf("restored version = %q", got)
	}
}

// A replica that was not the primary has no writes of its own: a locked
// version the primary no longer has was deleted there legitimately (a
// GOVERNANCE bypass), and the replica deletes it too.
func TestLockedDeleteReplicates(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	v := put(t, r, "vault", "k", "governed", storage.ObjectMeta{
		RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour),
	})
	waitSame(t, "vault", a, b)
	if _, err := r.DeleteObject(storage.WithGovernanceBypass(ctx), "vault", "k", v.VersionID); err != nil {
		t.Fatal(err)
	}
	waitSame(t, "vault", a, b)
	if hasVersion(t, a, "k", v.VersionID) || hasVersion(t, b, "k", v.VersionID) {
		t.Fatal("a deleted locked version came back")
	}
}

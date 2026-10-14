package multi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// G1-a: a bucket's encryption failure does not make its backing unhealthy.
func TestEncryptionFailureIsNotBackingFailure(t *testing.T) {
	if isBackingError(fmt.Errorf("restic: archive object: %w", storage.ErrEncryptionUnavailable)) {
		t.Fatal("encryption failure counted as backing failure")
	}
	if !isBackingError(errors.New("disk on fire")) {
		t.Fatal("plain failure not counted")
	}
}

// resticVersions is a backing whose WalkVersions reports a restic version.
type resticVersions struct{ storage.Backend }

func (r resticVersions) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	err := fn(storage.ObjectInfo{Key: "old", ObjectMeta: storage.ObjectMeta{Encryption: storage.EncryptionRestic}})
	if errors.Is(err, storage.ErrStopWalk) {
		return nil
	}
	return err
}

// ResticBackingSettings makes resticVersions a restic-capable backing.
func (r resticVersions) ResticBackingSettings(context.Context) (*storage.ResticS3, bool, error) {
	return nil, false, nil
}

// ConfigureResticBacking completes storage.ResticBackingBackend.
func (r resticVersions) ConfigureResticBacking(context.Context, *storage.ResticS3) error { return nil }

// K8: copies of restic buckets cannot be placed on backings that cannot
// store restic archives, in either order of configuration.
func TestResticPlacementNeedsResticBackings(t *testing.T) {
	ctx := context.Background()
	bk := []Backing{fsBacking(t, "f", t.TempDir()), {Name: "m", Kind: "memory", Backend: memory.New()}, fsBacking(t, "g", t.TempDir())}
	r := openRouter(t, Options{Backings: bk})
	for _, name := range []string{"plain", "arc"} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.PutBucketConfig(ctx, "arc", storage.ConfigEncryptionProvider, []byte(`{"provider":"restic"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "arc", ModeActivePassive, []string{"m"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("memory replica of a restic bucket: %v", err)
	}
	if _, err := r.Move(ctx, "arc", "", "m"); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("move of a restic bucket to memory: %v", err)
	}
	if _, err := r.SetReplication(ctx, "arc", ModeActivePassive, []string{"g"}); err != nil {
		t.Fatalf("fs replica of a restic bucket: %v", err)
	}
	// A bucket without restic may get a memory replica, which then
	// prevents switching it to restic.
	if _, err := r.SetReplication(ctx, "plain", ModeActivePassive, []string{"m"}); err != nil {
		t.Fatal(err)
	}
	err := r.ValidateEncryptionSettings(ctx, "plain", storage.EncryptionSettings{Provider: storage.EncryptionRestic})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("restic with a memory replica: %v", err)
	}
	if err := r.ValidateEncryptionSettings(ctx, "plain", storage.EncryptionSettings{Provider: storage.EncryptionAES256}); err != nil {
		t.Fatal(err)
	}
}

// K8: restic versions kept after switching the provider back still need
// restic-capable backings.
func TestResticVersionsNeedResticBackings(t *testing.T) {
	ctx := context.Background()
	f := fsBacking(t, "f", t.TempDir())
	f.Backend = resticVersions{f.Backend}
	r := openRouter(t, Options{Backings: []Backing{f, {Name: "m", Kind: "memory", Backend: memory.New()}}})
	if err := r.CreateBucket(ctx, "arc", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "arc", ModeActivePassive, []string{"m"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("memory replica of a bucket with restic versions: %v", err)
	}
}

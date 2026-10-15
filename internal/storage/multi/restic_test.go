package multi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
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

// EncryptionProviders makes resticVersions a restic-capable backing.
func (r resticVersions) EncryptionProviders(context.Context, string) ([]string, error) {
	return []string{storage.EncryptionAES256, storage.EncryptionRestic}, nil
}

// resticFS is an fs backend that offers restic whether or not the restic
// executable is installed (placement checks never run restic).
type resticFS struct{ *fsbackend.Backend }

func (resticFS) EncryptionProviders(context.Context, string) ([]string, error) {
	return []string{storage.EncryptionAES256, storage.EncryptionRestic}, nil
}

// resticFSBacking is fsBacking with a resticFS backend.
func resticFSBacking(t *testing.T, name, dir string) Backing {
	b := fsBacking(t, name, dir)
	b.Backend = resticFS{b.Backend.(*fsbackend.Backend)}
	return b
}

// K8: copies of restic buckets cannot be placed on backings that cannot
// store restic archives, in either order of configuration.
func TestResticPlacementNeedsResticBackings(t *testing.T) {
	ctx := context.Background()
	bk := []Backing{resticFSBacking(t, "f", t.TempDir()), {Name: "m", Kind: "memory", Backend: memory.New()}, resticFSBacking(t, "g", t.TempDir())}
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

// K8 with live backing configuration: a memory backing added at runtime
// cannot receive a copy of a restic bucket either.
func TestResticPlacementOnLiveBackings(t *testing.T) {
	ctx := context.Background()
	r := openRouter(t, Options{Backings: []Backing{fsBacking(t, "default", t.TempDir())}})
	r.EnableBackingManagement(config.StorageConfig{Backend: "fs"}, "", BackingOpener{Open: func(d config.BackingDefinition) (Backing, error) {
		return Backing{Name: d.Name, Kind: d.Backend, Path: d.Path, Backend: memory.New()}, nil
	}})
	if err := r.CreateBucket(ctx, "arc", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutBucketConfig(ctx, "arc", storage.ConfigEncryptionProvider, []byte(`{"provider":"restic"}`)); err != nil {
		t.Fatal(err)
	}
	in, _ := r.BackingConfiguration()
	in.Backings = append(in.Backings, config.BackingDefinition{Name: "live", Backend: "memory"})
	if _, err := r.ConfigureBackings(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "arc", ModeActivePassive, []string{"live"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("live memory replica of a restic bucket: %v", err)
	}
	if _, err := r.Move(ctx, "arc", "", "live"); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("move of a restic bucket to a live memory backing: %v", err)
	}
}

// noProviders is a backend without storage.EncryptionProvidersBackend,
// which therefore stores no encrypted data.
type noProviders struct{ storage.Backend }

// NewVersionWriter keeps noProviders a storage.VersionImporter.
func (n noProviders) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return n.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

// A backing that offers no SSE-S3 implementation cannot receive a copy of
// a bucket with AES256 data, but may receive one of an unencrypted bucket.
func TestEncryptedPlacementNeedsProviders(t *testing.T) {
	ctx := context.Background()
	bk := []Backing{fsBacking(t, "f", t.TempDir()), {Name: "p", Kind: "memory", Backend: noProviders{memory.New()}}}
	r := openRouter(t, Options{Backings: bk})
	for _, name := range []string{"plain", "enc"} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w, err := r.NewObjectWriterWith(ctx, "enc", "k", storage.WriteOptions{Encryption: storage.EncryptionAES256})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("secret"))
	if _, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "enc", ModeActivePassive, []string{"p"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("replica of AES256 data on a backing without providers: %v", err)
	}
	if _, err := r.SetReplication(ctx, "plain", ModeActivePassive, []string{"p"}); err != nil {
		t.Fatalf("replica of an unencrypted bucket: %v", err)
	}
}

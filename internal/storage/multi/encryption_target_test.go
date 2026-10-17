package multi

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// sseDefault is a storage.ConfigEncryption document encrypting new
// objects with algo by default.
func sseDefault(algo string) []byte {
	return []byte(`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>` +
		algo + `</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`)
}

func TestDefaultSSE(t *testing.T) {
	for _, c := range []struct {
		raw  []byte
		want bool
	}{
		{sseDefault("AES256"), true},
		{sseDefault("aws:kms"), true}, // legacy KMS defaults apply SSE-S3
		{[]byte(`<ServerSideEncryptionConfiguration></ServerSideEncryptionConfiguration>`), false},
		{[]byte(`not xml`), false}, // ignored by the S3 layer too
	} {
		if got := defaultSSE(c.raw); got != c.want {
			t.Errorf("defaultSSE(%s) = %v", c.raw, got)
		}
	}
}

// A bucket that encrypts (or will encrypt) with AES256 cannot get a copy
// on a backing without AES256 (a KEK-less s3 backing), and an AES256 write
// to a bucket with such a copy is refused instead of never replicating.
func TestAESNeedsAESOnEveryCopy(t *testing.T) {
	ctx := context.Background()
	bk := []Backing{fsBacking(t, "f", t.TempDir()), {Name: "k", Kind: "s3", Backend: noProviders{memory.New()}}}
	r := openRouter(t, Options{Backings: bk})
	for _, name := range []string{"dflt", "explicit", "plain"} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.PutBucketConfig(ctx, "dflt", storage.ConfigEncryption, sseDefault("AES256")); err != nil {
		t.Fatal(err)
	}
	if err := r.PutBucketConfig(ctx, "explicit", storage.ConfigEncryptionProvider, []byte(`{"provider":"AES256"}`)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dflt", "explicit"} {
		if _, err := r.SetReplication(ctx, name, ModeActivePassive, []string{"k"}); !errors.Is(err, ErrInvalidPlacement) {
			t.Fatalf("replica of %s on a backing without AES256: %v", name, err)
		}
		if _, err := r.Move(ctx, name, "", "k", nil); !errors.Is(err, ErrInvalidPlacement) {
			t.Fatalf("move of %s to a backing without AES256: %v", name, err)
		}
	}

	// An unencrypted bucket may get the copy; its encryption may not
	// then use a provider the copy lacks.
	if _, err := r.SetReplication(ctx, "plain", ModeActivePassive, []string{"k"}); err != nil {
		t.Fatal(err)
	}
	if err := r.PrepareEncryption(ctx, "plain", storage.EncryptionAES256); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("PrepareEncryption with a copy lacking AES256: %v", err)
	}
	cfg := storage.EncryptionSettings{Provider: storage.EncryptionAES256}
	if err := r.ValidateEncryptionSettings(ctx, "plain", cfg); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ValidateEncryptionSettings with a copy lacking AES256: %v", err)
	}
	if _, err := r.NewObjectWriterWith(ctx, "plain", "k", storage.WriteOptions{Encryption: storage.EncryptionAES256}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("AES256 write with a copy lacking AES256: %v", err)
	}
	if r.Backings()[0].Healthy != true || r.Backings()[1].Healthy != true {
		t.Fatal("a refused encrypted write marked a backing unhealthy")
	}
	// Plain writes still replicate.
	put(t, r, "plain", "k", "data", storage.ObjectMeta{})
	waitSame(t, "plain", bk[0].Backend, bk[1].Backend)

	// Without the copy, AES256 works on the primary.
	if _, err := r.SetReplication(ctx, "plain", ModeSingle, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.PrepareEncryption(ctx, "plain", storage.EncryptionAES256); err != nil {
		t.Fatalf("PrepareEncryption on a single copy: %v", err)
	}
}

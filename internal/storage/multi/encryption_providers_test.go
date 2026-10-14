package multi

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// resticCapable is a memory backend claiming restic support, standing in
// for an fs backing with restic installed.
type resticCapable struct{ *memory.Backend }

func (resticCapable) EncryptionProviders(context.Context, string) ([]string, error) {
	return []string{storage.EncryptionAES256, storage.EncryptionRestic}, nil
}

// A bucket can use restic only while every copy of it can.
func TestEncryptionProvidersIntersectMembers(t *testing.T) {
	ctx := context.Background()
	r := openRouter(t, Options{Backings: []Backing{
		{Name: "a", Kind: "fs", Backend: resticCapable{memory.New()}},
		{Name: "b", Kind: "fs", Backend: resticCapable{memory.New()}},
		{Name: "m", Kind: "memory", Backend: memory.New()},
	}})
	if err := r.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	both := []string{storage.EncryptionAES256, storage.EncryptionRestic}
	if got, err := r.EncryptionProviders(ctx, "bk"); err != nil || !slices.Equal(got, both) {
		t.Fatalf("single fs copy: %v %v", got, err)
	}
	if _, err := r.SetReplication(ctx, "bk", ModeActivePassive, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.EncryptionProviders(ctx, "bk"); err != nil || !slices.Equal(got, both) {
		t.Fatalf("fs replica: %v %v", got, err)
	}
	if _, err := r.SetReplication(ctx, "bk", ModeActivePassive, []string{"b", "m"}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.EncryptionProviders(ctx, "bk"); err != nil || !slices.Equal(got, []string{storage.EncryptionAES256}) {
		t.Fatalf("memory replica: %v %v", got, err)
	}
	if _, err := r.EncryptionProviders(ctx, "missing"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
}

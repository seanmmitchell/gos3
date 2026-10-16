package multi

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// The router reports a bucket's capabilities as its primary does, also
// through storage.CapsOf (the router imports versions, so CapsOf's default
// would claim every bucket imports), and those of a new bucket as the
// backing it would be created on.
func TestRouterBucketCaps(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	k := newKinded(memory.New(), storage.BucketKindNative, true, "box")
	r := openRouter(t, Options{Backings: []Backing{{Name: "m", Backend: mem}, {Name: "k", Kind: "s3", Backend: k}}})
	for name, opts := range map[string]storage.CreateBucketOptions{
		"full":    {},
		"nat":     {Backing: "k"},
		"wrapped": {Backing: "k", Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "box"}},
	} {
		if err := r.CreateBucket(ctx, name, opts); err != nil {
			t.Fatalf("CreateBucket %s: %v", name, err)
		}
	}
	memCaps, err := storage.CapsOf(ctx, mem, "full")
	if err != nil {
		t.Fatal(err)
	}
	for bucket, want := range map[string]storage.BucketCaps{
		"full":    memCaps,
		"nat":     kindCaps(storage.BucketKindNative),
		"wrapped": kindCaps(storage.BucketKindWrapped),
	} {
		got, err := r.BucketCaps(ctx, bucket)
		if err != nil || got != want {
			t.Errorf("BucketCaps(%s) = %+v, %v; want %+v", bucket, got, err, want)
		}
		got, err = storage.CapsOf(ctx, r, bucket)
		if err != nil || got != want {
			t.Errorf("storage.CapsOf(router, %s) = %+v, %v; want %+v", bucket, got, err, want)
		}
	}
	if !memCaps.Import || !memCaps.History {
		t.Fatalf("memory caps %+v", memCaps)
	}
	if _, err := r.BucketCaps(ctx, "nobucket"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("BucketCaps of an unknown bucket: %v", err)
	}

	for _, c := range []struct {
		name string
		opts storage.CreateBucketOptions
		want storage.BucketCaps
		err  error
	}{
		{"default backing", storage.CreateBucketOptions{}, memCaps, nil},
		{"named backing", storage.CreateBucketOptions{Backing: "k"}, kindCaps(storage.BucketKindNative), nil},
		{"copy", storage.CreateBucketOptions{Backing: "k", Import: true}, kindCaps(storage.BucketKindWrapped), nil},
		{"target", storage.CreateBucketOptions{Backing: "k", Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "box"}},
			kindCaps(storage.BucketKindWrapped), nil},
		{"unknown container", storage.CreateBucketOptions{Backing: "k", Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "nope"}},
			storage.BucketCaps{}, storage.ErrInvalidTarget},
		{"target without kinds", storage.CreateBucketOptions{Target: &storage.BucketTarget{Kind: storage.BucketKindNative}},
			storage.BucketCaps{}, storage.ErrInvalidTarget},
		{"unknown backing", storage.CreateBucketOptions{Backing: "zz"}, storage.BucketCaps{}, storage.ErrInvalidBacking},
	} {
		got, err := r.NewBucketCaps(ctx, c.opts)
		if (c.err == nil && err != nil) || (c.err != nil && !errors.Is(err, c.err)) || got != c.want {
			t.Errorf("NewBucketCaps(%s) = %+v, %v; want %+v, %v", c.name, got, err, c.want, c.err)
		}
	}
	// The member backends never see the router's backing names.
	if opts, _ := k.creation("nat"); opts.Backing != "" {
		t.Fatalf("the backing received the router's backing name: %+v", opts)
	}
}

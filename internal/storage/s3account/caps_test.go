package s3account

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestNewBucketCaps(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	withHome := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))
	local := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))

	type tc struct {
		name string
		b    *Backend
		opts storage.CreateBucketOptions
		kind storage.BucketKind // "" = an error
	}
	tgt := func(bt storage.BucketTarget) *storage.BucketTarget { return &bt }
	for _, c := range []tc{
		{"home default", withHome, storage.CreateBucketOptions{}, storage.BucketKindWrapped},
		{"home import", withHome, storage.CreateBucketOptions{Import: true}, storage.BucketKindWrapped},
		{"home native", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: storage.BucketKindNative})}, storage.BucketKindNative},
		{"home remote bucket implies native", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{RemoteBucket: "x"})}, storage.BucketKindNative},
		{"home container home", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Container: homeID})}, storage.BucketKindWrapped},
		{"home unknown container", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Container: "c0"})}, ""},
		{"home import native", withHome, storage.CreateBucketOptions{Import: true, Target: tgt(storage.BucketTarget{Kind: storage.BucketKindNative})}, ""},
		{"home native with container", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: storage.BucketKindNative, Container: homeID})}, ""},
		{"home wrapped with remote", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: storage.BucketKindWrapped, RemoteBucket: "x"})}, ""},
		{"unknown kind", withHome, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: "other"})}, ""},
		{"local default", local, storage.CreateBucketOptions{}, storage.BucketKindNative},
		{"local wrapped without container", local, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: storage.BucketKindWrapped})}, ""},
		{"local import without container", local, storage.CreateBucketOptions{Import: true}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			caps, err := c.b.NewBucketCaps(ctx, c.opts)
			if c.kind == "" {
				if !errors.Is(err, storage.ErrInvalidTarget) {
					t.Fatalf("got %+v, %v; want ErrInvalidTarget", caps, err)
				}
				return
			}
			noErr(t, "NewBucketCaps", err)
			if caps.Kind != c.kind {
				t.Fatalf("kind %q, want %q", caps.Kind, c.kind)
			}
			if want := c.kind == storage.BucketKindWrapped; caps.Import != want || caps.History != want || caps.Compression != want {
				t.Fatalf("caps %+v for kind %s", caps, c.kind)
			}
			if !caps.Clone {
				t.Fatalf("caps %+v: no Clone", caps)
			}
		})
	}

	// With a dedicated container, a local backing creates wrapped buckets
	// there once it is the default; imports go to the default container.
	ci, err := local.AddContainer(ctx, ContainerSpec{Bucket: "ded", CreateBucket: true})
	noErr(t, "AddContainer", err)
	if _, err := local.NewBucketCaps(ctx, storage.CreateBucketOptions{Import: true}); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("import without a default container: %v", err)
	}
	caps, err := local.NewBucketCaps(ctx, storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Container: ci.ID})})
	noErr(t, "NewBucketCaps(container)", err)
	if caps.Kind != storage.BucketKindWrapped || !caps.Import {
		t.Fatalf("caps %+v", caps)
	}
	noErr(t, "SetDefaultContainer", local.SetDefaultContainer(ctx, ci.ID))
	caps, err = local.NewBucketCaps(ctx, storage.CreateBucketOptions{Import: true})
	noErr(t, "NewBucketCaps(import)", err)
	if caps.Kind != storage.BucketKindWrapped {
		t.Fatalf("caps %+v", caps)
	}

	// BucketCaps of existing buckets.
	noErr(t, "create wrapped", withHome.CreateBucket(ctx, "w", storage.CreateBucketOptions{}))
	noErr(t, "create native", withHome.CreateBucket(ctx, "nat1", storage.CreateBucketOptions{Target: tgt(storage.BucketTarget{Kind: storage.BucketKindNative})}))
	if caps, err := withHome.BucketCaps(ctx, "w"); err != nil || caps != wrappedCaps {
		t.Fatalf("BucketCaps(w) = %+v, %v", caps, err)
	}
	caps, err = withHome.BucketCaps(ctx, "nat1")
	noErr(t, "BucketCaps(nat1)", err)
	if caps.Kind != storage.BucketKindNative || caps.Import || caps.History || caps.Compression || !caps.Clone || !caps.Versioning {
		t.Fatalf("BucketCaps(nat1) = %+v", caps)
	}
	if _, err := withHome.BucketCaps(ctx, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("BucketCaps(nope): %v", err)
	}

	// Kinds and the default kind.
	kinds, def := local.BucketKinds()
	if len(kinds) != 2 || def != storage.BucketKindNative {
		t.Fatalf("BucketKinds = %v, %v", kinds, def)
	}
	noErr(t, "SetDefaultKind(wrapped)", local.SetDefaultKind(storage.BucketKindWrapped))
	if _, def := local.BucketKinds(); def != storage.BucketKindWrapped {
		t.Fatalf("default kind %v", def)
	}
	local2 := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	if kinds, _ := local2.BucketKinds(); len(kinds) != 1 || kinds[0] != storage.BucketKindNative {
		t.Fatalf("BucketKinds without containers = %v", kinds)
	}
	if err := local2.SetDefaultKind(storage.BucketKindWrapped); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("SetDefaultKind(wrapped) without containers: %v", err)
	}
}

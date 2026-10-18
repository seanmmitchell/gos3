package s3account

import (
	"context"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// createPlan is where a new bucket goes (see planCreate).
type createPlan struct {
	kind storage.BucketKind
	// container is the container of a wrapped bucket.
	container string
	// remote, region and adopt describe a native bucket's remote bucket
	// (remote "" = the gos3 name).
	remote, region string
	adopt          bool
}

// planCreate resolves the kind and location of a bucket created with opts,
// without I/O (mu read-locked). The kind is the target's, else wrapped for
// an import target (copies need a wrapped bucket) or a target naming a
// container, native for a target naming a remote bucket, else the default
// kind. It fails with storage.ErrInvalidTarget for a target the backing
// cannot honour: an unknown kind or container, fields of the other kind,
// a wrapped bucket on a backing without containers, or an import target
// asked to be native. Before the records are loaded, containers cannot be
// checked: a wrapped bucket is then planned optimistically (CreateBucket
// checks again).
func (b *Backend) planCreate(opts storage.CreateBucketOptions) (createPlan, error) {
	var t storage.BucketTarget
	if opts.Target != nil {
		t = *opts.Target
	}
	kind := t.Kind
	switch kind {
	case "":
		switch {
		case opts.Import || t.Container != "":
			kind = storage.BucketKindWrapped
		case t.RemoteBucket != "" || t.RemoteRegion != "" || t.Adopt:
			kind = storage.BucketKindNative
		default:
			kind = b.effectiveDefaultKind()
		}
	case storage.BucketKindWrapped, storage.BucketKindNative:
	default:
		return createPlan{}, fmt.Errorf("%w: unknown bucket kind %q", storage.ErrInvalidTarget, t.Kind)
	}
	p := createPlan{kind: kind}
	if kind == storage.BucketKindNative {
		switch {
		case opts.Import:
			return createPlan{}, fmt.Errorf("%w: a native bucket (stored as-is in a remote bucket) cannot receive copied versions; choose a wrapped bucket",
				storage.ErrInvalidTarget)
		case t.Container != "":
			return createPlan{}, fmt.Errorf("%w: a native bucket has no container", storage.ErrInvalidTarget)
		}
		p.remote, p.region, p.adopt = t.RemoteBucket, t.RemoteRegion, t.Adopt
		return p, nil
	}
	if t.RemoteBucket != "" || t.RemoteRegion != "" || t.Adopt {
		return createPlan{}, fmt.Errorf("%w: a remote bucket can only be chosen for a native bucket", storage.ErrInvalidTarget)
	}
	p.container = t.Container
	if p.container == "" {
		p.container = b.defaultContainer()
	}
	switch {
	case p.container == "" && b.reg == nil:
		return createPlan{}, fmt.Errorf("%w: s3 backing %q has not loaded its records yet", storage.ErrBackingUnavailable, b.name)
	case p.container == "":
		return createPlan{}, fmt.Errorf("%w: s3 backing %q has no container for wrapped buckets (it has no home); add a container first",
			storage.ErrInvalidTarget, b.name)
	case b.reg == nil:
		// Not loaded: only the home can be checked.
	case p.container == homeID && b.home == nil, p.container != homeID && !hasKey(b.reg.containers, p.container):
		return createPlan{}, fmt.Errorf("%w: s3 backing %q has no container %q", storage.ErrInvalidTarget, b.name, p.container)
	}
	return p, nil
}

func hasKey[M ~map[K]V, K comparable, V any](m M, k K) bool {
	_, ok := m[k]
	return ok
}

// wrappedCaps are the capabilities of a wrapped bucket: everything.
var wrappedCaps = storage.BucketCaps{
	Kind: storage.BucketKindWrapped, Import: true, History: true, Clone: true,
	Versioning: true, ObjectLock: true, Compression: true,
}

// nativeCaps returns the capabilities of a native bucket whose remote was
// probed with caps (nil: not yet, assumed available).
func nativeCaps(rec bucketRecord) storage.BucketCaps {
	c := storage.BucketCaps{Kind: storage.BucketKindNative, Clone: true, Versioning: true, ObjectLock: true}
	if rec.Caps != nil {
		c.Versioning, c.ObjectLock = rec.Caps.Versioning, rec.Caps.ObjectLock
	}
	return c
}

// BucketCaps implements storage.BucketCapabilities.
func (b *Backend) BucketCaps(ctx context.Context, bucket string) (storage.BucketCaps, error) {
	if err := b.serving(ctx); err != nil {
		return storage.BucketCaps{}, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.reg.buckets[bucket]
	switch {
	case e == nil:
		return storage.BucketCaps{}, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	case e.Kind == storage.BucketKindNative:
		return nativeCaps(e.bucketRecord), nil
	}
	return wrappedCaps, nil
}

// NewBucketCaps implements storage.BucketCapabilities (no I/O; see
// planCreate). A new native bucket's remote has not been probed yet:
// versioning and object lock are reported as available.
func (b *Backend) NewBucketCaps(ctx context.Context, opts storage.CreateBucketOptions) (storage.BucketCaps, error) {
	if err := ctx.Err(); err != nil {
		return storage.BucketCaps{}, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, err := b.planCreate(opts)
	if err != nil {
		return storage.BucketCaps{}, err
	}
	if p.kind == storage.BucketKindNative {
		return nativeCaps(bucketRecord{}), nil
	}
	return wrappedCaps, nil
}

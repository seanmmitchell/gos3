package multi

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Copies of a bucket need two capabilities (see storage.BucketCaps), which
// can differ between the buckets of one backing (an s3 backing holds native
// and wrapped buckets):
//
//   - the bucket receiving a copy (a replica or a move target) must import
//     versions (Import), checked before it is created with
//     storage.NewCapsOf, without I/O (see importTarget);
//   - the primary the copies are made from must have a version history
//     that can be copied exactly (History), checked with storage.CapsOf
//     (see historySource).
//
// A native bucket has neither: it can be neither replicated nor moved, and
// copies are never stored in one.

// BucketCaps implements storage.BucketCapabilities: a bucket can do what
// its primary copy can (storage.CapsOf on the primary's backend), which
// receives every write and import first; its replicas and move target were
// checked when they were added (see checkNewMembers). Unknown buckets yield
// storage.ErrBucketNotFound.
//
// The router implements storage.BucketCapabilities rather than leave
// storage.CapsOf to its default: the router imports versions
// (storage.VersionImporter) whatever its buckets are, so the default would
// report every bucket, a native one too, as an import target.
func (r *Router) BucketCaps(ctx context.Context, bucket string) (storage.BucketCaps, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return storage.BucketCaps{}, err
	}
	caps, err := storage.CapsOf(ctx, b.Backend, bucket)
	if err != nil {
		return storage.BucketCaps{}, r.observe(b, err)
	}
	return caps, nil
}

// NewBucketCaps implements storage.BucketCapabilities: the capabilities of
// a bucket created with opts (see CreateBucket), as the backing it would be
// created on reports them (storage.NewCapsOf on opts.Backing, or on the
// default backing when it is empty; storage.ErrInvalidBacking for an
// unknown backing). No I/O. The default replicas CreateBucket adds do not
// change the result.
func (r *Router) NewBucketCaps(ctx context.Context, opts storage.CreateBucketOptions) (storage.BucketCaps, error) {
	if r.closed.Load() {
		return storage.BucketCaps{}, ErrClosed
	}
	topology := r.topology.Load()
	b := topology.def
	if opts.Backing != "" {
		if b = topology.backings[opts.Backing]; b == nil {
			return storage.BucketCaps{}, fmt.Errorf("%w: %q", storage.ErrInvalidBacking, opts.Backing)
		}
	}
	// The member backends do not know the router's backing names.
	opts.Backing = ""
	return storage.NewCapsOf(ctx, b.Backend, opts)
}

// importTarget returns nil when a bucket created on backing b with target
// (nil: the backing's choice for copies) can receive copies, i.e. be a
// replica or a move target, and else an ErrInvalidPlacement error saying
// why (wrapping storage.ErrInvalidTarget when b cannot honour target). It
// does no I/O.
func importTarget(ctx context.Context, b *backing, target *storage.BucketTarget) error {
	caps, err := storage.NewCapsOf(ctx, b.Backend, storage.CreateBucketOptions{Import: true, Target: target})
	switch {
	case err != nil:
		return fmt.Errorf("%w: backing %q cannot hold a copy of the bucket there: %w", ErrInvalidPlacement, b.Name, err)
	case !caps.Import:
		return errNoImport(b, caps.Kind)
	}
	return nil
}

// historySource returns nil when the copy of bucket on backing b keeps a
// version history that can be copied exactly, so that the bucket can be
// the primary of replicas or the source of a move, and else an
// ErrInvalidPlacement error saying why. Errors of b are returned wrapped
// (the caller observes them).
func historySource(ctx context.Context, b *backing, bucket string) error {
	caps, err := storage.CapsOf(ctx, b.Backend, bucket)
	switch {
	case err != nil:
		return fmt.Errorf("checking bucket %q on backing %q: %w", bucket, b.Name, err)
	case !caps.History:
		return errNoHistory(b, bucket, caps.Kind)
	}
	return nil
}

// errNoImport is the error for backing b, which would hold a copy in a
// bucket of kind that cannot receive copied versions.
func errNoImport(b *backing, kind storage.BucketKind) error {
	if kind == storage.BucketKindNative {
		return fmt.Errorf("%w: backing %q would hold the copy in a native bucket (stored as-is in a remote bucket), "+
			"which cannot receive copied versions; choose a wrapped bucket", ErrInvalidPlacement, b.Name)
	}
	return fmt.Errorf("%w: backing %q cannot receive copied versions, so it can hold no replicas and receive no moves",
		ErrInvalidPlacement, b.Name)
}

// errNoHistory is the error for bucket on backing b (of kind), whose
// versions cannot be copied.
func errNoHistory(b *backing, bucket string, kind storage.BucketKind) error {
	if kind == storage.BucketKindNative {
		return fmt.Errorf("%w: bucket %q on backing %q is a native bucket; its versions cannot be replicated or moved",
			ErrInvalidPlacement, bucket, b.Name)
	}
	return fmt.Errorf("%w: bucket %q on backing %q keeps no version history that can be copied; "+
		"it can be neither replicated nor moved", ErrInvalidPlacement, bucket, b.Name)
}

// checkNewBucketReplicas checks the replicas every new bucket gets
// (Options.NewBucketReplicas, resolved) against def, the default backing:
// each must receive copies (see importTarget), and the buckets def creates
// by default must have a version history to copy, which rules out a
// backing creating native buckets by default. It does no I/O.
func checkNewBucketReplicas(ctx context.Context, def *backing, replicas []*backing) error {
	if len(replicas) == 0 {
		return nil
	}
	for _, b := range replicas {
		if err := importTarget(ctx, b, nil); err != nil {
			return fmt.Errorf("new bucket replica: %w", err)
		}
	}
	caps, err := storage.NewCapsOf(ctx, def.Backend, storage.CreateBucketOptions{})
	switch {
	case err != nil:
		return fmt.Errorf("%w: default backing %q: %w", ErrInvalidPlacement, def.Name, err)
	case caps.History:
		return nil
	case caps.Kind == storage.BucketKindNative:
		return fmt.Errorf("%w: new buckets cannot have replicas: the default backing %q creates native buckets, "+
			"whose versions cannot be replicated", ErrInvalidPlacement, def.Name)
	}
	return fmt.Errorf("%w: new buckets cannot have replicas: the buckets of the default backing %q keep no version "+
		"history that can be copied", ErrInvalidPlacement, def.Name)
}

// checkNewMembers checks the backings of names that do not hold a copy of
// the bucket (state bs) yet before they receive one, in a bucket created
// with target (nil: the backing's choice for copies): the bucket's primary
// must be a history source (see historySource), and each new backing must
// receive copies there (see importTarget) and store the bucket's
// encryption (see checkEncryptionTarget). The caller holds adminMu.
func (r *Router) checkNewMembers(ctx context.Context, bucket string, bs *bucketState, names []string, target *storage.BucketTarget) error {
	r.mu.RLock()
	rec := bs.rec
	r.mu.RUnlock()
	// The caller holds adminMu, which also serializes live backing
	// configuration, so this topology stays current during the checks.
	topology := r.topology.Load()
	primary := topology.backings[rec.primary()]
	if primary == nil {
		return fmt.Errorf("%w %q of bucket %q", ErrUnknownBacking, rec.primary(), bucket)
	}
	var added []*backing
	for _, name := range names {
		if rec.member(name) >= 0 {
			continue
		}
		// Re-resolved here: the backing may have been reconfigured since
		// the caller validated the name.
		b := topology.backings[name]
		if b == nil {
			return fmt.Errorf("%w %q", ErrUnknownBacking, name)
		}
		added = append(added, b)
	}
	if len(added) == 0 {
		return nil
	}
	if err := historySource(ctx, primary, bucket); err != nil {
		if errors.Is(err, ErrInvalidPlacement) {
			return err
		}
		return r.observe(primary, err)
	}
	for _, b := range added {
		if _, err := b.Backend.GetBucket(ctx, bucket); err == nil {
			return fmt.Errorf("%w: backing %q already holds a bucket named %q; refusing to overwrite unrelated data", ErrInvalidPlacement, b.Name, bucket)
		} else if !errors.Is(err, storage.ErrBucketNotFound) {
			return r.observe(b, err)
		}
		if err := importTarget(ctx, b, target); err != nil {
			return err
		}
	}
	for _, b := range added {
		if err := r.checkEncryptionTarget(ctx, bucket, primary, b); err != nil {
			return err
		}
	}
	return nil
}

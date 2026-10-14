package multi

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The optional storage interface implemented in this file.
var _ storage.EncryptionBackend = (*Router)(nil)

// PrepareEncryption prepares mode's keys or repository for the bucket on
// its primary, which receives every write first (see
// storage.EncryptionBackend). Replicas prepare their own copy when they
// receive data.
func (r *Router) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	b, err := r.primary(bucket)
	if err != nil {
		return err
	}
	e, ok := b.Backend.(storage.EncryptionBackend)
	if !ok {
		return errors.ErrUnsupported
	}
	return e.PrepareEncryption(ctx, bucket, mode)
}

// ValidateEncryptionSettings checks cfg on the bucket's primary (see
// storage.EncryptionBackend). Restic additionally requires that every
// other backing holding or receiving a copy of the bucket can store restic
// archives: a copy it cannot store would never be replicated.
func (r *Router) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	members, err := r.placementBackings(bucket)
	if err != nil {
		return err
	}
	if cfg.Provider == storage.EncryptionRestic {
		for _, b := range members[1:] {
			if !storesRestic(b) {
				return fmt.Errorf("backing %q of bucket %q cannot store restic archives: %w", b.Name, bucket, errors.ErrUnsupported)
			}
		}
	}
	e, ok := members[0].Backend.(storage.EncryptionBackend)
	if !ok {
		return errors.ErrUnsupported
	}
	return e.ValidateEncryptionSettings(ctx, bucket, cfg)
}

// ResticBacking returns the restic settings interface of the backing
// name: errors.ErrUnsupported when it cannot store restic archives.
func (r *Router) ResticBacking(name string) (storage.ResticBackingBackend, error) {
	r.mu.RLock()
	b := r.backings[name]
	r.mu.RUnlock()
	if b == nil {
		return nil, ErrUnknownBacking
	}
	e, ok := b.Backend.(storage.ResticBackingBackend)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return e, nil
}

// storesRestic reports whether backing b can store restic archives (it has
// restic repository settings; the memory backend has none).
func storesRestic(b *backing) bool {
	_, ok := b.Backend.(storage.ResticBackingBackend)
	return ok
}

// placementBackings returns the backings holding or receiving a copy of
// bucket: the primary first, then the replicas and a move target.
func (r *Router) placementBackings(bucket string) ([]*backing, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec := bs.rec
	out := make([]*backing, 0, len(rec.Members)+1)
	for _, m := range rec.Members {
		out = append(out, r.backings[m.Backing])
	}
	if rec.Move != nil {
		out = append(out, r.backings[rec.Move.To])
	}
	return out, nil
}

// checkResticTarget refuses to place a copy of bucket on backing b when b
// cannot store restic archives but the bucket archives new writes with
// restic or holds restic versions: replication to b would never complete.
// primary is the bucket's primary. Finding restic versions walks every
// version of the bucket, which only happens for such backings.
func (r *Router) checkResticTarget(ctx context.Context, bucket string, primary, b *backing) error {
	if storesRestic(b) {
		return nil
	}
	refuse := fmt.Errorf("%w: bucket %q stores restic archives, which backing %q cannot store", ErrInvalidPlacement, bucket, b.Name)
	provider, err := storage.EncryptionProvider(ctx, primary.Backend, bucket)
	if err != nil {
		return err
	}
	if provider == storage.EncryptionRestic {
		return refuse
	}
	found := false
	err = primary.Backend.WalkVersions(ctx, bucket, "", "", "", func(oi storage.ObjectInfo) error {
		if oi.Encryption == storage.EncryptionRestic {
			found = true
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		return r.observe(primary, err)
	}
	if found {
		return refuse
	}
	return nil
}

// checkNewMembers runs checkResticTarget for each backing of names that
// does not hold a copy of the bucket (state bs) yet.
func (r *Router) checkNewMembers(ctx context.Context, bucket string, bs *bucketState, names []string) error {
	r.mu.RLock()
	rec := bs.rec
	primary := r.backings[rec.primary()]
	r.mu.RUnlock()
	for _, name := range names {
		if rec.member(name) >= 0 {
			continue
		}
		if err := r.checkResticTarget(ctx, bucket, primary, r.backings[name]); err != nil {
			return err
		}
	}
	return nil
}

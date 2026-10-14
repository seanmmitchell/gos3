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
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if _, err := r.resticBackend(name); err != nil {
		return nil, err
	}
	return routerResticBacking{r: r, name: name}, nil
}

// resticBackend returns the restic settings interface of the backing name.
// Resolve under adminMu so live reconfiguration cannot close a backing while
// its restic settings are being inspected or credentials are being validated.
func (r *Router) resticBackend(name string) (storage.ResticBackingBackend, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	b := r.topology.Load().backings[name]
	if b == nil {
		return nil, ErrUnknownBacking
	}
	e, ok := b.Backend.(storage.ResticBackingBackend)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return e, nil
}

// routerResticBacking is the storage.ResticBackingBackend of one named
// backing. Every call resolves the backing again under adminMu, so it acts
// on the backing currently configured under that name.
type routerResticBacking struct {
	r    *Router
	name string
}

// ResticBackingSettings implements storage.ResticBackingBackend.
func (b routerResticBacking) ResticBackingSettings(ctx context.Context) (*storage.ResticS3, bool, error) {
	b.r.adminMu.Lock()
	defer b.r.adminMu.Unlock()
	e, err := b.r.resticBackend(b.name)
	if err != nil {
		return nil, false, err
	}
	return e.ResticBackingSettings(ctx)
}

// ConfigureResticBacking implements storage.ResticBackingBackend.
func (b routerResticBacking) ConfigureResticBacking(ctx context.Context, cfg *storage.ResticS3) error {
	b.r.adminMu.Lock()
	defer b.r.adminMu.Unlock()
	e, err := b.r.resticBackend(b.name)
	if err != nil {
		return err
	}
	return e.ConfigureResticBacking(ctx, cfg)
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
	rec := bs.rec
	r.mu.RUnlock()
	// Live backing configuration never replaces or removes a backing that
	// holds or receives a copy, so the members resolve in any topology.
	topology := r.topology.Load()
	names := memberNames(rec.Members)
	if rec.Move != nil {
		names = append(names, rec.Move.To)
	}
	out := make([]*backing, 0, len(names))
	for _, name := range names {
		b := topology.backings[name]
		if b == nil {
			return nil, fmt.Errorf("%w %q of bucket %q", ErrUnknownBacking, name, bucket)
		}
		out = append(out, b)
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
	r.mu.RUnlock()
	// The caller holds adminMu, which also serializes live backing
	// configuration, so this topology stays current during the checks.
	topology := r.topology.Load()
	primary := topology.backings[rec.primary()]
	if primary == nil {
		return fmt.Errorf("%w %q of bucket %q", ErrUnknownBacking, rec.primary(), bucket)
	}
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
		if err := r.checkResticTarget(ctx, bucket, primary, b); err != nil {
			return err
		}
	}
	return nil
}

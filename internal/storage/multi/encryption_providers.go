package multi

import (
	"context"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// EncryptionProviders implements storage.EncryptionProvidersBackend: the
// providers usable on every backing that holds (or is receiving) a copy of
// bucket. Replicas and move targets must be able to store the bucket's
// objects with the same provider, so a memory replica or a backing without
// restic rules restic out for the whole bucket.
func (r *Router) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, err
	}
	// bs.members holds the members and the move target. Live backing
	// reconfiguration refuses to remove or replace a backing that holds a
	// bucket (including as move target), so these backings stay open.
	r.mu.RLock()
	members := make([]*backing, 0, len(bs.members))
	for _, ms := range bs.members {
		members = append(members, ms.backing)
	}
	r.mu.RUnlock()
	var lists [][]string
	for _, b := range members {
		if b == nil {
			return []string{}, nil
		}
		e, ok := b.Backend.(storage.EncryptionProvidersBackend)
		if !ok {
			return []string{}, nil
		}
		p, err := e.EncryptionProviders(ctx, bucket)
		if err != nil {
			return nil, err
		}
		lists = append(lists, p)
	}
	return storage.IntersectEncryptionProviders(lists...), nil
}

var _ storage.EncryptionProvidersBackend = (*Router)(nil)

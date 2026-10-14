package multi

import (
	"context"
	"errors"

	"github.com/seanmmitchell/gos3/internal/storage"
)

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

func (r *Router) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	b, err := r.primary(bucket)
	if err != nil {
		return err
	}
	e, ok := b.Backend.(storage.EncryptionBackend)
	if !ok {
		return errors.ErrUnsupported
	}
	return e.ValidateEncryptionSettings(ctx, bucket, cfg)
}

func (r *Router) ResticBacking(name string) (storage.ResticBackingBackend, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if _, err := r.resticBackend(name); err != nil {
		return nil, err
	}
	return routerResticBacking{r: r, name: name}, nil
}

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

type routerResticBacking struct {
	r    *Router
	name string
}

func (b routerResticBacking) ResticBackingSettings(ctx context.Context) (*storage.ResticS3, bool, error) {
	b.r.adminMu.Lock()
	defer b.r.adminMu.Unlock()
	e, err := b.r.resticBackend(b.name)
	if err != nil {
		return nil, false, err
	}
	return e.ResticBackingSettings(ctx)
}

func (b routerResticBacking) ConfigureResticBacking(ctx context.Context, cfg *storage.ResticS3) error {
	b.r.adminMu.Lock()
	defer b.r.adminMu.Unlock()
	e, err := b.r.resticBackend(b.name)
	if err != nil {
		return err
	}
	return e.ConfigureResticBacking(ctx, cfg)
}

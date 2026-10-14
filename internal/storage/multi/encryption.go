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

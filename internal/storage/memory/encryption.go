package memory

import (
	"context"
	"errors"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The optional storage interface implemented by PrepareEncryption and
// ValidateEncryptionSettings.
var _ storage.EncryptionBackend = (*Backend)(nil)

// PrepareEncryption implements storage.EncryptionBackend. The memory
// backend accepts storage.EncryptionAES256 and records it on the versions
// written with it (ObjectMeta.Encryption), so SSE responses and replication
// behave as on the fs backend, but keeps their bytes as they are: its data
// never leaves the process, where any key would have to sit next to the
// data it protects. storage.EncryptionRestic and other modes return
// errors.ErrUnsupported.
func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return err
	}
	if mode != storage.EncryptionAES256 {
		return errors.ErrUnsupported
	}
	return nil
}

// ValidateEncryptionSettings implements storage.EncryptionBackend: it
// accepts the settings PrepareEncryption accepts for their provider.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	return b.PrepareEncryption(ctx, bucket, cfg.Provider)
}
